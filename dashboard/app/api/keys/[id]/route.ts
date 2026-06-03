import { auth } from "@/auth";
import { ensureCustomer, revokeApiKey } from "@/lib/db";
import { UUID_RE } from "@/lib/validation";

export async function DELETE(
  request: Request,
  { params }: { params: Promise<{ id: string }> },
): Promise<Response> {
  // Declared outside try so the catch block can include them in log context.
  let keyId: string | undefined;
  let customerId: string | undefined;
  try {
    // Lightweight CSRF signal: custom headers require CORS preflight on cross-origin
    // requests, making it harder for cross-origin pages to trigger this endpoint.
    // Primary defense is the session cookie's SameSite attribute; this is defense-in-depth.
    // Convention: clients send "XMLHttpRequest"; compare case-insensitively so
    // proxies that normalize header values do not break the guard.
    if (request.headers.get("X-Requested-With")?.toLowerCase() !== "xmlhttprequest") {
      return new Response("Forbidden", { status: 403 });
    }

    const session = await auth();
    if (!session?.user?.email) {
      return new Response("Unauthorized", { status: 401 });
    }

    keyId = (await params).id;
    // Reject non-UUID path segments before hitting Postgres — otherwise pgx
    // throws "invalid input syntax for type uuid" which the catch turns into 500.
    if (!UUID_RE.test(keyId)) {
      return new Response("Not found", { status: 404 });
    }
    const customer = await ensureCustomer(session.user.email);
    customerId = customer.id;

    const result = await revokeApiKey(keyId, customer.id);
    switch (result) {
      case "not_found":
        return new Response("Not found", { status: 404 });
      case "forbidden":
        return new Response("Forbidden", { status: 403 });
      case "revoked":
      case "already_revoked":
        // Both are success — idempotent.
        return new Response(null, { status: 200, headers: { "cache-control": "no-store" } });
      default: {
        // Compile-time exhaustiveness: TypeScript flags this if a new RevokeResult
        // variant is added without updating this switch.
        const _exhaustive: never = result;
        throw new Error(`Unexpected revokeApiKey result: ${_exhaustive}`);
      }
    }
  } catch (err) {
    const errorId = crypto.randomUUID();
    console.error("DELETE /api/keys/[id] failed:", {
      errorId,
      keyId,
      customerId,
      error: err instanceof Error ? err.message : String(err),
    });
    return new Response("Internal server error", { status: 500, headers: { "x-error-id": errorId } });
  }
}
