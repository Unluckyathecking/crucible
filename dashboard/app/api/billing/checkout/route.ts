import { auth } from "@/auth";
import { ensureCustomer } from "@/lib/db";
import { ALLOWED_ORIGIN, DASHBOARD_BASE_URL } from "@/lib/env";
import { getCsrfFromRequest, verifyCsrfToken } from "@/lib/csrf";

const STRIPE_API_BASE = "https://api.stripe.com/v1";

export async function POST(request: Request): Promise<Response> {
  try {
    // CSRF double-submit cookie: the middleware sets __csrf on every dashboard page load;
    // the client reads it from document.cookie and echoes it as X-CSRF-Token.
    // Constant-time compare prevents timing side-channels.
    // An attacker on a different origin cannot read the SameSite=Strict cookie,
    // so they cannot forge the matching header.
    const csrfCookie = getCsrfFromRequest(request);
    const csrfHeader = request.headers.get("X-CSRF-Token");
    if (!verifyCsrfToken(csrfHeader, csrfCookie)) {
      console.warn("CSRF token mismatch for POST /api/billing/checkout");
      return new Response("Forbidden", { status: 403 });
    }

    // Retain Origin check as defense-in-depth: a present-but-wrong Origin is rejected.
    // Safari omits Origin on same-origin POSTs; the CSRF cookie above is the primary guard.
    const origin = request.headers.get("Origin");
    if (origin !== null && origin !== ALLOWED_ORIGIN) {
      const safeOrigin = JSON.stringify(origin.slice(0, 60));
      console.warn("CSRF: invalid Origin for POST /api/billing/checkout", { origin: safeOrigin, expected: ALLOWED_ORIGIN });
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

    const dashboardOrigin = DASHBOARD_BASE_URL;

    let planId = "";
    try {
      const body = (await request.json()) as Record<string, unknown>;
      planId = typeof body.plan_id === "string" ? body.plan_id.trim() : "";
    } catch {
      return new Response("Invalid JSON", { status: 400 });
    }
    if (!planId || !PLAN_ID_RE.test(planId)) {
      return new Response("plan_id required: lowercase alphanumeric and hyphens, max 32 chars", { status: 400 });
    }

    const customer = await ensureCustomer(session.user.email);

    // Look up the Stripe price for the requested plan from the environment.
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

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 15_000);
    let stripeResp: Response;
    try {
      stripeResp = await fetch(`${STRIPE_API_BASE}/checkout/sessions`, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${stripeSecretKey}`,
          "Content-Type": "application/x-www-form-urlencoded",
        },
        body: form.toString(),
        signal: controller.signal,
      });
    } catch (fetchErr) {
      if (fetchErr instanceof Error && fetchErr.name === "AbortError") {
        console.error("POST /api/billing/checkout: Stripe request timed out");
        return new Response(JSON.stringify({ error: { code: "STRIPE_TIMEOUT", message: "billing unavailable" } }), {
          status: 504,
          headers: { "content-type": "application/json" },
        });
      }
      throw fetchErr;
    } finally {
      clearTimeout(timer);
    }

    type StripeSessionResp = { url?: string; error?: { message?: string } };
    let stripeBody: StripeSessionResp;
    try {
      stripeBody = (await stripeResp.json()) as StripeSessionResp;
    } catch {
      console.error("POST /api/billing/checkout: non-JSON response from Stripe", { status: stripeResp.status });
      return new Response(JSON.stringify({ error: { code: "STRIPE_ERROR", message: "billing unavailable" } }), {
        status: 502,
        headers: { "content-type": "application/json" },
      });
    }

    if (!stripeResp.ok || !stripeBody.url) {
      console.error("POST /api/billing/checkout: stripe error", { status: stripeResp.status });
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
// This prevents env-var probing via the STRIPE_PRICE_<X> mapping.
const PLAN_ID_RE = /^[a-z0-9-]{1,32}$/;

// STRIPE_PRICE_ID_RE validates that the resolved value is a real Stripe price ID.
// Stripe price IDs are alphanumeric after the prefix — no underscores in the suffix.
const STRIPE_PRICE_ID_RE = /^price_[a-zA-Z0-9]+$/;

// resolveStripePriceId looks up STRIPE_PRICE_<PLAN_ID_UPPER> and validates it.
async function resolveStripePriceId(planId: string): Promise<string | null> {
  if (!PLAN_ID_RE.test(planId)) {
    return null;
  }
  const envKey = `STRIPE_PRICE_${planId.toUpperCase().replace(/-/g, "_")}`;
  const priceId = process.env[envKey];
  if (!priceId || !STRIPE_PRICE_ID_RE.test(priceId)) {
    return null;
  }
  return priceId;
}
