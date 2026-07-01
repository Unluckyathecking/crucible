// ALLOWED_ORIGIN is the scheme+host+port the dashboard runs on, parsed once at
// module load. Used by API route handlers for CSRF origin verification.
export const ALLOWED_ORIGIN = (() => {
  const raw = process.env.NEXTAUTH_URL ?? process.env.DASHBOARD_ORIGIN ?? "http://localhost:3001";
  try {
    return new URL(raw).origin;
  } catch {
    // raw may lack a scheme (e.g. "localhost:3001"); prepend http:// so origin
    // comparisons and the secure-cookie check don't silently break.
    const withScheme = raw.includes("://") ? raw : `http://${raw}`;
    try { return new URL(withScheme).origin; } catch { return "http://localhost:3001"; }
  }
})();

// DASHBOARD_BASE_URL is the full base URL (with path) used to construct Stripe
// success/cancel/return redirect URLs. Uses NEXTAUTH_URL preferred over DASHBOARD_ORIGIN.
export const DASHBOARD_BASE_URL = process.env.NEXTAUTH_URL ?? process.env.DASHBOARD_ORIGIN ?? "http://localhost:3001";

// getGatewayUrl returns the base URL of the Crucible gateway the operator console
// reads /v1/admin/* from. A function (not a module-level const) so it reflects
// the current process.env.GATEWAY_URL at call time. Never used for
// customer-facing requests.
export function getGatewayUrl(): string {
  return process.env.GATEWAY_URL ?? "http://localhost:8080";
}

// requireOperatorToken returns the static operator bearer token the gateway's
// /v1/admin/* middleware expects. Server-only: call exclusively from route
// handlers, server components, and server actions — never pass the return value
// into a client component prop or a JSON response body.
export function requireOperatorToken(): string {
  const token = process.env.OPERATOR_TOKEN;
  if (!token || token.length < 32) {
    throw new Error("OPERATOR_TOKEN not configured or too short (need >= 32 bytes, matching the gateway's requirement)");
  }
  return token;
}
