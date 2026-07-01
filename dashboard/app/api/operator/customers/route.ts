import { jsonResponse, operatorErrorResponse, requireOperatorSession } from "../_lib/guard";
import { listCustomers } from "@/lib/operator/client";

export async function GET(request: Request): Promise<Response> {
  const unauthorized = await requireOperatorSession();
  if (unauthorized) return unauthorized;

  const url = new URL(request.url);
  const pageParam = url.searchParams.get("page");
  const perPageParam = url.searchParams.get("per_page");

  try {
    const data = await listCustomers({
      planId: url.searchParams.get("plan_id") ?? undefined,
      page: pageParam ? Number(pageParam) : undefined,
      perPage: perPageParam ? Number(perPageParam) : undefined,
    });
    return jsonResponse(data);
  } catch (err) {
    return operatorErrorResponse(err);
  }
}
