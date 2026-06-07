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
// Returns Invalid Date (NaN getTime) for any string that is not exactly YYYY-MM-DD,
// preventing lenient JS engine parsing of partial strings or strings with appended time parts.
export function parseDateParam(s: string): Date {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(s)) {
    return new Date(NaN);
  }
  return new Date(s + "T00:00:00.000Z");
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
  // Mirrors route.ts:120 — strictly greater than, so exactly 90 days is allowed.
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
