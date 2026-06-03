// GET /api/usage — returns raw usage events [{operation, billable_units, created_at}]
// for the authenticated customer over the requested time window.
// All date parameters are interpreted as UTC midnight. Clients should express dates in UTC.
import { randomUUID } from "crypto";
import { auth } from "@/auth";
import { ensureCustomer, listUsageEvents, MAX_USAGE_RANGE_DAYS, MAX_OPERATION_LENGTH, MS_PER_DAY } from "@/lib/db";

const DEFAULT_DAYS = 30;
// Accepts ISO 8601 date-only (YYYY-MM-DD). Month bounded 01-12, day 01-31;
// calendar validity (e.g. Feb 31) is enforced by the round-trip check below.
const ISO_DATE_RE = /^\d{4}-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])$/;

export async function GET(request: Request): Promise<Response> {
  try {
    const session = await auth();
    if (!session?.user?.email) {
      return new Response(JSON.stringify({ error: "Unauthorized" }), {
        status: 401,
        headers: { "content-type": "application/json" },
      });
    }

    const customer = await ensureCustomer(session.user!.email);

    const url = new URL(request.url);
    const fromParam = url.searchParams.get("from");
    const toParam = url.searchParams.get("to");
    const operationRaw = url.searchParams.get("operation");
    // Fast-path: reject before spread to avoid OOM on multi-megabyte inputs.
    if (operationRaw !== null && operationRaw.length > MAX_OPERATION_LENGTH * 2) {
      return new Response(JSON.stringify({ error: "operation parameter too long" }), {
        status: 400,
        headers: { "content-type": "application/json" },
      });
    }
    const operationTrimmed = operationRaw?.trim();
    // Reject both explicitly empty and whitespace-only; both mean the caller omitted the value.
    if (operationRaw !== null && operationTrimmed === "") {
      return new Response(JSON.stringify({ error: "operation parameter must not be empty" }), {
        status: 400,
        headers: { "content-type": "application/json" },
      });
    }
    if (operationTrimmed !== undefined && [...operationTrimmed].length > MAX_OPERATION_LENGTH) {
      return new Response(JSON.stringify({ error: "operation parameter too long" }), {
        status: 400,
        headers: { "content-type": "application/json" },
      });
    }
    const operationParam = operationTrimmed || undefined;

    const now = new Date();
    // Use Date.UTC calendar arithmetic so from/to are always exact UTC midnight
    // boundaries regardless of DST or leap seconds in the server's local timezone.
    const tomorrowMidnight = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() + 1));
    let from = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() + 1 - DEFAULT_DAYS));
    let to = tomorrowMidnight;

    if (fromParam) {
      if (!ISO_DATE_RE.test(fromParam)) {
        return new Response(JSON.stringify({ error: "invalid 'from' date, expected ISO 8601" }), {
          status: 400,
          headers: { "content-type": "application/json" },
        });
      }
      // Append explicit UTC midnight so the semantics match the database (UTC timestamps).
      const parsed = new Date(fromParam + 'T00:00:00.000Z');
      // Round-trip check catches calendar-invalid strings like 2023-02-31 that JS silently shifts.
      if (isNaN(parsed.getTime()) || parsed.toISOString().slice(0, 10) !== fromParam) {
        return new Response(JSON.stringify({ error: "invalid 'from' date" }), {
          status: 400,
          headers: { "content-type": "application/json" },
        });
      }
      from = parsed;
    }
    if (toParam) {
      if (!ISO_DATE_RE.test(toParam)) {
        return new Response(JSON.stringify({ error: "invalid 'to' date, expected ISO 8601" }), {
          status: 400,
          headers: { "content-type": "application/json" },
        });
      }
      // Append explicit UTC midnight so the semantics match the database (UTC timestamps).
      const parsed = new Date(toParam + 'T00:00:00.000Z');
      if (isNaN(parsed.getTime()) || parsed.toISOString().slice(0, 10) !== toParam) {
        return new Response(JSON.stringify({ error: "invalid 'to' date" }), {
          status: 400,
          headers: { "content-type": "application/json" },
        });
      }
      to = parsed;
    }

    if (from.getTime() > to.getTime()) {
      return new Response(JSON.stringify({ error: "'from' must not be after 'to'" }), {
        status: 400,
        headers: { "content-type": "application/json" },
      });
    }
    if (to.getTime() - from.getTime() > MAX_USAGE_RANGE_DAYS * MS_PER_DAY) {
      return new Response(JSON.stringify({ error: `date range exceeds maximum of ${MAX_USAGE_RANGE_DAYS} days` }), {
        status: 400,
        headers: { "content-type": "application/json" },
      });
    }
    if (to.getTime() > tomorrowMidnight.getTime()) {
      return new Response(JSON.stringify({ error: "'to' date cannot be after tomorrow" }), {
        status: 400,
        headers: { "content-type": "application/json" },
      });
    }

    // Returns raw usage event rows (newest first, capped at MAX_USAGE_EVENTS_LIMIT).
    // The dashboard server component uses usageByOperation for per-operation aggregates.
    const rows = await listUsageEvents(customer.id, from, to, operationParam);

    return new Response(JSON.stringify(rows), {
      headers: { "content-type": "application/json", "cache-control": "no-store" },
    });
  } catch (err) {
    const errorId = randomUUID();
    console.error("GET /api/usage failed:", {
      errorId,
      error: err instanceof Error ? err.message : String(err),
      stack: err instanceof Error ? err.stack : undefined,
    });
    return new Response(JSON.stringify({ error: "Internal server error", errorId }), {
      status: 500,
      headers: { "content-type": "application/json", "cache-control": "no-store", "x-error-id": errorId },
    });
  }
}
