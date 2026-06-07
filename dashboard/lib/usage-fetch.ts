// Client-side fetch helper for /api/usage.
// Extracted so it can be unit-tested without the full React component tree.

import type { RawEvent } from "./usage-format";

// Strips angle brackets from server error strings and caps their length.
// Scope: these strings are passed as React text-node children (JSX), which automatically
// HTML-encodes every character including &, ", and '. Manually encoding & here would
// cause double-encoding (& → &amp; in sanitizeError → &amp;amp; in React output),
// corrupting readable messages like "query & limit exceeded". Only < and > need explicit
// stripping because they are the characters that break out of HTML text context; the
// stripping is defence-in-depth for any future non-React use of this helper.
const MAX_ERROR_LENGTH = 200;

export function sanitizeError(s: string): string {
  return s.replace(/[<>]/g, "").slice(0, MAX_ERROR_LENGTH);
}

function isRawEvent(item: unknown): item is RawEvent {
  if (item === null || typeof item !== "object") return false;
  const r = item as Record<string, unknown>;
  if (
    typeof r.id !== "string" ||
    typeof r.operation !== "string" ||
    typeof r.created_at !== "string" ||
    typeof r.billable_units !== "number" ||
    !Number.isFinite(r.billable_units) ||
    !Number.isInteger(r.billable_units)
  ) return false;
  return true;
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
  if (!res.ok) {
    const ct = res.headers.get("content-type") ?? "";
    if (!ct.includes("application/json")) {
      return { error: `Server error (${res.status})` };
    }
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
