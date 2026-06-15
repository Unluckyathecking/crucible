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
// Validate operation (/v1/echo) and code (RATE_LIMITED) at the boundary.
const OPERATION_FILTER_RE = /^\/(?:[a-zA-Z0-9_-]+\/)*[a-zA-Z0-9_-]+$/;
const CODE_FILTER_RE = /^[A-Z0-9_]{1,128}$/;
// Gateway already truncates stored payloads at ErrorPayloadMaxBytes (default 4 KiB).
// This 8 KiB display cap is higher so it never silently truncates operator-enlarged limits.
const MAX_PAYLOAD_DISPLAY_BYTES = 8192;


function parseISODate(s: string): Date | null {
  if (!ISO_DATE_RE.test(s)) return null;
  const d = new Date(s + ISO_MIDNIGHT_SUFFIX);
  if (isNaN(d.getTime())) return null;
  // Round-trip check catches calendar overflow ("2023-02-30" → "2023-03-02").
  // toISOString() always returns UTC; slicing the first 10 chars gives the
  // UTC date string for comparison — correct because d was constructed at UTC midnight.
  if (d.toISOString().slice(0, 10) !== s) return null;
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
  // BYTEA from postgres; null when capture is off.
  request_payload: Buffer | null;
}

type SerializedErrorEvent = Omit<ErrorEventRow, "created_at" | "request_payload"> & {
  created_at: string;
  request_payload: string | null;
};

async function listErrorEvents(
  customerId: string,
  from: Date,
  toExclusive: Date,
  operation: string | undefined,
  code: string | undefined,
  offset: number,
  limit: number,
): Promise<{ data: SerializedErrorEvent[]; has_more: boolean }> {
  const sqlLimit = limit + 1;
  const r = await pool.query<ErrorEventRow>(
    `SELECT id, operation, error_code, http_status,
            message, request_id, created_at, request_payload
     FROM error_events
     WHERE customer_id = $1
       AND created_at >= $2
       AND created_at < $3
       AND ($4::text IS NULL OR operation = $4)
       AND ($5::text IS NULL OR error_code = $5)
     ORDER BY created_at DESC
     LIMIT $6 OFFSET $7`,
    [
      customerId,
      from,
      toExclusive,
      operation ?? null,
      code ?? null,
      sqlLimit,
      offset,
    ],
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
    request_payload: row.request_payload
      ? row.request_payload.toString("utf8", 0, MAX_PAYLOAD_DISPLAY_BYTES)
      : null,
  }));
  return { data: rows, has_more: hasMore };
}

const noStore = { "content-type": "application/json", "cache-control": "no-store" } as const;

export async function GET(request: Request): Promise<Response> {
  // Defense-in-depth CSRF check: a cross-origin browser fetch cannot set
  // X-Requested-With without a CORS preflight, and this route does not send
  // CORS headers, so the preflight would be denied before the request reaches
  // here. The primary CSRF protection is the NextAuth session cookie (SameSite).
  // This header check is an additional layer; it is not bypassable via a
  // standard cross-origin request from a web page.
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
    const customer = await ensureCustomer(session.user.email);
    const url = new URL(request.url);

    // Date-range defaults: [tomorrowMidnight − 30 days, tomorrowMidnight).
    // This is identical to the /api/usage default: 30 calendar days inclusive.
    const now = new Date();
    // Date.UTC handles day-of-month overflow correctly (e.g. d+1 on the last
    // day of a month rolls to the 1st of the next month), making this safer
    // than adding a fixed 86_400_000ms offset.
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
      // next midnight for the exclusive DB bound using date-overflow-safe math.
      const d = parsed;
      toExclusive = new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate() + 1));
    }
    if (toExclusive.getTime() > tomorrowMidnight.getTime()) {
      return new Response(JSON.stringify({ error: "'to' date must not be in the future" }), {
        status: 400,
        headers: noStore,
      });
    }
    // Range validation compares the user-visible bounds directly.
    // userVisibleToMs is the inclusive `to` date (toExclusive minus one day).
    const userVisibleToMs = toExclusive.getTime() - MS_PER_DAY;
    if (from.getTime() > userVisibleToMs) {
      return new Response(JSON.stringify({ error: "'from' must not be after 'to'" }), {
        status: 400,
        headers: noStore,
      });
    }
    const userVisibleRangeMs = userVisibleToMs - from.getTime();
    if (userVisibleRangeMs < 0 || userVisibleRangeMs > MAX_RANGE_DAYS * MS_PER_DAY) {
      return new Response(
        JSON.stringify({ error: `date range exceeds maximum of ${MAX_RANGE_DAYS} days` }),
        { status: 400, headers: noStore },
      );
    }

    const operationRaw = url.searchParams.get("operation");
    let operation: string | undefined;
    if (operationRaw && operationRaw.trim().length > 0) {
      const trimmed = operationRaw.trim();
      if (trimmed.length > MAX_FILTER_LENGTH || !OPERATION_FILTER_RE.test(trimmed)) {
        return new Response(
          JSON.stringify({ error: `invalid 'operation' filter: must be a /v1/... path (max ${MAX_FILTER_LENGTH} chars)` }),
          { status: 400, headers: noStore },
        );
      }
      operation = trimmed;
    }

    const codeRaw = url.searchParams.get("code");
    let code: string | undefined;
    if (codeRaw && codeRaw.trim().length > 0) {
      const trimmed = codeRaw.trim();
      if (trimmed.length > MAX_FILTER_LENGTH || !CODE_FILTER_RE.test(trimmed)) {
        return new Response(
          JSON.stringify({ error: `invalid 'code' filter: must be uppercase letters, digits, and underscores (max ${MAX_FILTER_LENGTH} chars)` }),
          { status: 400, headers: noStore },
        );
      }
      code = trimmed;
    }

    const pageStr = url.searchParams.get("page") ?? "1";
    const pageRaw = /^\d+$/.test(pageStr) ? parseInt(pageStr, 10) : NaN;
    const page = Number.isFinite(pageRaw) && pageRaw >= 1 ? pageRaw : 1;

    const limitStr = url.searchParams.get("limit") ?? String(DEFAULT_PAGE_SIZE);
    const limitRaw = /^\d+$/.test(limitStr) ? parseInt(limitStr, 10) : NaN;
    const limit =
      Number.isFinite(limitRaw) && limitRaw >= 1
        ? Math.min(limitRaw, MAX_PAGE_SIZE)
        : DEFAULT_PAGE_SIZE;

    const offset = (page - 1) * limit;
    if (!Number.isSafeInteger(offset) || offset > 10_000_000) {
      return new Response(JSON.stringify({ error: "page too large" }), {
        status: 400,
        headers: noStore,
      });
    }

    const result = await listErrorEvents(customer.id, from, toExclusive, operation, code, offset, limit);
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
