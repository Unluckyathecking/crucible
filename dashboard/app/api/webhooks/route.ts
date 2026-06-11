import { auth } from "@/auth";
import {
  ensureCustomer,
  insertWebhookEndpoint,
  listWebhookEndpoints,
} from "@/lib/db";

/** Maximum URL length we accept to prevent DB bloat from adversarial input. */
const MAX_URL_LENGTH = 2048;

function csrfCheck(request: Request): boolean {
  const xrw = request.headers.get("X-Requested-With");
  return !!xrw && xrw.toLowerCase() === "xmlhttprequest";
}

/**
 * Returns true if the hostname resolves to a private, loopback, or link-local
 * address that should not receive outbound webhook deliveries (SSRF protection).
 * Only the hostname portion of the URL is checked here; actual DNS resolution
 * happens at delivery time in the Go worker, but we reject obviously bad inputs
 * at registration to give the customer immediate feedback.
 */
function isPrivateHostname(hostname: string): boolean {
  const h = hostname.toLowerCase();

  // Loopback names
  if (h === "localhost") return true;

  // IPv6 loopback
  if (h === "::1" || h === "[::1]") return true;

  // Check for well-formed dotted-quad IPv4 first.
  const ipv4 = h.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);

  // Reject any hostname that starts with a digit but is NOT a well-formed
  // 4-part dotted-decimal IPv4. Partial notations (e.g., 1.2.3 / 1.2.3.4.5)
  // and hex/octal forms may be interpreted as IP addresses by some platforms
  // or DNS resolvers; rejecting them avoids any platform-dependent ambiguity.
  if (/^\d/.test(h) && !ipv4) {
    return true;
  }

  if (ipv4) {
    const [a, b, c, d] = [ipv4[1], ipv4[2], ipv4[3], ipv4[4]].map(Number);
    if (a > 255 || b > 255 || c > 255 || d > 255) return true; // invalid octet
    if (a === 10) return true;                         // 10.0.0.0/8
    if (a === 127) return true;                        // 127.0.0.0/8 loopback
    if (a === 169 && b === 254) return true;           // 169.254.0.0/16 link-local (includes AWS metadata)
    if (a === 172 && b >= 16 && b <= 31) return true;  // 172.16.0.0/12
    if (a === 192 && b === 168) return true;           // 192.168.0.0/16
    if (a === 0) return true;                          // 0.0.0.0/8 "this" network
    if (a === 100 && b >= 64 && b <= 127) return true; // 100.64.0.0/10 shared address space
  }

  return false;
}

/**
 * GET /api/webhooks
 * Returns the authenticated customer's active webhook endpoints (no secrets).
 */
export async function GET(request: Request): Promise<Response> {
  // CSRF check for state-mutating calls; GET is safe but we enforce it for
  // consistency with the POST handler so the frontend always sends the header.
  if (!csrfCheck(request)) {
    return new Response("Forbidden", { status: 403 });
  }
  const session = await auth();
  if (!session?.user?.email) {
    return new Response("Unauthorized", { status: 401 });
  }
  try {
    const customer = await ensureCustomer(session.user.email);
    const endpoints = await listWebhookEndpoints(customer.id);
    return new Response(JSON.stringify(endpoints), {
      headers: { "content-type": "application/json" },
    });
  } catch (err) {
    const errorId = crypto.randomUUID();
    console.error("GET /api/webhooks failed:", {
      errorId,
      error: err instanceof Error ? err.message : String(err),
    });
    return new Response("Internal server error", {
      status: 500,
      headers: { "x-error-id": errorId },
    });
  }
}

/**
 * POST /api/webhooks
 * Creates a new webhook endpoint. Returns the signing secret ONCE in the response.
 * The secret is never returned again — the client must store it immediately.
 */
export async function POST(request: Request): Promise<Response> {
  if (!csrfCheck(request)) {
    const xrw = request.headers.get("X-Requested-With");
    const safe = xrw ? xrw.replace(/[^a-zA-Z0-9_-]/g, "").slice(0, 20) : "missing";
    console.warn("CSRF check failed for POST /api/webhooks", { header: safe });
    return new Response("Forbidden", { status: 403 });
  }

  const session = await auth();
  if (!session?.user?.email) {
    return new Response("Unauthorized", { status: 401 });
  }

  let rawUrl = "";
  const contentType = request.headers.get("content-type") || "";
  if (contentType.includes("application/json")) {
    let body: unknown;
    try {
      body = await request.json();
    } catch {
      return new Response("Invalid JSON", { status: 400 });
    }
    rawUrl = typeof (body as Record<string, unknown>).url === "string"
      ? ((body as Record<string, unknown>).url as string).trim()
      : "";
  } else {
    let formData: FormData;
    try {
      formData = await request.formData();
    } catch {
      return new Response("Invalid form data", { status: 400 });
    }
    rawUrl = ((formData.get("url") as string | undefined) || "").trim();
  }

  if (!rawUrl) {
    return new Response("url is required", { status: 400 });
  }
  if (rawUrl.length > MAX_URL_LENGTH) {
    return new Response("url exceeds maximum length", { status: 400 });
  }

  // Validate: must be a well-formed HTTPS URL.
  let parsedUrl: URL;
  try {
    parsedUrl = new URL(rawUrl);
  } catch {
    return new Response("Invalid URL", { status: 400 });
  }
  if (parsedUrl.protocol !== "https:") {
    return new Response("URL must use HTTPS", { status: 400 });
  }

  // Reject embedded credentials — they would be forwarded to the customer's
  // server in the Authorization header and may leak in logs.
  if (parsedUrl.username || parsedUrl.password) {
    return new Response("URL must not contain credentials", { status: 400 });
  }

  // SSRF protection: reject private/loopback/link-local hostnames.
  // DNS rebinding attacks are handled by the Go delivery worker's HTTP client;
  // this check provides early feedback for obviously disallowed targets.
  if (isPrivateHostname(parsedUrl.hostname)) {
    return new Response("URL hostname not allowed", { status: 400 });
  }

  try {
    const customer = await ensureCustomer(session.user.email);
    const endpoint = await insertWebhookEndpoint(customer.id, parsedUrl.toString());
    // Return the secret once. cache-control: no-store prevents any caching layer
    // from retaining the secret beyond this response.
    return new Response(JSON.stringify(endpoint), {
      headers: {
        "content-type": "application/json",
        "cache-control": "no-store",
      },
    });
  } catch (err) {
    const errorId = crypto.randomUUID();
    console.error("POST /api/webhooks failed:", {
      errorId,
      error: err instanceof Error ? err.message : String(err),
    });
    return new Response("Internal server error", {
      status: 500,
      headers: { "x-error-id": errorId },
    });
  }
}
