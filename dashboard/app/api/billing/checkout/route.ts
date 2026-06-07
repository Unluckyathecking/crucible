import { auth } from "@/auth";
import { ensureCustomer } from "@/lib/db";

const STRIPE_API_BASE = "https://api.stripe.com/v1";

export async function POST(request: Request): Promise<Response> {
  try {
    // Lightweight CSRF signal: custom headers require CORS preflight on cross-origin requests.
    const xrw = request.headers.get("X-Requested-With");
    if (!xrw || xrw.toLowerCase() !== "xmlhttprequest") {
      const safeHeader = xrw ? xrw.replace(/[^a-zA-Z0-9_-]/g, "").slice(0, 20) : "missing";
      console.warn("CSRF check failed for POST /api/billing/checkout", { header: safeHeader });
      return new Response("Forbidden", { status: 403 });
    }

    const session = await auth();
    if (!session?.user?.email) {
      return new Response("Unauthorized", { status: 401 });
    }

    const stripeSecretKey = process.env.STRIPE_SECRET_KEY;
    if (!stripeSecretKey) {
      console.error("POST /api/billing/checkout: STRIPE_SECRET_KEY not configured");
      return new Response("Internal server error", { status: 500 });
    }

    const dashboardOrigin = process.env.NEXTAUTH_URL ?? process.env.DASHBOARD_ORIGIN ?? "http://localhost:3001";

    let planId = "";
    try {
      const body = (await request.json()) as Record<string, unknown>;
      planId = typeof body.plan_id === "string" ? body.plan_id.trim() : "";
    } catch {
      return new Response("Invalid JSON", { status: 400 });
    }
    if (!planId) {
      return new Response("plan_id required", { status: 400 });
    }

    const customer = await ensureCustomer(session.user.email);

    // Look up the Stripe price for the requested plan from the gateway's plans table.
    // The plan must have a stripe_price_id to be upgradeable.
    const stripePriceId = await resolveStripePriceId(planId);
    if (!stripePriceId) {
      return new Response(JSON.stringify({ error: { code: "PLAN_NOT_FOUND", message: "plan not found or not upgradeable" } }), {
        status: 422,
        headers: { "content-type": "application/json" },
      });
    }

    const form = new URLSearchParams();
    form.set("mode", "subscription");
    form.set("client_reference_id", customer.id);
    form.set("line_items[0][price]", stripePriceId);
    form.set("line_items[0][quantity]", "1");
    form.set("customer_creation", "always");
    form.set("subscription_data[metadata][crucible_customer_id]", customer.id);
    form.set("success_url", `${dashboardOrigin}/dashboard/billing?success=1`);
    form.set("cancel_url", `${dashboardOrigin}/dashboard/billing?canceled=1`);

    const stripeResp = await fetch(`${STRIPE_API_BASE}/checkout/sessions`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${stripeSecretKey}`,
        "Content-Type": "application/x-www-form-urlencoded",
      },
      body: form.toString(),
    });

    type StripeSessionResp = { url?: string; error?: { message?: string } };
    const stripeBody = (await stripeResp.json()) as StripeSessionResp;

    if (!stripeResp.ok || !stripeBody.url) {
      const msg = stripeBody.error?.message ?? `stripe status ${stripeResp.status}`;
      console.error("POST /api/billing/checkout: stripe error", { status: stripeResp.status, msg });
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
    console.error("POST /api/billing/checkout failed:", {
      errorId,
      error: err instanceof Error ? err.message : String(err),
    });
    return new Response("Internal server error", { status: 500, headers: { "x-error-id": errorId } });
  }
}

// PLAN_ID_RE restricts plan IDs to lowercase alphanumeric + hyphens, max 32 chars.
// This prevents env-var probing: an attacker can't construct a STRIPE_PRICE_<X> key
// that maps to an arbitrary variable like PATH or HOME.
const PLAN_ID_RE = /^[a-z0-9-]{1,32}$/;

// resolveStripePriceId fetches the stripe_price_id for a plan from the environment
// or a static mapping. In production this should query the gateway's plans table via a
// shared DB connection or a gateway API call. For the dashboard tier, we use a simple
// env-var mapping: STRIPE_PRICE_<PLAN_ID_UPPER>=price_xxx.
async function resolveStripePriceId(planId: string): Promise<string | null> {
  if (!PLAN_ID_RE.test(planId)) {
    return null;
  }
  const envKey = `STRIPE_PRICE_${planId.toUpperCase().replace(/-/g, "_")}`;
  const priceId = process.env[envKey];
  return priceId ?? null;
}
