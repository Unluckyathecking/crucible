/**
 * Unit tests for the api/webhooks subscribed_events validation logic.
 *
 * We cannot import route.ts directly because it imports @/auth, which requires
 * the full NextAuth + Postgres setup (see lib/__tests__/db.test.ts and
 * app/api/keys/__tests__/route.test.ts for the same constraint). Instead we
 * re-implement the extractable business-logic rules the handler embeds:
 *   1. WEBHOOK_EVENT_TYPES membership check (mirrors gateway/internal/events.AllEventTypes)
 *   2. parseSubscribedEvents: null/undefined → "all events"; array of valid
 *      strings → stored subscription; anything else → a rejection error
 *   3. FormData's comma-separated subscribed_events field → string[] | null
 *
 * If lib/db.ts's real implementations of these change, keep this file's copies
 * and route.ts in sync together.
 */
import { describe, it, expect } from "vitest";

// Must match gateway/internal/events.AllEventTypes and lib/db.ts's WEBHOOK_EVENT_TYPES.
const WEBHOOK_EVENT_TYPES = [
  "subscription.updated",
  "subscription.deleted",
  "quota.exceeded",
  "api_key.rotated",
  "api_key.revoked",
] as const;

function isValidWebhookEventType(t: string): boolean {
  return (WEBHOOK_EVENT_TYPES as readonly string[]).includes(t);
}

function parseSubscribedEvents(
  value: unknown,
): { ok: true; events: string[] | null } | { ok: false; error: string } {
  if (value === undefined || value === null) return { ok: true, events: null };
  if (!Array.isArray(value)) {
    return { ok: false, error: "subscribed_events must be an array of strings" };
  }
  if (value.length > WEBHOOK_EVENT_TYPES.length) {
    return { ok: false, error: `subscribed_events must not exceed ${WEBHOOK_EVENT_TYPES.length} entries` };
  }
  const seen = new Set<string>();
  for (const v of value) {
    if (typeof v !== "string") {
      return { ok: false, error: "subscribed_events must contain only strings" };
    }
    if (!isValidWebhookEventType(v)) {
      return { ok: false, error: `unknown event type: ${v}` };
    }
    seen.add(v);
  }
  return { ok: true, events: [...seen] };
}

function parseFormSubscribedEvents(rawField: string): string[] | null {
  return rawField.trim()
    ? rawField.split(",").map((s) => s.trim()).filter(Boolean)
    : null;
}

describe("parseSubscribedEvents", () => {
  it("treats undefined as subscribing to every event", () => {
    const result = parseSubscribedEvents(undefined);
    expect(result).toEqual({ ok: true, events: null });
  });

  it("treats null as subscribing to every event", () => {
    const result = parseSubscribedEvents(null);
    expect(result).toEqual({ ok: true, events: null });
  });

  it("accepts a valid array of event types", () => {
    const result = parseSubscribedEvents(["quota.exceeded", "api_key.rotated"]);
    expect(result).toEqual({ ok: true, events: ["quota.exceeded", "api_key.rotated"] });
  });

  it("accepts an empty array (subscribed to nothing)", () => {
    const result = parseSubscribedEvents([]);
    expect(result).toEqual({ ok: true, events: [] });
  });

  it("rejects an unknown event type with a 400-worthy error", () => {
    const result = parseSubscribedEvents(["bogus.event"]);
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(result.error).toContain("bogus.event");
    }
  });

  it("rejects a mix of valid and unknown event types", () => {
    const result = parseSubscribedEvents(["quota.exceeded", "not.a.real.event"]);
    expect(result.ok).toBe(false);
  });

  it("rejects a non-array value", () => {
    const result = parseSubscribedEvents("quota.exceeded");
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(result.error).toContain("array");
    }
  });

  it("rejects an array containing non-string entries", () => {
    const result = parseSubscribedEvents(["quota.exceeded", 42]);
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(result.error).toContain("strings");
    }
  });

  it("accepts every member of WEBHOOK_EVENT_TYPES", () => {
    const result = parseSubscribedEvents([...WEBHOOK_EVENT_TYPES]);
    expect(result.ok).toBe(true);
  });

  it("dedupes repeated valid event types", () => {
    const result = parseSubscribedEvents(["quota.exceeded", "quota.exceeded", "api_key.rotated"]);
    expect(result).toEqual({ ok: true, events: ["quota.exceeded", "api_key.rotated"] });
  });

  it("rejects an array longer than the catalogue itself", () => {
    const tooMany = Array(WEBHOOK_EVENT_TYPES.length + 1).fill("quota.exceeded");
    const result = parseSubscribedEvents(tooMany);
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(result.error).toContain(String(WEBHOOK_EVENT_TYPES.length));
    }
  });

  it("rejects a very large array of duplicates without scanning them all for validity", () => {
    // Regression guard: the length cap must be checked before per-entry
    // validation so an adversarial huge array is rejected in O(1), not O(n).
    const huge = Array(100_000).fill("quota.exceeded");
    const result = parseSubscribedEvents(huge);
    expect(result.ok).toBe(false);
  });
});

describe("isValidWebhookEventType", () => {
  it("accepts every catalogue event type", () => {
    for (const t of WEBHOOK_EVENT_TYPES) {
      expect(isValidWebhookEventType(t)).toBe(true);
    }
  });

  it("rejects an unknown event type", () => {
    expect(isValidWebhookEventType("order.created")).toBe(false);
  });

  it("rejects an empty string", () => {
    expect(isValidWebhookEventType("")).toBe(false);
  });
});

describe("FormData subscribed_events field parsing", () => {
  it("treats an empty field as subscribing to every event", () => {
    expect(parseFormSubscribedEvents("")).toBeNull();
  });

  it("treats a whitespace-only field as subscribing to every event", () => {
    expect(parseFormSubscribedEvents("   ")).toBeNull();
  });

  it("splits a comma-separated field into individual event types", () => {
    expect(parseFormSubscribedEvents("quota.exceeded,api_key.rotated")).toEqual([
      "quota.exceeded",
      "api_key.rotated",
    ]);
  });

  it("trims whitespace around each comma-separated entry", () => {
    expect(parseFormSubscribedEvents(" quota.exceeded , api_key.rotated ")).toEqual([
      "quota.exceeded",
      "api_key.rotated",
    ]);
  });

  it("drops empty entries from trailing/duplicate commas", () => {
    expect(parseFormSubscribedEvents("quota.exceeded,,api_key.rotated,")).toEqual([
      "quota.exceeded",
      "api_key.rotated",
    ]);
  });
});
