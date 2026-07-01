import { jsonResponse, operatorErrorResponse, requireOperatorSession } from "../../_lib/guard";
import { getCustomer } from "@/lib/operator/client";

export async function GET(_request: Request, context: { params: Promise<{ id: string }> }): Promise<Response> {
  const unauthorized = await requireOperatorSession();
  if (unauthorized) return unauthorized;

  const { id } = await context.params;
  try {
    const data = await getCustomer(id);
    return jsonResponse(data);
  } catch (err) {
    return operatorErrorResponse(err);
  }
}
