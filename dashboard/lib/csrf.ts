// verifyCsrfToken compares CSRF tokens in constant time. We avoid an early
// return on null/empty inputs (which would leak via timing) by coercing to
// empty strings and looping to at least 32 iterations regardless of input
// length. Lengths are XOR'd into result so unequal-length tokens fail.
export function verifyCsrfToken(header: string | null, cookie: string | null): boolean {
  const h = header ?? "";
  const c = cookie ?? "";
  const maxLen = Math.max(32, h.length, c.length);
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
