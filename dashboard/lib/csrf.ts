// MAX_CSRF_TOKEN_LENGTH is the fixed loop bound for constant-time comparison.
// Must be >= the actual token length (32 hex chars from UUID-without-dashes).
const MAX_CSRF_TOKEN_LENGTH = 128;

// verifyCsrfToken compares CSRF tokens using a constant-time loop of fixed
// length to prevent timing side-channels on both token content and length.
// Empty or null inputs are rejected immediately — two absent tokens must not
// verify as equal (that would defeat CSRF protection when cookies are absent).
export function verifyCsrfToken(header: string | null, cookie: string | null): boolean {
  const h = header ?? "";
  const c = cookie ?? "";
  if (h.length === 0 || c.length === 0) {
    return false;
  }
  const maxLen = MAX_CSRF_TOKEN_LENGTH;
  let result = h.length ^ c.length;
  for (let i = 0; i < maxLen; i++) {
    const hv = i < h.length ? h.charCodeAt(i) : 0;
    const cv = i < c.length ? c.charCodeAt(i) : 0;
    result |= hv ^ cv;
  }
  return result === 0;
}

// getCsrfFromRequest extracts the __csrf double-submit cookie from an incoming request.
export function getCsrfFromRequest(request: Request): string | null {
  const cookieHeader = request.headers.get("cookie") ?? "";
  const match = cookieHeader.match(/(?:^|;\s*)__csrf=([^;]*)/);
  return match ? decodeURIComponent(match[1]) : null;
}
