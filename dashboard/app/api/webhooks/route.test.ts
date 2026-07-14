/**
 * Unit tests for the api/webhooks subscribed_events validation logic and
 * the isPrivateHostname SSRF guard.
 *
 * We cannot import route.ts directly because it imports @/auth, which requires
 * the full NextAuth + Postgres setup (see lib/__tests__/db.test.ts and
 * app/api/keys/__tests__/route.test.ts for the same constraint). Instead we
 * re-implement the extractable business-logic rules the handler embeds:
 *   1. WEBHOOK_EVENT_TYPES membership check (mirrors gateway/internal/events.AllEventTypes)
 *   2. parseSubscribedEvents: null/undefined → "all events"; array of valid
 *      strings → stored subscription; anything else → a rejection error
 *   3. FormData's comma-separated subscribed_events field → string[] | null
 *   4. isPrivateHostname: SSRF guard — rejects loopback, private IPv4, and IPv6
 *      loopback/ULA/link-local/IPv4-mapped address literals at registration time
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

/** Mirror of isPrivateHostname in route.ts — keep in sync. */
function isPrivateHostname(hostname: string): boolean {
  const h = hostname.toLowerCase();

  if (h === "localhost") return true;

  if (h === "::1" || h === "[::1]") return true;

  // IPv6 unique-local (fc00::/7): first octet is fc or fd
  if (h.startsWith("[fc") || h.startsWith("[fd")) return true;
  if ((h.startsWith("fc") || h.startsWith("fd")) && h.includes(":")) return true;

  // IPv6 link-local (fe80::/10): fe80 through febf
  if (h.startsWith("[fe8") || h.startsWith("[fe9") ||
      h.startsWith("[fea") || h.startsWith("[feb")) return true;
  if ((h.startsWith("fe8") || h.startsWith("fe9") ||
       h.startsWith("fea") || h.startsWith("feb")) && h.includes(":")) return true;

  // IPv4-mapped IPv6 (::ffff::/96)
  if (h.startsWith("[::ffff:") || h.startsWith("::ffff:")) return true;

  const ipv4 = h.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);

  if (/^\d/.test(h) && !ipv4) {
    return true;
  }

  if (ipv4) {
    const [a, b, c, d] = [ipv4[1], ipv4[2], ipv4[3], ipv4[4]].map(Number);
    if (a > 255 || b > 255 || c > 255 || d > 255) return true;
    if (a === 10) return true;
    if (a === 127) return true;
    if (a === 169 && b === 254) return true;
    if (a === 172 && b >= 16 && b <= 31) return true;
    if (a === 192 && b === 168) return true;
    if (a === 0) return true;
    if (a === 100 && b >= 64 && b <= 127) return true;
  }

  return false;
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

describe("isPrivateHostname", () => {
  describe("IPv6 loopback (regression guard)", () => {
    it("blocks bare ::1", () => {
      expect(isPrivateHostname("::1")).toBe(true);
    });
    it("blocks bracketed [::1]", () => {
      expect(isPrivateHostname("[::1]")).toBe(true);
    });
  });

  describe("IPv6 unique-local (fc00::/7)", () => {
    it("blocks [fc00::1] in bracketed form", () => {
      expect(isPrivateHostname("[fc00::1]")).toBe(true);
    });
    it("blocks [fd12:3456::1] in bracketed form", () => {
      expect(isPrivateHostname("[fd12:3456::1]")).toBe(true);
    });
    it("blocks fc00::1 in bare form", () => {
      expect(isPrivateHostname("fc00::1")).toBe(true);
    });
    it("blocks fd00::1 in bare form", () => {
      expect(isPrivateHostname("fd00::1")).toBe(true);
    });
    it("is case-insensitive for bracketed ULA", () => {
      expect(isPrivateHostname("[FC00::1]")).toBe(true);
      expect(isPrivateHostname("[FD12::1]")).toBe(true);
    });
  });

  describe("IPv6 link-local (fe80::/10)", () => {
    it("blocks [fe80::1] (low end of range)", () => {
      expect(isPrivateHostname("[fe80::1]")).toBe(true);
    });
    it("blocks [fe90::ab] (fe90:: within /10)", () => {
      expect(isPrivateHostname("[fe90::ab]")).toBe(true);
    });
    it("blocks [fea0::1] (fea0:: within /10)", () => {
      expect(isPrivateHostname("[fea0::1]")).toBe(true);
    });
    it("blocks [feb0::1] (feb0:: within /10)", () => {
      expect(isPrivateHostname("[feb0::1]")).toBe(true);
    });
    it("blocks [febf::1] (high end of fe80::/10 range)", () => {
      expect(isPrivateHostname("[febf::1]")).toBe(true);
    });
    it("blocks bare fe80::1", () => {
      expect(isPrivateHostname("fe80::1")).toBe(true);
    });
    it("is case-insensitive for bracketed link-local", () => {
      expect(isPrivateHostname("[FE80::1]")).toBe(true);
      expect(isPrivateHostname("[FEAF::1]")).toBe(true);
    });
    it("does not block [fec0::1] — just above the fe80::/10 range", () => {
      expect(isPrivateHostname("[fec0::1]")).toBe(false);
    });
  });

  describe("IPv4-mapped IPv6 (::ffff::/96)", () => {
    it("blocks a private IPv4-mapped address in bracketed form", () => {
      // new URL('https://[::ffff:10.0.0.1]/').hostname normalizes to [::ffff:a00:1]
      expect(isPrivateHostname("[::ffff:a00:1]")).toBe(true);
    });
    it("blocks a private IPv4-mapped address in bare form", () => {
      expect(isPrivateHostname("::ffff:a00:1")).toBe(true);
    });
  });

  describe("public hostnames that must not be blocked", () => {
    it("allows a hostname starting with 'fc' that has no colon (not IPv6)", () => {
      expect(isPrivateHostname("fc-api.example.com")).toBe(false);
    });
    it("allows a hostname starting with 'fea' that has no colon (not IPv6)", () => {
      expect(isPrivateHostname("feast.io")).toBe(false);
    });
    it("allows a hostname starting with 'feb' that has no colon (not IPv6)", () => {
      expect(isPrivateHostname("february.example.com")).toBe(false);
    });
    it("allows an ordinary public hostname", () => {
      expect(isPrivateHostname("webhook.example.com")).toBe(false);
    });
  });
});
