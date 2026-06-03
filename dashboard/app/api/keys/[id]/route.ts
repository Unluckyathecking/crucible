import { auth } from "@/auth";
import { ensureCustomer, revokeApiKey } from "@/lib/db";

export async function DELETE(
  _request: Request,
  { params }: { params: Promise<{ id: string }> },
): Promise<Response> {
  try {
    const session = await auth();
    if (!session?.user?.email) {
      return new Response("Unauthorized", { status: 401 });
    }

    const { id } = await params;
    // Reject non-UUID path segments before hitting Postgres — otherwise pgx
    // throws "invalid input syntax for type uuid" which the catch turns into 500.
    if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(id)) {
      return new Response("Not found", { status: 404 });
    }
    const customer = await ensureCustomer(session.user.email);

    const result = await revokeApiKey(id, customer.id);
    if (result === "not_found") {
      return new Response("Not found", { status: 404 });
    }
    if (result === "forbidden") {
      return new Response("Forbidden", { status: 403 });
    }

    // Both "revoked" and "already_revoked" are success — idempotent.
    return new Response(null, { status: 200 });
  } catch (err) {
    console.error("DELETE /api/keys/[id] failed:", err instanceof Error ? err.message : String(err));
    return new Response("Internal server error", { status: 500 });
  }
}
