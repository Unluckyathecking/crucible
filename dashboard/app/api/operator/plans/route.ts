import { jsonResponse, operatorErrorResponse, requireOperatorSession } from "../_lib/guard";
import { listPlans } from "@/lib/operator/client";

export async function GET(): Promise<Response> {
  const unauthorized = await requireOperatorSession();
  if (unauthorized) return unauthorized;

  try {
    const data = await listPlans();
    return jsonResponse(data);
  } catch (err) {
    return operatorErrorResponse(err);
  }
}
