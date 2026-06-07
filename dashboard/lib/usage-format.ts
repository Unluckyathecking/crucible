// Client-safe helpers for the usage analytics page.
// No server-only imports — this file is bundled for both client and server.

// MS_PER_DAY and MAX_USAGE_RANGE_DAYS are the authoritative values shared between
// this client-safe file and lib/db.ts — neither defines them independently.
import { MS_PER_DAY, MAX_USAGE_RANGE_DAYS } from "./constants";
export { MAX_USAGE_RANGE_DAYS };
// Earliest year accepted by parseDateParam. Analytics data does not predate the Unix epoch.
const MIN_YEAR = 1970;

export interface RawEvent {
  /** BIGSERIAL primary key from usage_events, cast to text in SQL (::text). Unique per row. */
  id: string;
  operation: string;
  /** Validated as a finite integer by isRawEvent; safe for BigInt() conversion. */
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
// Returns Invalid Date for anything not matching the format or not a real calendar date.
// Uses Date.UTC with numeric components so JS overflow-normalisation (e.g. Feb 30 → Mar 1)
// is detectable: if any UTC component doesn't round-trip, the input date is impossible.
export function parseDateParam(s: string): Date {
  // Simple structural check first: exactly YYYY-MM-DD (10 chars, two-digit month and day).
  if (!/^\d{4}-\d{2}-\d{2}$/.test(s)) return new Date(NaN);
  // parseInt with explicit radix 10 avoids any engine-specific octal ambiguity
  // for values like "08"/"09" that Number() handles correctly in ES5+ but that
  // older lint rules and engines historically treated as invalid octals.
  const [y, m, day] = s.split("-").map((v) => parseInt(v, 10));
  // Lower bound: analytics data does not predate the Unix epoch, and years
  // 0–99 trigger Date.UTC's two-digit-year quirk (y=99 → 1999). The round-trip
  // check catches this (getUTCFullYear() returns 1999 ≠ 99), but the MIN_YEAR
  // guard rejects them first for clarity.
  // Upper bound: computed per-call so long-lived server processes and open
  // browser tabs stay correct across year boundaries without a restart.
  // getUTCFullYear() extracts the UTC year, unaffected by local timezone offset.
  const nowYear = new Date().getUTCFullYear();
  if (y < MIN_YEAR || y > nowYear + 1) return new Date(NaN);
  // Explicit bounds: month 1–12, day 1–31. Narrower calendar constraints
  // (Feb 30, Apr 31, etc.) are caught by the round-trip check below:
  // Date.UTC normalises overflow (Feb 30 → Mar 1), and the UTC component
  // comparison detects the mismatch, returning Invalid Date.
  if (m < 1 || m > 12 || day < 1 || day > 31) return new Date(NaN);
  const d = new Date(Date.UTC(y, m - 1, day));
  if (d.getUTCFullYear() !== y || d.getUTCMonth() + 1 !== m || d.getUTCDate() !== day) {
    return new Date(NaN);
  }
  return d;
}

// Shared UTC date formatter used by toISODateString and bucketByDay.
function formatUTCDate(d: Date): string {
  return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, "0")}-${String(d.getUTCDate()).padStart(2, "0")}`;
}

// Returns YYYY-MM-DD from the UTC components of a Date, or "" for Invalid Date.
// Uses getUTCFullYear/Month/Date directly to avoid timezone-dependent toISOString() shifts.
export function toISODateString(d: Date): string {
  if (isNaN(d.getTime())) return "";
  return formatUTCDate(d);
}

// Validates a date range using the same rules as the /api/usage server route:
// from and to are UTC midnights; to is the exclusive upper bound (API convention);
// maximum span is MAX_USAGE_RANGE_DAYS days.
// Accepts null/undefined to allow callers to forward unvalidated parsed values.
export function validateDateRange(
  from: Date | null | undefined,
  to: Date | null | undefined,
): { valid: boolean; error?: string } {
  if (from == null || to == null || isNaN(from.getTime()) || isNaN(to.getTime())) {
    return { valid: false, error: "Invalid date" };
  }
  if (from.getTime() > to.getTime()) {
    return { valid: false, error: "'From' must not be after 'To'" };
  }
  // `>` not `>=`: an exclusive diff of exactly MAX_USAGE_RANGE_DAYS days is allowed.
  // With handleApply's apiTo = userTo + 1 day, this corresponds to exactly
  // MAX_USAGE_RANGE_DAYS inclusive user-visible calendar days — matching the
  // server-side validation in the /api/usage route.
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
// Precision: billable_units values are integers (gateway enforces ≥ 1). JavaScript's
// number type is exact for integer arithmetic up to Number.MAX_SAFE_INTEGER, so
// per-day sums are lossless as long as no single day exceeds ~9 × 10^15 units.
export function bucketByDay(events: RawEvent[]): DayBucket[] {
  const map = new Map<string, number>();
  // MAX_USAGE_RANGE_DAYS + 1 is the maximum number of distinct calendar days that can
  // appear in a valid query window (from-inclusive, to-exclusive, up to 90 days).
  // Capping here prevents unbounded Map growth if callers ever pass unvalidated input.
  const MAX_BUCKETS = MAX_USAGE_RANGE_DAYS + 1;
  for (const e of events) {
    const d = new Date(e.created_at);
    if (isNaN(d.getTime())) continue;
    const key = formatUTCDate(d);
    if (!map.has(key) && map.size >= MAX_BUCKETS) continue;
    map.set(key, (map.get(key) ?? 0) + Math.max(0, e.billable_units));
  }
  return Array.from(map.entries())
    .map(([date, units]) => ({ date, units }))
    .sort((a, b) => a.date.localeCompare(b.date)); // YYYY-MM-DD lexicographic === chronological
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
