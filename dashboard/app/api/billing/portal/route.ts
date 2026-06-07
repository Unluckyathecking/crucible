import { auth } from "@/auth";
import { ensureCustomer, getStripeCustomerId } from "@/lib/db";

const STRIPE_API_BASE = "https://api.stripe.com/v1";

export async function POST(request: Request): Promise<Response> {
  try {
    // Lightweight CSRF signal: custom headers require CORS preflight on cross-origin requests.
    const xrw = request.headers.get("X-Requested-With");
    if (!xrw || xrw.toLowerCase() !== "xmlhttprequest") {
      const safeHeader = xrw ? xrw.replace(/[^a-zA-Z0-9_-]/g, "").slice(0, 20) : "missing";
      console.warn("CSRF check failed for POST /api/billing/portal", { header: safeHeader });
      return new Response("Forbidden", { status: 403 });
    }

    const session = await auth();
    if (!session?.user?.email) {
      return new Response("Unauthorized", { status: 401 });
    }

    const stripeSecretKey = process.env.STRIPE_SECRET_KEY;
    if (!stripeSecretKey) {
      console.error("POST /api/billing/portal: STRIPE_SECRET_KEY not configured");
      return new Response("Internal server error", { status: 500 });
    }

    const dashboardOrigin = process.env.NEXTAUTH_URL ?? process.env.DASHBOARD_ORIGIN ?? "http://localhost:3001";

    const customer = await ensureCustomer(session.user.email);
    const stripeCustomerId = await getStripeCustomerId(customer.id);

    if (!stripeCustomerId) {
      return new Response(
        JSON.stringify({ error: { code: "NO_STRIPE_CUSTOMER", message: "complete checkout first" } }),
        { status: 402, headers: { "content-type": "application/json" } },
      );
    }

    const form = new URLSearchParams();
    form.set("customer", stripeCustomerId);
    form.set("return_url", `${dashboardOrigin}/dashboard/billing`);

    const stripeResp = await fetch(`${STRIPE_API_BASE}/billing/portal/sessions`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${stripeSecretKey}`,
        "Content-Type": "application/x-www-form-urlencoded",
      },
      body: form.toString(),
    });

    type StripePortalResp = { url?: string; error?: { message?: string } };
    const stripeBody = (await stripeResp.json()) as StripePortalResp;

    if (!stripeResp.ok || !stripeBody.url) {
      const msg = stripeBody.error?.message ?? `stripe status ${stripeResp.status}`;
      console.error("POST /api/billing/portal: stripe error", { status: stripeResp.status, msg });
      return new Response(JSON.stringify({ error: { code: "STRIPE_ERROR", message: "billing unavailable" } }), {
        status: 502,
        headers: { "content-type": "application/json" },
      });
    }

    return new Response(JSON.stringify({ url: stripeBody.url }), {
      headers: { "content-type": "application/json", "cache-control": "no-store" },
    });
  } catch (err) {
    const errorId = crypto.randomUUID();
    console.error("POST /api/billing/portal failed:", {
      errorId,
      error: err instanceof Error ? err.message : String(err),
    });
    return new Response("Internal server error", { status: 500, headers: { "x-error-id": errorId } });
  }
}
