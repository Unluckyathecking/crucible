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
