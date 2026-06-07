// Client-side fetch helper for /api/usage.
// Extracted so it can be unit-tested without the full React component tree.

import type { RawEvent } from "./usage-format";

// Strips characters that are meaningful in HTML contexts from server error strings.
// This is a data-layer contract: no downstream renderer should receive raw angle brackets
// from server errors, regardless of how it renders them.
// Strip angle brackets only: React auto-escapes text content, so &"' are not
// dangerous here. < and > are stripped as defence-in-depth against non-React renderers.
function sanitizeError(s: string): string {
  return s.replace(/[<>]/g, "").slice(0, 200);
}

function isRawEvent(item: unknown): item is RawEvent {
  if (item === null || typeof item !== "object") return false;
  const r = item as Record<string, unknown>;
  return (
    typeof r.operation === "string" &&
    typeof r.billable_units === "number" &&
    Number.isFinite(r.billable_units) &&
    typeof r.created_at === "string"
  );
}

export async function fetchUsage(
  from: string,
  to: string,
  operation?: string,
  signal?: AbortSignal,
): Promise<{ data: RawEvent[] } | { error: string } | null> {
  const params = new URLSearchParams({ from, to });
  // Skip empty string: server rejects operation= with 400 "must not be empty".
  if (operation !== undefined && operation !== "") params.set("operation", operation);
  let res: Response;
  try {
    res = await fetch(`/api/usage?${params}`, {
      headers: {
        "X-Requested-With": "XMLHttpRequest",
        "Accept": "application/json",
      },
      cache: "no-store",
      signal,
    });
  } catch (err) {
    if (err instanceof Error && err.name === "AbortError") return null;
    return { error: "Network error — please check your connection." };
  }
  if (res.status === 401) {
    return { error: "Session expired — please reload the page." };
  }
  if (res.status === 403) {
    return { error: "Forbidden — request rejected (403)." };
  }
  if (!res.ok) {
    const body: unknown = await res.json().catch(() => ({}));
    if (typeof body !== "object" || body === null || Array.isArray(body)) {
      return { error: `Server error (${res.status})` };
    }
    const err = (body as Record<string, unknown>).error;
    return { error: typeof err === "string" ? sanitizeError(err) : `Server error (${res.status})` };
  }
  let json: unknown;
  try {
    json = await res.json();
  } catch {
    return { error: "Unexpected response format from server" };
  }
  if (!Array.isArray(json)) {
    return { error: "Unexpected response format from server" };
  }
  if (!json.every(isRawEvent)) {
    return { error: "Unexpected response format from server" };
  }
  return { data: json };
}
