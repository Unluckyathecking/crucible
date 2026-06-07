// Client-safe helpers for the usage analytics page.
// No server-only imports — this file is bundled for both client and server.

export const MAX_USAGE_RANGE_DAYS = 90;
export const MS_PER_DAY = 24 * 60 * 60 * 1000;

export interface RawEvent {
  operation: string;
  billable_units: number;
  created_at: string;
}

export interface DayBucket {
  date: string; // YYYY-MM-DD UTC
  units: number;
}

export interface OperationRow {
  operation: string;
  total_billable_units: number;
  event_count: number;
}

// Parses a YYYY-MM-DD string as UTC midnight.
// Returns Invalid Date for anything that is not a valid calendar date:
// - strings not matching YYYY-MM-DD format (including wrong lengths, time parts, spaces)
// - months outside 01-12, days outside 01-31
// - valid-format but overflowing dates (e.g. 2023-02-29, which JS would silently roll over)
export function parseDateParam(s: string): Date {
  if (!/^(\d{4})-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])$/.test(s)) {
    return new Date(NaN);
  }
  const d = new Date(s + "T00:00:00.000Z");
  // Detect month/day overflow: e.g. Feb 29 in a non-leap year rolls to Mar 1 silently.
  const [, m, day] = s.split("-").map(Number);
  if (d.getUTCMonth() + 1 !== m || d.getUTCDate() !== day) {
    return new Date(NaN);
  }
  return d;
}

// Returns YYYY-MM-DD from the UTC components of a Date.
// Uses getUTCFullYear/Month/Date directly to avoid timezone-dependent toISOString() shifts.
export function toISODateString(d: Date): string {
  return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, "0")}-${String(d.getUTCDate()).padStart(2, "0")}`;
}

// Validates a date range using the same rules as /api/usage route.ts:114-125.
// from and to are both UTC midnights; to is the exclusive upper bound (API convention).
export function validateDateRange(
  from: Date,
  to: Date,
): { valid: boolean; error?: string } {
  if (isNaN(from.getTime()) || isNaN(to.getTime())) {
    return { valid: false, error: "Invalid date" };
  }
  if (from.getTime() > to.getTime()) {
    return { valid: false, error: "'From' must not be after 'to'" };
  }
  // Strictly greater than, so exactly MAX_USAGE_RANGE_DAYS is allowed; one day over is rejected.
  if (to.getTime() - from.getTime() > MAX_USAGE_RANGE_DAYS * MS_PER_DAY) {
    return {
      valid: false,
      error: `Date range exceeds maximum of ${MAX_USAGE_RANGE_DAYS} days`,
    };
  }
  return { valid: true };
}

// Groups events by UTC calendar date and sums billable_units, sorted oldest-first.
// Skips events with malformed created_at. Clamps negative billable_units to 0
// (gateway enforces >= 1, but defensive against data corruption / future refund rows).
export function bucketByDay(events: RawEvent[]): DayBucket[] {
  const map = new Map<string, number>();
  for (const e of events) {
    const d = new Date(e.created_at);
    if (isNaN(d.getTime())) continue;
    const key = `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, "0")}-${String(d.getUTCDate()).padStart(2, "0")}`;
    map.set(key, (map.get(key) ?? 0) + Math.max(0, e.billable_units));
  }
  return Array.from(map.entries())
    .map(([date, units]) => ({ date, units }))
    .sort((a, b) => a.date.localeCompare(b.date));
}

// Aggregates events by operation, sorted by total_billable_units descending.
// Clamps negative billable_units to 0 (same rationale as bucketByDay).
export function aggregateByOperation(events: RawEvent[]): OperationRow[] {
  const map = new Map<string, { units: number; count: number }>();
  for (const e of events) {
    const cur = map.get(e.operation) ?? { units: 0, count: 0 };
    map.set(e.operation, {
      units: cur.units + Math.max(0, e.billable_units),
      count: cur.count + 1,
    });
  }
  return Array.from(map.entries())
    .map(([operation, { units, count }]) => ({
      operation,
      total_billable_units: units,
      event_count: count,
    }))
    .sort((a, b) => b.total_billable_units - a.total_billable_units);
}
