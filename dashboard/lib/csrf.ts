// verifyCsrfToken compares a CSRF header value against the __csrf cookie using
// constant-time comparison to prevent timing side-channels.
export function verifyCsrfToken(header: string | null, cookie: string | null): boolean {
  if (!header || !cookie) return false;
  if (header.length !== cookie.length) return false;
  let result = 0;
  for (let i = 0; i < header.length; i++) {
    result |= header.charCodeAt(i) ^ cookie.charCodeAt(i);
  }
  return result === 0;
}

// getCsrfFromRequest extracts the __csrf double-submit cookie from an incoming request.
export function getCsrfFromRequest(request: Request): string | null {
  const cookieHeader = request.headers.get("cookie") ?? "";
  const match = cookieHeader.match(/(?:^|;\s*)__csrf=([^;]*)/);
  return match ? decodeURIComponent(match[1]) : null;
}
