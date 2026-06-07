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
