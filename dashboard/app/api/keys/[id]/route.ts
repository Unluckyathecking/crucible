import { auth } from "@/auth";
import { ensureCustomer, revokeApiKey } from "@/lib/db";

export async function DELETE(
  _request: Request,
  { params }: { params: Promise<{ id: string }> },
): Promise<Response> {
  const session = await auth();
  if (!session?.user?.email) {
    return new Response("Unauthorized", { status: 401 });
  }

  const { id } = await params;
  const customer = await ensureCustomer(session.user.email);

  const result = await revokeApiKey(id, customer.id);
  if (result === "not_found") {
    return new Response("Not found", { status: 404 });
  }

  // Both "revoked" and "already_revoked" are success — idempotent.
  return new Response(null, { status: 200 });
}
