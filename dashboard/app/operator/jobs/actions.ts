"use server";

import { redirect } from "next/navigation";
import { revalidatePath } from "next/cache";
import { OperatorApiError, releaseJobs, requeueJob } from "@/lib/operator/client";

// requeueJobAction and releaseJobsAction both redirect back to /operator/jobs
// on success (a plain server-action return would leave the browser on a POST
// response) and, on a caller-fixable error (400/404 — bad id, unknown job,
// unknown instance), redirect with ?error= so the page can show it inline,
// mirroring the audit page's filterError convention. Anything else re-throws
// to the error boundary.
export async function requeueJobAction(formData: FormData): Promise<void> {
  const jobId = (formData.get("job_id") as string | null)?.trim() ?? "";
  try {
    await requeueJob(jobId);
  } catch (err) {
    if (err instanceof OperatorApiError && (err.status === 400 || err.status === 404)) {
      redirect(`/operator/jobs?error=${encodeURIComponent(err.message)}`);
    }
    throw err;
  }
  revalidatePath("/operator/jobs");
  redirect("/operator/jobs");
}

export async function releaseJobsAction(formData: FormData): Promise<void> {
  const instanceId = (formData.get("instance_id") as string | null)?.trim() ?? "";
  let released: number;
  try {
    released = (await releaseJobs(instanceId)).released;
  } catch (err) {
    if (err instanceof OperatorApiError && err.status === 400) {
      redirect(`/operator/jobs?error=${encodeURIComponent(err.message)}`);
    }
    throw err;
  }
  revalidatePath("/operator/jobs");
  redirect(`/operator/jobs?released=${released}`);
}
