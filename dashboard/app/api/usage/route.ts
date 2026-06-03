// GET /api/usage — returns raw per-event rows {operation, billable_units, created_at}
// matching acceptance criterion: "per-row fields {operation, billable_units, created_at}".
// Per-operation aggregates (grouped by operation) are served by usageByOperation()
// used directly in the server-rendered dashboard page, not via this endpoint.
import { auth } from "@/auth";
import { ensureCustomer, listUsageEvents } from "@/lib/db";

const DEFAULT_DAYS = 30;
const MAX_RANGE_DAYS = 90;

export async function GET(request: Request): Promise<Response> {
  try {
    const session = await auth();
    if (!session?.user?.email) {
      return new Response("Unauthorized", { status: 401 });
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
    const operationParam = operationRaw ?? undefined;

    const now = new Date();
    let from = new Date(now.getTime() - DEFAULT_DAYS * 24 * 60 * 60 * 1000);
    let to = now;

    if (fromParam) {
      const parsed = new Date(fromParam);
      if (isNaN(parsed.getTime())) {
        return new Response(JSON.stringify({ error: "invalid 'from' date" }), {
          status: 400,
          headers: { "content-type": "application/json" },
        });
      }
      from = parsed;
    }
    if (toParam) {
      const parsed = new Date(toParam);
      if (isNaN(parsed.getTime())) {
        return new Response(JSON.stringify({ error: "invalid 'to' date" }), {
          status: 400,
          headers: { "content-type": "application/json" },
        });
      }
      to = parsed;
    }

    if (from >= to) {
      return new Response(JSON.stringify({ error: "'from' must be before 'to'" }), {
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
    const errorId = crypto.randomUUID();
    console.error("GET /api/usage failed:", {
      errorId,
      error: err instanceof Error ? err.message : String(err),
    });
    return new Response("Internal server error", {
      status: 500,
      headers: { "x-error-id": errorId },
    });
  }
}
