import { auth } from "@/auth";
import { ensureCustomer, listUsageEvents } from "@/lib/db";

const DEFAULT_DAYS = 30;

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
    const operationParam = url.searchParams.get("operation") ?? undefined;

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
    return new Response(JSON.stringify({ error: "internal server error", errorId }), {
      status: 500,
      headers: { "content-type": "application/json", "x-error-id": errorId },
    });
  }
}
