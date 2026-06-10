// GET /api/errors?from=&to=&operation=&code=&page=&limit=
// Returns the authenticated customer's own error_events rows, newest-first,
// paginated. All dates are interpreted as UTC midnight.
import { randomUUID } from "crypto";
import { Pool } from "pg";
import { auth } from "@/auth";
import { ensureCustomer } from "@/lib/db";

// Reuse the same pool created by lib/db.ts when available to avoid duplicate
// connection pools in the same process (important under Next.js dev HMR).
declare global {
  // eslint-disable-next-line no-var
  var _crucible_pool: Pool | undefined;
}
const pool: Pool =
  global._crucible_pool ?? new Pool({ connectionString: process.env.DATABASE_URL });
if (process.env.NODE_ENV !== "production") global._crucible_pool = pool;

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
  created_at: string;
}

async function listErrorEvents(
  customerId: string,
  from: Date,
  to: Date,
  operation: string | undefined,
  code: string | undefined,
  page: number,
  limit: number,
): Promise<{ data: ErrorEventRow[]; has_more: boolean }> {
  const offset = (page - 1) * limit;
  const params: unknown[] = [customerId, from, to];
  let filter = "";
  if (operation) {
    params.push(operation);
    filter += ` AND operation = $${params.length}`;
  }
  if (code) {
    params.push(code);
    filter += ` AND error_code = $${params.length}`;
  }
  // Fetch one extra row to determine has_more without a separate COUNT query.
  params.push(limit + 1, offset);
  const fetchParam = params.length - 1;
  const offsetParam = params.length;
  const r = await pool.query<{
    id: string;
    operation: string;
    error_code: string;
    http_status: number;
    message: string;
    request_id: string;
    created_at: Date;
  }>(
    `SELECT id::text AS id, operation, error_code, http_status, message, request_id, created_at
     FROM error_events
     WHERE customer_id = $1
       AND created_at >= $2
       AND created_at < $3
       ${filter}
     ORDER BY created_at DESC
     LIMIT $${fetchParam} OFFSET $${offsetParam}`,
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

export async function GET(request: Request): Promise<Response> {
  const requestedWith = request.headers.get("X-Requested-With") ?? "";
  if (requestedWith.toLowerCase() !== "xmlhttprequest") {
    return new Response(JSON.stringify({ error: "Forbidden" }), {
      status: 403,
      headers: { "content-type": "application/json" },
    });
  }
  try {
    const session = await auth();
    if (!session?.user?.email) {
      return new Response(JSON.stringify({ error: "Unauthorized" }), {
        status: 401,
        headers: { "content-type": "application/json" },
      });
    }
    const customer = await ensureCustomer(session.user.email);
    const url = new URL(request.url);

    // Date range defaults
    const now = new Date();
    const tomorrowMidnight = new Date(
      Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() + 1),
    );
    let from = new Date(tomorrowMidnight.getTime() - DEFAULT_DAYS * MS_PER_DAY);
    let to = tomorrowMidnight;

    const fromParam = url.searchParams.get("from");
    if (fromParam) {
      const parsed = parseISODate(fromParam);
      if (!parsed) {
        return new Response(JSON.stringify({ error: "invalid 'from' date, expected ISO 8601" }), {
          status: 400,
          headers: { "content-type": "application/json" },
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
    if (to.getTime() - from.getTime() > MAX_RANGE_DAYS * MS_PER_DAY) {
      return new Response(
        JSON.stringify({ error: `date range exceeds maximum of ${MAX_RANGE_DAYS} days` }),
        { status: 400, headers: { "content-type": "application/json" } },
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

    const result = await listErrorEvents(customer.id, from, to, operation, code, page, limit);
    return new Response(
      JSON.stringify({ ...result, page, limit }),
      { headers: { "content-type": "application/json", "cache-control": "no-store" } },
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
