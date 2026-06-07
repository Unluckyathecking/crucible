// ALLOWED_ORIGIN is the scheme+host+port the dashboard runs on, parsed once at
// module load. Used by API route handlers for CSRF origin verification.
export const ALLOWED_ORIGIN = (() => {
  const raw = process.env.NEXTAUTH_URL ?? process.env.DASHBOARD_ORIGIN ?? "http://localhost:3001";
  try {
    return new URL(raw).origin;
  } catch {
    return raw;
  }
})();
