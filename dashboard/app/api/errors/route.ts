// GET /api/errors?from=&to=&operation=&code=&page=&limit=
// Returns the authenticated customer's own error_events rows, newest-first,
// paginated. `from` and `to` are inclusive ISO 8601 dates (YYYY-MM-DD, UTC).
// The API converts `to` to an exclusive midnight bound internally so the query
// is [from-midnight, to+1-day-midnight) — consistent with the usage API pattern.
import { randomUUID } from "crypto";
import { auth } from "@/auth";
import { ensureCustomer, pool } from "@/lib/db";

const DEFAULT_PAGE_SIZE = 50;
const MAX_PAGE_SIZE = 200;
const DEFAULT_DAYS = 30;
const MAX_RANGE_DAYS = 90;
const MS_PER_DAY = 86_400_000;
const ISO_DATE_RE = /^\d{4}-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])$/;
const ISO_MIDNIGHT_SUFFIX = "T00:00:00.000Z";
const MAX_FILTER_LENGTH = 128;

function parseISODate(s: string): Date | null {
  if (!ISO_DATE_RE.test(s)) return null;
  const d = new Date(s + ISO_MIDNIGHT_SUFFIX);
  // Round-trip check catches calendar overflow: "2023-02-30" parses to "2023-03-02".
  if (isNaN(d.getTime()) || d.toISOString().slice(0, 10) !== s) return null;
  return d;
}

interface ErrorEventRow {
  id: string;
  operation: string;
  error_code: string;
  http_status: number;
  message: string;
  request_id: string;
  created_at: Date;
}

async function listErrorEvents(
  customerEmail: string,
  from: Date,
  toExclusive: Date,
  operation: string | undefined,
  code: string | undefined,
  page: number,
  limit: number,
): Promise<{ data: (Omit<ErrorEventRow, "created_at"> & { created_at: string })[]; has_more: boolean }> {
  const offset = (page - 1) * limit;
  // customerEmail comes from the authenticated session — not user input.
  // The subquery (SELECT id FROM customers WHERE email = $1) binds the query to the
  // authenticated user at the DB level, so no row from another customer can leak.
  // All $N placeholders use params.length evaluated immediately after each push.
  const params: unknown[] = [customerEmail, from, toExclusive];
  let filter = "";
  if (operation) {
    params.push(operation);
    filter += ` AND operation = $${params.length}`;
  }
  if (code) {
    // error_code values are always uppercase (RATE_LIMITED, etc.) in the store.
    params.push(code.toUpperCase());
    filter += ` AND error_code = $${params.length}`;
  }
  // Fetch one extra row to determine has_more without a separate COUNT query.
  params.push(limit + 1, offset);
  const r = await pool.query<ErrorEventRow>(
    `SELECT id::text AS id, operation, error_code, http_status, message, request_id, created_at
     FROM error_events
     WHERE customer_id = (SELECT id FROM customers WHERE email = $1 LIMIT 1)
       AND created_at >= $2
       AND created_at < $3
       ${filter}
     ORDER BY created_at DESC
     LIMIT $${params.length - 1} OFFSET $${params.length}`,
    params,
  );
  const hasMore = r.rows.length > limit;
  const rows = r.rows.slice(0, limit).map((row) => ({
    id: row.id,
    operation: row.operation,
    error_code: row.error_code,
    http_status: row.http_status,
    message: row.message,
    request_id: row.request_id,
    created_at: row.created_at.toISOString(),
  }));
  return { data: rows, has_more: hasMore };
}

const noStore = { "content-type": "application/json", "cache-control": "no-store" } as const;

export async function GET(request: Request): Promise<Response> {
  const requestedWith = request.headers.get("X-Requested-With") ?? "";
  if (requestedWith.toLowerCase() !== "xmlhttprequest") {
    return new Response(JSON.stringify({ error: "Forbidden" }), {
      status: 403,
      headers: noStore,
    });
  }
  try {
    const session = await auth();
    if (!session?.user?.email) {
      return new Response(JSON.stringify({ error: "Unauthorized" }), {
        status: 401,
        headers: noStore,
      });
    }
    await ensureCustomer(session.user.email);
    const url = new URL(request.url);

    // Date-range defaults: [tomorrowMidnight − 30 days, tomorrowMidnight).
    // This is identical to the /api/usage default: 30 calendar days inclusive.
    const now = new Date();
    const tomorrowMidnight = new Date(
      Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() + 1),
    );
    let from = new Date(tomorrowMidnight.getTime() - DEFAULT_DAYS * MS_PER_DAY);
    // toExclusive is the exclusive upper DB bound.  Callers pass an inclusive
    // date (e.g. "today"); we add one day here so events from the entire named
    // day are included in the results.
    let toExclusive = tomorrowMidnight;

    const fromParam = url.searchParams.get("from");
    if (fromParam) {
      const parsed = parseISODate(fromParam);
      if (!parsed) {
        return new Response(JSON.stringify({ error: "invalid 'from' date, expected ISO 8601" }), {
          status: 400,
          headers: noStore,
        });
      }
      from = parsed;
    }
    const toParam = url.searchParams.get("to");
    if (toParam) {
      const parsed = parseISODate(toParam);
      if (!parsed) {
        return new Response(JSON.stringify({ error: "invalid 'to' date, expected ISO 8601" }), {
          status: 400,
          headers: noStore,
        });
      }
      // `to` is inclusive (the full named day is included); advance to the
      // next midnight for the exclusive DB bound.
      toExclusive = new Date(parsed.getTime() + MS_PER_DAY);
    }
    // Range validation uses the user-visible window (from → toExclusive-1day).
    if (from.getTime() >= toExclusive.getTime()) {
      return new Response(JSON.stringify({ error: "'from' must not be after 'to'" }), {
        status: 400,
        headers: noStore,
      });
    }
    const userVisibleRangeMs = toExclusive.getTime() - MS_PER_DAY - from.getTime();
    if (userVisibleRangeMs > MAX_RANGE_DAYS * MS_PER_DAY) {
      return new Response(
        JSON.stringify({ error: `date range exceeds maximum of ${MAX_RANGE_DAYS} days` }),
        { status: 400, headers: noStore },
      );
    }

    // Optional filters — capped to prevent unbounded inputs.
    const operationRaw = url.searchParams.get("operation");
    const operation =
      operationRaw && operationRaw.trim().length > 0
        ? operationRaw.trim().slice(0, MAX_FILTER_LENGTH)
        : undefined;

    const codeRaw = url.searchParams.get("code");
    const code =
      codeRaw && codeRaw.trim().length > 0
        ? codeRaw.trim().slice(0, MAX_FILTER_LENGTH)
        : undefined;

    // Pagination
    const pageRaw = parseInt(url.searchParams.get("page") ?? "1", 10);
    const page = Number.isFinite(pageRaw) && pageRaw >= 1 ? pageRaw : 1;

    const limitRaw = parseInt(url.searchParams.get("limit") ?? String(DEFAULT_PAGE_SIZE), 10);
    const limit =
      Number.isFinite(limitRaw) && limitRaw >= 1
        ? Math.min(limitRaw, MAX_PAGE_SIZE)
        : DEFAULT_PAGE_SIZE;

    const result = await listErrorEvents(session.user.email, from, toExclusive, operation, code, page, limit);
    return new Response(
      JSON.stringify({ ...result, page, limit }),
      { headers: noStore },
    );
  } catch (err) {
    const errorId = randomUUID();
    console.error("GET /api/errors failed:", {
      errorId,
      error: err instanceof Error ? err.message : String(err),
      stack: err instanceof Error ? err.stack : undefined,
    });
    return new Response(JSON.stringify({ error: "Internal server error" }), {
      status: 500,
      headers: {
        "content-type": "application/json",
        "cache-control": "no-store",
        "x-error-id": errorId,
      },
    });
  }
}
