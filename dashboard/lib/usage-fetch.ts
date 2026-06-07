// Client-side fetch helper for /api/usage.
// Extracted so it can be unit-tested without the full React component tree.

import type { RawEvent } from "./usage-format";
import { sanitizeError } from "./usage-format";

export async function fetchUsage(
  from: string,
  to: string,
  operation?: string,
  signal?: AbortSignal,
): Promise<{ data: RawEvent[] } | { error: string } | null> {
  const params = new URLSearchParams({ from, to });
  if (operation !== undefined) params.set("operation", operation);
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
    console.error("fetchUsage failed:", err);
    return { error: "Network error — please check your connection." };
  }
  if (res.status === 403) {
    // The header should always be sent; 403 means something stripped it (proxy, extension).
    return { error: "Forbidden (403) — the X-Requested-With header was stripped by your environment." };
  }
  if (res.status === 401) {
    return { error: "Session expired — please reload the page." };
  }
  if (!res.ok) {
    const body: unknown = await res.json().catch(() => ({}));
    if (typeof body !== "object" || body === null) {
      return { error: `Server error (${res.status})` };
    }
    const err = (body as Record<string, unknown>).error;
    return { error: typeof err === "string" ? sanitizeError(err) : `Server error (${res.status})` };
  }
  let json: unknown;
  try {
    json = await res.json();
  } catch {
    return { error: "Invalid response from server" };
  }
  if (!Array.isArray(json)) {
    return { error: "Unexpected response format from server" };
  }
  if (!json.every((item): item is RawEvent => {
    if (item === null || typeof item !== "object") return false;
    const r = item as Record<string, unknown>;
    return (
      typeof r.operation === "string" &&
      typeof r.billable_units === "number" &&
      Number.isFinite(r.billable_units) &&
      typeof r.created_at === "string"
    );
  })) {
    return { error: "Unexpected response format from server" };
  }
  return { data: json };
}
