import { cookies } from "next/headers";
import { OPERATOR_SESSION_COOKIE, verifyOperatorSession } from "@/lib/operator/session";
import { OperatorApiError } from "@/lib/operator/client";

// requireOperatorSession re-checks the operator session cookie inside the route
// handler even though middleware.ts already gates /api/operator/* — defense in
// depth, matching the existing /api/keys convention of re-verifying auth() despite
// middleware coverage. Returns a 401 Response to short-circuit, or null to proceed.
export async function requireOperatorSession(): Promise<Response | null> {
  const store = await cookies();
  const authorized = await verifyOperatorSession(store.get(OPERATOR_SESSION_COOKIE)?.value);
  if (!authorized) {
    return new Response(JSON.stringify({ error: "Unauthorized" }), {
      status: 401,
      headers: { "content-type": "application/json", "cache-control": "no-store" },
    });
  }
  return null;
}

export function jsonResponse(data: unknown): Response {
  return new Response(JSON.stringify(data), {
    headers: { "content-type": "application/json", "cache-control": "no-store" },
  });
}

// operatorErrorResponse maps OperatorApiError (gateway rejection or shape
// mismatch) to its status/message; any other error is logged server-side with an
// opaque id and reported as a generic 500 — no stack traces or internal details
// reach the client.
export function operatorErrorResponse(err: unknown): Response {
  if (err instanceof OperatorApiError) {
    const status = err.status >= 400 && err.status < 600 ? err.status : 502;
    return new Response(JSON.stringify({ error: err.message }), {
      status,
      headers: { "content-type": "application/json", "cache-control": "no-store" },
    });
  }
  const errorId = crypto.randomUUID();
  console.error("operator proxy route failed:", { errorId, error: err instanceof Error ? err.message : String(err) });
  return new Response(JSON.stringify({ error: "Internal server error" }), {
    status: 500,
    headers: { "content-type": "application/json", "cache-control": "no-store", "x-error-id": errorId },
  });
}
