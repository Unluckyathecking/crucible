import { jsonResponse, operatorErrorResponse, requireOperatorSession } from "../../../_lib/guard";
import { getCustomerUsage } from "@/lib/operator/client";

export async function GET(request: Request, context: { params: Promise<{ id: string }> }): Promise<Response> {
  const unauthorized = await requireOperatorSession();
  if (unauthorized) return unauthorized;

  const { id } = await context.params;
  const url = new URL(request.url);
  try {
    const data = await getCustomerUsage(id, {
      start: url.searchParams.get("start") ?? undefined,
      end: url.searchParams.get("end") ?? undefined,
    });
    return jsonResponse(data);
  } catch (err) {
    return operatorErrorResponse(err);
  }
}
