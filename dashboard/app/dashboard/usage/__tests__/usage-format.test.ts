import { describe, it, expect } from "vitest";
import {
  validateDateRange,
  bucketByDay,
  aggregateByOperation,
  parseDateParam,
  toISODateString,
  MAX_USAGE_RANGE_DAYS,
  MS_PER_DAY,
} from "@/lib/usage-format";

// ---------------------------------------------------------------------------
// validateDateRange — mirrors the server-side checks in /api/usage/route.ts
// ---------------------------------------------------------------------------

describe("validateDateRange", () => {
  it("accepts a range well within 90 days", () => {
    const from = parseDateParam("2024-01-01");
    const to = parseDateParam("2024-02-15"); // 45 days
    expect(validateDateRange(from, to).valid).toBe(true);
  });

  it("accepts exclusive diff of exactly MAX_USAGE_RANGE_DAYS days", () => {
    // 2024-01-01 to 2024-03-31 is an exclusive diff of exactly 90 days.
    // Jan (31) + Feb 2024 leap (29) + Mar 1-30 (30) = 90 days.
    // validateDateRange uses strict > so a diff of exactly MAX_USAGE_RANGE_DAYS is accepted.
    const from = parseDateParam("2024-01-01");
    const to = parseDateParam("2024-03-31");
    expect(validateDateRange(from, to).valid).toBe(true);
  });

  it("rejects a range of 90 days + 1 millisecond", () => {
    const from = parseDateParam("2024-01-01");
    const to = new Date(from.getTime() + MAX_USAGE_RANGE_DAYS * MS_PER_DAY + 1);
    const result = validateDateRange(from, to);
    expect(result.valid).toBe(false);
    expect(result.error).toContain("90");
  });

  it("rejects a range exceeding 90 days", () => {
    const from = parseDateParam("2024-01-01");
    const to = new Date(from.getTime() + (MAX_USAGE_RANGE_DAYS + 1) * MS_PER_DAY);
    const result = validateDateRange(from, to);
    expect(result.valid).toBe(false);
    expect(result.error).toContain("90");
  });

  it("rejects from > to", () => {
    const from = parseDateParam("2024-03-01");
    const to = parseDateParam("2024-01-01");
    const result = validateDateRange(from, to);
    expect(result.valid).toBe(false);
    expect(result.error).toContain("From");
  });

  it("accepts from === to (empty half-open interval is valid)", () => {
    const d = parseDateParam("2024-06-01");
    expect(validateDateRange(d, d).valid).toBe(true);
  });

  it("rejects an invalid 'from' date", () => {
    const result = validateDateRange(new Date("not-a-date"), parseDateParam("2024-01-01"));
    expect(result.valid).toBe(false);
  });

  it("rejects an invalid 'to' date", () => {
    const result = validateDateRange(parseDateParam("2024-01-01"), new Date("bad"));
    expect(result.valid).toBe(false);
  });

  it("rejects null 'from' date", () => {
    // validateDateRange accepts null/undefined to handle unvalidated parsed values.
    const result = validateDateRange(null, parseDateParam("2024-01-01"));
    expect(result.valid).toBe(false);
  });

  it("rejects undefined 'to' date", () => {
    const result = validateDateRange(parseDateParam("2024-01-01"), undefined);
    expect(result.valid).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// bucketByDay — client-side data-shaping helper
// ---------------------------------------------------------------------------

describe("bucketByDay", () => {
  it("returns empty array for no events", () => {
    expect(bucketByDay([])).toEqual([]);
  });

  it("groups events on the same UTC day and sums units", () => {
    const events = [
      { id: "1", operation: "search", billable_units: 5, created_at: "2024-01-15T08:00:00.000Z" },
      { id: "2", operation: "export", billable_units: 3, created_at: "2024-01-15T14:00:00.000Z" },
      { id: "3", operation: "search", billable_units: 10, created_at: "2024-01-16T00:00:00.000Z" },
    ];
    const buckets = bucketByDay(events);
    expect(buckets).toHaveLength(2);
    expect(buckets[0]).toEqual({ date: "2024-01-15", units: 8 });
    expect(buckets[1]).toEqual({ date: "2024-01-16", units: 10 });
  });

  it("sorts buckets chronologically (oldest-first)", () => {
    const events = [
      { id: "1", operation: "a", billable_units: 1, created_at: "2024-03-01T00:00:00.000Z" },
      { id: "2", operation: "a", billable_units: 1, created_at: "2024-01-01T00:00:00.000Z" },
    ];
    const buckets = bucketByDay(events);
    expect(buckets[0].date).toBe("2024-01-01");
    expect(buckets[1].date).toBe("2024-03-01");
  });

  it("uses UTC midnight boundaries (event at 23:59 UTC is on its UTC date)", () => {
    const events = [
      { id: "1", operation: "a", billable_units: 7, created_at: "2024-06-15T23:59:59.999Z" },
    ];
    expect(events[0].created_at).toContain("T23:");
    const buckets = bucketByDay(events);
    expect(buckets[0].date).toBe("2024-06-15");
  });

  it("buckets by UTC date even when local date differs (23:00 UTC = next day in +1 or later timezone)", () => {
    const events = [
      { id: "1", operation: "a", billable_units: 1, created_at: "2024-01-15T23:00:00.000Z" },
    ];
    const buckets = bucketByDay(events);
    expect(buckets).toHaveLength(1);
    expect(buckets[0].date).toBe("2024-01-15");
  });
});

// ---------------------------------------------------------------------------
// aggregateByOperation — client-side data-shaping helper
// ---------------------------------------------------------------------------

describe("aggregateByOperation", () => {
  it("returns empty array for no events", () => {
    expect(aggregateByOperation([])).toEqual([]);
  });

  it("counts each event and sums units per operation", () => {
    const events = [
      { id: "1", operation: "search", billable_units: 5, created_at: "2024-01-15T08:00:00.000Z" },
      { id: "2", operation: "search", billable_units: 3, created_at: "2024-01-15T14:00:00.000Z" },
      { id: "3", operation: "export", billable_units: 20, created_at: "2024-01-16T08:00:00.000Z" },
    ];
    const rows = aggregateByOperation(events);
    const search = rows.find((r) => r.operation === "search");
    expect(search).toBeDefined();
    expect(search!.total_billable_units).toBe(8);
    expect(search!.event_count).toBe(2);
    const exp = rows.find((r) => r.operation === "export");
    expect(exp).toBeDefined();
    expect(exp!.total_billable_units).toBe(20);
    expect(exp!.event_count).toBe(1);
  });

  it("sorts by total_billable_units descending", () => {
    const events = [
      { id: "1", operation: "cheap", billable_units: 1, created_at: "2024-01-01T00:00:00.000Z" },
      { id: "2", operation: "expensive", billable_units: 100, created_at: "2024-01-01T00:00:00.000Z" },
    ];
    const rows = aggregateByOperation(events);
    expect(rows[0].operation).toBe("expensive");
    expect(rows[1].operation).toBe("cheap");
  });

  it("clamps negative billable_units to 0 (defensive against data corruption)", () => {
    const events = [
      { id: "1", operation: "op", billable_units: -10, created_at: "2024-01-01T00:00:00.000Z" },
      { id: "2", operation: "op", billable_units: 5, created_at: "2024-01-01T00:00:00.000Z" },
    ];
    const rows = aggregateByOperation(events);
    expect(rows[0].total_billable_units).toBe(5);
    expect(rows[0].event_count).toBe(2);
  });

  it("groups events with empty-string operation key under empty string", () => {
    const events = [
      { id: "1", operation: "", billable_units: 3, created_at: "2024-01-01T00:00:00.000Z" },
      { id: "2", operation: "named", billable_units: 1, created_at: "2024-01-01T00:00:00.000Z" },
    ];
    const rows = aggregateByOperation(events);
    const emptyRow = rows.find((r) => r.operation === "");
    const namedRow = rows.find((r) => r.operation === "named");
    expect(emptyRow).toBeDefined();
    expect(emptyRow?.total_billable_units).toBe(3);
    expect(namedRow).toBeDefined();
    expect(namedRow?.total_billable_units).toBe(1);
  });

  it("clamps all-negative billable_units to 0 (total is 0, count is 2)", () => {
    const events = [
      { id: "1", operation: "op", billable_units: -5, created_at: "2024-01-01T00:00:00.000Z" },
      { id: "2", operation: "op", billable_units: -10, created_at: "2024-01-01T00:00:00.000Z" },
    ];
    const rows = aggregateByOperation(events);
    expect(rows[0].total_billable_units).toBe(0);
    expect(rows[0].event_count).toBe(2);
  });

  it("preserves zero billable_units (not clamped, counted)", () => {
    const events = [
      { id: "1", operation: "op", billable_units: 0, created_at: "2024-01-01T00:00:00.000Z" },
    ];
    const rows = aggregateByOperation(events);
    expect(rows[0].total_billable_units).toBe(0);
    expect(rows[0].event_count).toBe(1);
  });
});

// ---------------------------------------------------------------------------
// Edge cases: malformed / adversarial API responses
// ---------------------------------------------------------------------------

describe("bucketByDay — edge cases", () => {
  it("skips events with malformed created_at (no NaN-NaN-NaN bucket)", () => {
    const events = [
      { id: "1", operation: "a", billable_units: 5, created_at: "not-a-date" },
      { id: "2", operation: "a", billable_units: 3, created_at: "2024-01-15T08:00:00.000Z" },
    ];
    const buckets = bucketByDay(events);
    expect(buckets).toHaveLength(1);
    expect(buckets[0].date).toBe("2024-01-15");
    expect(buckets[0].units).toBe(3);
    const nanBucket = buckets.find((b) => b.date.includes("NaN"));
    expect(nanBucket).toBeUndefined();
  });

  it("returns empty array when all events have malformed created_at", () => {
    const events = [
      { id: "1", operation: "a", billable_units: 1, created_at: "" },
      { id: "2", operation: "a", billable_units: 1, created_at: "invalid" },
    ];
    expect(bucketByDay(events)).toEqual([]);
  });

  it("clamps negative billable_units to 0", () => {
    const events = [
      { id: "1", operation: "a", billable_units: -5, created_at: "2024-01-15T00:00:00.000Z" },
      { id: "2", operation: "a", billable_units: 10, created_at: "2024-01-15T12:00:00.000Z" },
    ];
    const buckets = bucketByDay(events);
    expect(buckets[0].units).toBe(10);
  });

  it("handles very large billable_units without throwing", () => {
    const events = [
      {
        id: "1",
        operation: "a",
        billable_units: Number.MAX_SAFE_INTEGER,
        created_at: "2024-01-15T00:00:00.000Z",
      },
    ];
    const buckets = bucketByDay(events);
    expect(buckets[0].units).toBe(Number.MAX_SAFE_INTEGER);
  });

  it("buckets same month/day in different years separately", () => {
    const events = [
      { id: "1", operation: "a", billable_units: 1, created_at: "2023-01-01T00:00:00.000Z" },
      { id: "2", operation: "a", billable_units: 2, created_at: "2024-01-01T00:00:00.000Z" },
    ];
    const buckets = bucketByDay(events);
    expect(buckets).toHaveLength(2);
    expect(buckets[0]).toEqual({ date: "2023-01-01", units: 1 });
    expect(buckets[1]).toEqual({ date: "2024-01-01", units: 2 });
  });

  it("aggregates two events per day across full range into daily buckets", () => {
    // Two events per day across MAX_USAGE_RANGE_DAYS days — verifies that each unique
    // UTC date gets its own bucket and that multiple events per day are summed.
    const events = Array.from({ length: MAX_USAGE_RANGE_DAYS * 2 }, (_, i) => ({
      id: String(i),
      operation: "op",
      billable_units: 1,
      created_at: new Date(
        Date.UTC(2024, 0, 1) + Math.floor(i / 2) * MS_PER_DAY,
      ).toISOString(),
    }));
    const buckets = bucketByDay(events);
    expect(buckets).toHaveLength(MAX_USAGE_RANGE_DAYS);
    expect(buckets[0].date).toBe("2024-01-01");
    expect(buckets[MAX_USAGE_RANGE_DAYS - 1].date).toBe("2024-03-30");
    expect(buckets.every((b) => b.units === 2)).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// parseDateParam — input validation and UTC parsing
// ---------------------------------------------------------------------------

describe("parseDateParam", () => {
  it("parses a valid YYYY-MM-DD string as UTC midnight", () => {
    const d = parseDateParam("2024-06-15");
    expect(d.getTime()).toBe(new Date("2024-06-15T00:00:00.000Z").getTime());
    expect(d.toISOString()).toBe("2024-06-15T00:00:00.000Z");
  });

  it("returns Invalid Date for empty string", () => {
    expect(isNaN(parseDateParam("").getTime())).toBe(true);
  });

  it("returns Invalid Date for non-ISO string", () => {
    expect(isNaN(parseDateParam("not-a-date").getTime())).toBe(true);
  });

  it("returns Invalid Date for partial ISO string (YYYY-MM)", () => {
    expect(isNaN(parseDateParam("2024-01").getTime())).toBe(true);
  });

  it("returns Invalid Date for string with time component already appended", () => {
    // The strict YYYY-MM-DD regex rejects strings containing time components.
    expect(isNaN(parseDateParam("2024-01-01T00:00:00.000Z").getTime())).toBe(true);
  });

  it("returns Invalid Date for string with leading/trailing space", () => {
    expect(isNaN(parseDateParam(" 2024-01-01").getTime())).toBe(true);
    expect(isNaN(parseDateParam("2024-01-01 ").getTime())).toBe(true);
  });

  it("returns Invalid Date for out-of-range month (2024-13-01)", () => {
    expect(isNaN(parseDateParam("2024-13-01").getTime())).toBe(true);
  });

  it("returns Invalid Date for out-of-range day (2024-01-32)", () => {
    expect(isNaN(parseDateParam("2024-01-32").getTime())).toBe(true);
  });

  it("returns Invalid Date for zero month (2024-00-01)", () => {
    expect(isNaN(parseDateParam("2024-00-01").getTime())).toBe(true);
  });

  it("returns Invalid Date for zero day (2024-01-00)", () => {
    expect(isNaN(parseDateParam("2024-01-00").getTime())).toBe(true);
  });

  it("returns Invalid Date for Feb 29 in a non-leap year (rejected by UTC round-trip check, not rolled to Mar 1)", () => {
    expect(isNaN(parseDateParam("2023-02-29").getTime())).toBe(true);
  });

  it("accepts Feb 29 in a leap year (2024-02-29 is valid)", () => {
    const d = parseDateParam("2024-02-29");
    expect(isNaN(d.getTime())).toBe(false);
    expect(d.toISOString()).toBe("2024-02-29T00:00:00.000Z");
  });

  // 30-day months: day=31 is in the 1–31 bounds check but must be caught by the
  // UTC round-trip (Date.UTC normalises Apr 31 → May 1, month component mismatches).
  it("returns Invalid Date for Apr 31 (30-day month, caught by UTC round-trip)", () => {
    expect(isNaN(parseDateParam("2024-04-31").getTime())).toBe(true);
  });

  it("returns Invalid Date for Jun 31 (30-day month, caught by UTC round-trip)", () => {
    expect(isNaN(parseDateParam("2024-06-31").getTime())).toBe(true);
  });

  it("returns Invalid Date for Sep 31 (30-day month, caught by UTC round-trip)", () => {
    expect(isNaN(parseDateParam("2024-09-31").getTime())).toBe(true);
  });

  it("returns Invalid Date for Nov 31 (30-day month, caught by UTC round-trip)", () => {
    expect(isNaN(parseDateParam("2024-11-31").getTime())).toBe(true);
  });

  it("returns Invalid Date for Feb 30 in a leap year (caught by UTC round-trip)", () => {
    expect(isNaN(parseDateParam("2024-02-30").getTime())).toBe(true);
  });

  it("returns Invalid Date for year 0000 (below minimum year 1970)", () => {
    expect(isNaN(parseDateParam("0000-01-01").getTime())).toBe(true);
  });

  it("returns Invalid Date for year 1969 (below minimum year 1970)", () => {
    expect(isNaN(parseDateParam("1969-12-31").getTime())).toBe(true);
  });

  it("accepts year 1970 (minimum valid year)", () => {
    expect(isNaN(parseDateParam("1970-01-01").getTime())).toBe(false);
  });

  it("returns Invalid Date for year two years ahead (exceeds currentYear+1 upper bound)", () => {
    const twoAhead = String(new Date().getUTCFullYear() + 2).padStart(4, "0");
    expect(isNaN(parseDateParam(`${twoAhead}-01-01`).getTime())).toBe(true);
  });

  it("accepts year one year ahead (equals currentYear+1 upper bound)", () => {
    const oneAhead = String(new Date().getUTCFullYear() + 1).padStart(4, "0");
    expect(isNaN(parseDateParam(`${oneAhead}-01-01`).getTime())).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// toISODateString — UTC component extraction (no toISOString() timezone shift)
// ---------------------------------------------------------------------------

describe("toISODateString", () => {
  it("returns YYYY-MM-DD from a UTC-midnight Date", () => {
    const d = new Date("2024-06-15T00:00:00.000Z");
    expect(toISODateString(d)).toBe("2024-06-15");
  });

  it("uses UTC components, not local time (no midnight-crossing shift)", () => {
    // Date.UTC guarantees the value is UTC midnight regardless of local timezone.
    const d = new Date(Date.UTC(2024, 0, 1)); // Jan 1 UTC midnight
    expect(toISODateString(d)).toBe("2024-01-01");
  });

  it("pads single-digit months and days", () => {
    const d = new Date(Date.UTC(2024, 0, 5)); // Jan 5
    expect(toISODateString(d)).toBe("2024-01-05");
  });

  it("round-trips with parseDateParam", () => {
    const original = "2024-11-30";
    expect(toISODateString(parseDateParam(original))).toBe(original);
  });

  it("returns empty string for Invalid Date input", () => {
    expect(toISODateString(new Date(NaN))).toBe("");
  });
});
