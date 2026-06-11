import { auth } from "@/auth";
import { ensureCustomer, revokeWebhookEndpoint } from "@/lib/db";
import { UUID_RE } from "@/lib/validation";

/**
 * DELETE /api/webhooks/[id]
 * Deactivates a webhook endpoint. The endpoint is owned-checked against the
 * authenticated customer so cross-customer revocation is impossible.
 */
export async function DELETE(
  request: Request,
  { params }: { params: Promise<{ id: string }> },
): Promise<Response> {
  // CSRF check: custom headers require CORS preflight on cross-origin requests.
  const xrw = request.headers.get("X-Requested-With");
  if (!xrw || xrw.toLowerCase() !== "xmlhttprequest") {
    const safe = xrw ? xrw.replace(/[^a-zA-Z0-9_-]/g, "").slice(0, 20) : "missing";
    console.warn("CSRF check failed for DELETE /api/webhooks/[id]", { header: safe });
    return new Response("Forbidden", { status: 403 });
  }

  const session = await auth();
  if (!session?.user?.email) {
    return new Response("Unauthorized", { status: 401 });
  }

  const { id } = await params;
  if (!UUID_RE.test(id)) {
    return new Response("Invalid endpoint id", { status: 400 });
  }

  try {
    const customer = await ensureCustomer(session.user.email);
    const result = await revokeWebhookEndpoint(id, customer.id);
    if (result === "not_found") {
      return new Response("Not found", { status: 404 });
    }
    if (result === "forbidden") {
      return new Response("Forbidden", { status: 403 });
    }
    return new Response(null, { status: 204 });
  } catch (err) {
    const errorId = crypto.randomUUID();
    console.error("DELETE /api/webhooks/[id] failed:", {
      errorId,
      id,
      error: err instanceof Error ? err.message : String(err),
    });
    return new Response("Internal server error", {
      status: 500,
      headers: { "x-error-id": errorId },
    });
  }
}
