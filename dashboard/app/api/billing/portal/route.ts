import { auth } from "@/auth";
import { ensureCustomer, getStripeCustomerId } from "@/lib/db";
import { ALLOWED_ORIGIN } from "@/lib/env";
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
      console.warn("CSRF token mismatch for POST /api/billing/portal");
      return new Response("Forbidden", { status: 403 });
    }

    // Retain Origin check as defense-in-depth: a present-but-wrong Origin is rejected.
    // Safari omits Origin on same-origin POSTs; the CSRF cookie above is the primary guard.
    const origin = request.headers.get("Origin");
    if (origin !== null && origin !== ALLOWED_ORIGIN) {
      const safeOrigin = origin.replace(/[^a-zA-Z0-9/:._-]/g, "").slice(0, 60);
      console.warn("CSRF: invalid Origin for POST /api/billing/portal", { origin: safeOrigin, expected: ALLOWED_ORIGIN });
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
      console.error("POST /api/billing/portal: stripe error", { status: stripeResp.status });
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
