"use server";

import { redirect } from "next/navigation";
import { revalidatePath } from "next/cache";
import { OperatorApiError, replayDeadLetter, replayEndpointDeadLetters } from "@/lib/operator/client";

// replayDeadLetterAction and replayEndpointAction both redirect back to
// /operator/webhooks on success (a plain server-action return would leave the
// browser on a POST response) and, on a caller-fixable error (400 invalid id,
// 404 unknown delivery, 409 inactive endpoint), redirect with ?error= so the
// page can show it inline, mirroring the jobs actions' ?error= convention.
// Anything else re-throws to the error boundary.
export async function replayDeadLetterAction(formData: FormData): Promise<void> {
  const id = (formData.get("id") as string | null)?.trim() ?? "";
  let requeued: number;
  try {
    requeued = (await replayDeadLetter(id)).requeued;
  } catch (err) {
    if (err instanceof OperatorApiError && (err.status === 400 || err.status === 404 || err.status === 409)) {
      redirect(`/operator/webhooks?error=${encodeURIComponent(err.message)}`);
    }
    throw err;
  }
  revalidatePath("/operator/webhooks");
  redirect(`/operator/webhooks?replayed=${requeued}`);
}

export async function replayEndpointAction(formData: FormData): Promise<void> {
  const endpointId = (formData.get("endpoint_id") as string | null)?.trim() ?? "";
  let requeued: number;
  try {
    requeued = (await replayEndpointDeadLetters(endpointId)).requeued;
  } catch (err) {
    if (err instanceof OperatorApiError && err.status === 400) {
      redirect(`/operator/webhooks?error=${encodeURIComponent(err.message)}`);
    }
    throw err;
  }
  revalidatePath("/operator/webhooks");
  redirect(`/operator/webhooks?replayed=${requeued}`);
}
