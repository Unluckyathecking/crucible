import { auth } from "@/auth";
import { ensureCustomer, revokeApiKey } from "@/lib/db";

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export async function DELETE(
  request: Request,
  { params }: { params: Promise<{ id: string }> },
): Promise<Response> {
  // Declared outside try so the catch block can include them in log context.
  let keyId: string | undefined;
  let customerId: string | undefined;
  try {
    // Reject cross-origin form submissions: browsers cannot set X-Requested-With
    // on cross-origin requests without a preflight (which the server doesn't allow).
    if (request.headers.get("X-Requested-With") !== "XMLHttpRequest") {
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
    if (result === "not_found") {
      return new Response("Not found", { status: 404 });
    }
    if (result === "forbidden") {
      return new Response("Forbidden", { status: 403 });
    }

    // Both "revoked" and "already_revoked" are success — idempotent.
    return new Response(null, { status: 200 });
  } catch (err) {
    const errorId = crypto.randomUUID();
    console.error("DELETE /api/keys/[id] failed:", {
      errorId,
      keyId,
      customerId,
      error: err instanceof Error ? err.message : String(err),
    });
    return new Response("Internal server error", { status: 500 });
  }
}
