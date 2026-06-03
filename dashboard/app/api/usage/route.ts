// GET /api/usage — returns raw usage events {operation, billable_units, created_at}
// for the authenticated customer over the requested time window.
import { randomUUID } from "crypto";
import { auth } from "@/auth";
import { ensureCustomer, listUsageEvents } from "@/lib/db";

const DEFAULT_DAYS = 30;
const MAX_RANGE_DAYS = 90;
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

    const customer = await ensureCustomer(session.user.email);

    const url = new URL(request.url);
    const fromParam = url.searchParams.get("from");
    const toParam = url.searchParams.get("to");
    const operationRaw = url.searchParams.get("operation");
    if (operationRaw !== null && operationRaw.length > 128) {
      return new Response(JSON.stringify({ error: "operation parameter too long" }), {
        status: 400,
        headers: { "content-type": "application/json" },
      });
    }
    const operationParam = operationRaw || undefined;

    const now = new Date();
    let from = new Date(now.getTime() - DEFAULT_DAYS * 24 * 60 * 60 * 1000);
    let to = now;

    if (fromParam) {
      if (!ISO_DATE_RE.test(fromParam)) {
        return new Response(JSON.stringify({ error: "invalid 'from' date, expected ISO 8601" }), {
          status: 400,
          headers: { "content-type": "application/json" },
        });
      }
      const parsed = new Date(fromParam);
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
      const parsed = new Date(toParam);
      if (isNaN(parsed.getTime()) || parsed.toISOString().slice(0, 10) !== toParam) {
        return new Response(JSON.stringify({ error: "invalid 'to' date" }), {
          status: 400,
          headers: { "content-type": "application/json" },
        });
      }
      to = parsed;
    }

    if (from > to) {
      return new Response(JSON.stringify({ error: "'from' must not be after 'to'" }), {
        status: 400,
        headers: { "content-type": "application/json" },
      });
    }
    if (to.getTime() - from.getTime() > MAX_RANGE_DAYS * 24 * 60 * 60 * 1000) {
      return new Response(JSON.stringify({ error: `date range exceeds maximum of ${MAX_RANGE_DAYS} days` }), {
        status: 400,
        headers: { "content-type": "application/json" },
      });
    }

    const rows = await listUsageEvents(customer.id, from, to, operationParam);

    return new Response(JSON.stringify(rows), {
      headers: { "content-type": "application/json", "cache-control": "no-store" },
    });
  } catch (err) {
    const errorId = randomUUID();
    console.error("GET /api/usage failed:", {
      errorId,
      error: err instanceof Error ? err.message : String(err),
    });
    return new Response(JSON.stringify({ error: "Internal server error" }), {
      status: 500,
      headers: { "content-type": "application/json", "x-error-id": errorId },
    });
  }
}
