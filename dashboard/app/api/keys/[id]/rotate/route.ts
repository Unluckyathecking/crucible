import { auth } from "@/auth";
import { ensureCustomer, rotateApiKey } from "@/lib/db";
import { UUID_RE } from "@/lib/validation";

const MIN_GRACE_SECS = 0;
const MAX_GRACE_SECS = 7 * 24 * 3600; // 7 days — mirrors maxGrace in gateway/internal/auth/store.go
const DEFAULT_GRACE_SECS = 3600;       // 1 hour

export async function POST(
  request: Request,
  { params }: { params: Promise<{ id: string }> },
): Promise<Response> {
  let keyId: string | undefined;
  let customerId: string | undefined;
  try {
    // Lightweight CSRF signal: custom headers require CORS preflight on cross-origin
    // requests. Defense-in-depth alongside SameSite session cookies.
    const xrw = request.headers.get("X-Requested-With");
    if (!xrw || xrw.toLowerCase() !== "xmlhttprequest") {
      const safeHeader = xrw ? xrw.replace(/[^a-zA-Z0-9_-]/g, "").slice(0, 20) : "missing";
      console.warn("CSRF check failed for POST /api/keys/[id]/rotate", { header: safeHeader });
      return new Response("Forbidden", { status: 403 });
    }

    const session = await auth();
    if (!session?.user?.email) {
      return new Response("Unauthorized", { status: 401 });
    }

    keyId = (await params).id;
    if (!UUID_RE.test(keyId)) {
      return new Response("Not found", { status: 404 });
    }

    // Parse optional grace_secs from JSON body; server-clamp to [MIN, MAX].
    let graceSecs = DEFAULT_GRACE_SECS;
    const contentType = request.headers.get("content-type") ?? "";
    if (contentType.includes("application/json")) {
      let body: unknown;
      try {
        body = await request.json();
      } catch {
        return new Response("Invalid JSON", { status: 400 });
      }
      const raw = (body as Record<string, unknown>).grace_secs;
      if (typeof raw === "number" && Number.isFinite(raw)) {
        graceSecs = Math.max(MIN_GRACE_SECS, Math.min(Math.floor(raw), MAX_GRACE_SECS));
      }
    }

    const customer = await ensureCustomer(session.user.email);
    customerId = customer.id;

    const result = await rotateApiKey(keyId, customer.id, graceSecs);

    if (!result.ok) {
      switch (result.reason) {
        case "not_found":
          return new Response("Not found", { status: 404 });
        case "forbidden":
          return new Response("Forbidden", { status: 403 });
        case "already_expired":
          return new Response("Key already expired", { status: 409 });
        default: {
          const _exhaustive: never = result.reason;
          const errorId = crypto.randomUUID();
          console.error("Unexpected rotateApiKey reason:", { errorId, reason: _exhaustive });
          return new Response("Internal server error", { status: 500, headers: { "x-error-id": errorId } });
        }
      }
    }

    // Return the new full key exactly once — the caller must display and copy it now.
    return new Response(JSON.stringify({ key: result.newKey }), {
      headers: { "content-type": "application/json", "cache-control": "no-store" },
    });
  } catch (err) {
    const errorId = crypto.randomUUID();
    console.error("POST /api/keys/[id]/rotate failed:", {
      errorId,
      keyId,
      customerId,
      error: err instanceof Error ? err.message : String(err),
    });
    return new Response("Internal server error", { status: 500, headers: { "x-error-id": errorId } });
  }
}
