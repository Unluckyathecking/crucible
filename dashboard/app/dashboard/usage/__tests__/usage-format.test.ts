import { describe, it, expect } from "vitest";
import {
  validateDateRange,
  bucketByDay,
  aggregateByOperation,
  parseDateParam,
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

  it("accepts exactly 90 days (boundary: strictly greater-than rejects, equal allows)", () => {
    const from = parseDateParam("2024-01-01");
    const to = new Date(from.getTime() + MAX_USAGE_RANGE_DAYS * MS_PER_DAY);
    const result = validateDateRange(from, to);
    expect(result.valid).toBe(true);
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
      { operation: "search", billable_units: 5, created_at: "2024-01-15T08:00:00.000Z" },
      { operation: "export", billable_units: 3, created_at: "2024-01-15T14:00:00.000Z" },
      { operation: "search", billable_units: 10, created_at: "2024-01-16T00:00:00.000Z" },
    ];
    const buckets = bucketByDay(events);
    expect(buckets).toHaveLength(2);
    expect(buckets[0]).toEqual({ date: "2024-01-15", units: 8 });
    expect(buckets[1]).toEqual({ date: "2024-01-16", units: 10 });
  });

  it("sorts buckets chronologically (oldest-first)", () => {
    const events = [
      { operation: "a", billable_units: 1, created_at: "2024-03-01T00:00:00.000Z" },
      { operation: "a", billable_units: 1, created_at: "2024-01-01T00:00:00.000Z" },
    ];
    const buckets = bucketByDay(events);
    expect(buckets[0].date).toBe("2024-01-01");
    expect(buckets[1].date).toBe("2024-03-01");
  });

  it("uses UTC midnight boundaries (event at 23:59 UTC is on its UTC date)", () => {
    const events = [
      { operation: "a", billable_units: 7, created_at: "2024-06-15T23:59:59.999Z" },
    ];
    const buckets = bucketByDay(events);
    expect(buckets[0].date).toBe("2024-06-15");
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
      { operation: "search", billable_units: 5, created_at: "2024-01-15T08:00:00.000Z" },
      { operation: "search", billable_units: 3, created_at: "2024-01-15T14:00:00.000Z" },
      { operation: "export", billable_units: 20, created_at: "2024-01-16T08:00:00.000Z" },
    ];
    const rows = aggregateByOperation(events);
    const search = rows.find((r) => r.operation === "search")!;
    expect(search.total_billable_units).toBe(8);
    expect(search.event_count).toBe(2);
    const exp = rows.find((r) => r.operation === "export")!;
    expect(exp.total_billable_units).toBe(20);
    expect(exp.event_count).toBe(1);
  });

  it("sorts by total_billable_units descending", () => {
    const events = [
      { operation: "cheap", billable_units: 1, created_at: "2024-01-01T00:00:00.000Z" },
      { operation: "expensive", billable_units: 100, created_at: "2024-01-01T00:00:00.000Z" },
    ];
    const rows = aggregateByOperation(events);
    expect(rows[0].operation).toBe("expensive");
    expect(rows[1].operation).toBe("cheap");
  });
});
