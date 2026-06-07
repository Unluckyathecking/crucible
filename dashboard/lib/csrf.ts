// verifyCsrfToken compares a CSRF header value against the __csrf cookie using
// constant-time comparison to prevent timing side-channels. We XOR the lengths
// into `result` and loop to max(header, cookie) length — early-exit on length
// mismatch would leak timing information about which byte first differs.
export function verifyCsrfToken(header: string | null, cookie: string | null): boolean {
  if (!header || !cookie) return false;
  const maxLen = Math.max(header.length, cookie.length);
  let result = header.length ^ cookie.length;
  for (let i = 0; i < maxLen; i++) {
    const h = i < header.length ? header.charCodeAt(i) : 0;
    const c = i < cookie.length ? cookie.charCodeAt(i) : 0;
    result |= h ^ c;
  }
  return result === 0;
}

// getCsrfFromRequest extracts the __csrf double-submit cookie from an incoming request.
export function getCsrfFromRequest(request: Request): string | null {
  const cookieHeader = request.headers.get("cookie") ?? "";
  const match = cookieHeader.match(/(?:^|;\s*)__csrf=([^;]*)/);
  return match ? decodeURIComponent(match[1]) : null;
}
