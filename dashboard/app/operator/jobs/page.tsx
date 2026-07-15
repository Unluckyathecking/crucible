import Link from "next/link";
import { OperatorNav } from "../_components/operator-nav";
import { Pagination } from "../_components/pagination";
import { releaseJobsAction, requeueJobAction } from "./actions";
import { OperatorApiError, listAdminJobs } from "@/lib/operator/client";
import type { AdminJob, Page } from "@/lib/operator/client";

export const dynamic = "force-dynamic";

const PER_PAGE = 25;

// Requeuing a job that's actually still running risks a second, concurrent
// execution (see gateway/internal/jobs.Store.Requeue's doc comment) — only
// offer it for statuses where that can't still be true.
const REQUEUEABLE_STATUSES = new Set(["running", "failed"]);

interface JobsPageProps {
  searchParams: Promise<{
    status?: string;
    page?: string;
    error?: string;
    released?: string;
  }>;
}

export default async function OperatorJobsPage({ searchParams }: JobsPageProps) {
  const params = await searchParams;
  const page = Math.max(1, Number(params.page) || 1);

  let jobs: Page<AdminJob> | null = null;
  let filterError: string | null = null;
  try {
    jobs = await listAdminJobs({ status: params.status, page, perPage: PER_PAGE });
  } catch (err) {
    // Only the gateway's documented ?status= validation failure is a
    // user-fixable input error worth showing inline, same treatment as the
    // audit page's filter errors. Anything else re-throws to the error boundary.
    if (err instanceof OperatorApiError && err.status === 400) {
      filterError = err.message;
    } else {
      throw err;
    }
  }

  return (
    <main id="main-content" className="min-h-screen px-4 py-6 sm:px-6 sm:py-8 md:px-8">
      <div className="mx-auto w-full max-w-6xl">
        <OperatorNav />
        <h1 className="text-2xl font-bold mb-4">Jobs</h1>

        {params.error && (
          <p className="text-sm text-red-600 dark:text-red-400 mb-4" role="alert">
            {params.error}
          </p>
        )}
        {params.released !== undefined && (
          <p className="text-sm text-green-700 dark:text-green-400 mb-4" role="status">
            Released {params.released} job(s).
          </p>
        )}

        <form method="GET" className="flex items-end gap-3 mb-4 text-sm">
          <div className="flex flex-col gap-1">
            <label htmlFor="status" className="text-zinc-600 dark:text-zinc-400">
              Status
            </label>
            <select
              id="status"
              name="status"
              defaultValue={params.status ?? ""}
              className="px-2 py-1 border border-zinc-300 dark:border-zinc-600 rounded bg-transparent"
            >
              <option value="">All</option>
              <option value="queued">Queued</option>
              <option value="running">Running</option>
              <option value="succeeded">Succeeded</option>
              <option value="failed">Failed</option>
            </select>
          </div>
          <button type="submit" className="px-3 py-1 border border-zinc-300 dark:border-zinc-600 rounded">
            Filter
          </button>
        </form>

        <details className="mb-6 text-sm border border-zinc-200 dark:border-zinc-700 rounded-lg p-3">
          <summary className="cursor-pointer font-medium">Release jobs from a dead instance</summary>
          <p className="text-zinc-600 dark:text-zinc-400 mt-2 mb-2">
            Force-releases every job still claimed by the given gateway instance back to queued. Only do this once you have
            positively confirmed that instance is dead — releasing a still-live instance&apos;s claimed jobs risks a second,
            concurrent execution of work that instance is still processing.
          </p>
          <form action={releaseJobsAction} className="flex items-end gap-3">
            <div className="flex flex-col gap-1 flex-1 max-w-md">
              <label htmlFor="instance_id" className="text-zinc-600 dark:text-zinc-400">
                Instance ID
              </label>
              <input
                id="instance_id"
                name="instance_id"
                required
                placeholder="00000000-0000-0000-0000-000000000000"
                className="px-2 py-1 border border-zinc-300 dark:border-zinc-600 rounded bg-transparent"
              />
            </div>
            <button type="submit" className="px-3 py-1 border border-zinc-300 dark:border-zinc-600 rounded">
              Release
            </button>
          </form>
        </details>

        {filterError ? (
          <p className="text-sm text-red-600 dark:text-red-400" role="alert">
            {filterError}{" "}
            <Link href="/operator/jobs" className="underline">
              Reset filters
            </Link>
          </p>
        ) : (
          jobs && (
            <>
              <div className="overflow-x-auto border border-zinc-200 dark:border-zinc-700 rounded-lg">
                <table className="w-full text-sm">
                  <thead className="bg-zinc-50 dark:bg-zinc-800 text-left">
                    <tr>
                      <th className="px-3 py-2">Job ID</th>
                      <th className="px-3 py-2">Customer</th>
                      <th className="px-3 py-2">Operation</th>
                      <th className="px-3 py-2">Status</th>
                      <th className="px-3 py-2">Claimed by</th>
                      <th className="px-3 py-2">Updated</th>
                      <th className="px-3 py-2">Action</th>
                    </tr>
                  </thead>
                  <tbody>
                    {jobs.items.map((job) => (
                      <tr key={job.job_id} className="border-t border-zinc-200 dark:border-zinc-700">
                        <td className="px-3 py-2 font-mono text-xs">{job.job_id.slice(0, 8)}…</td>
                        <td className="px-3 py-2">
                          <Link href={`/operator/customers/${job.customer_id}`} className="underline font-mono text-xs">
                            {job.customer_id.slice(0, 8)}…
                          </Link>
                        </td>
                        <td className="px-3 py-2">{job.operation}</td>
                        <td className="px-3 py-2">
                          {job.status}
                          {job.error && <div className="text-xs text-red-600 dark:text-red-400">{job.error.code}</div>}
                        </td>
                        <td className="px-3 py-2 font-mono text-xs">{job.claimed_by ? job.claimed_by.slice(0, 8) + "…" : "—"}</td>
                        <td className="px-3 py-2 whitespace-nowrap">{new Date(job.updated_at).toLocaleString()}</td>
                        <td className="px-3 py-2">
                          {REQUEUEABLE_STATUSES.has(job.status) ? (
                            <form action={requeueJobAction}>
                              <input type="hidden" name="job_id" value={job.job_id} />
                              <button
                                type="submit"
                                title="Only requeue if you've confirmed no worker is still processing this job — requeuing a still-running job risks double execution and double billing."
                                className="px-2 py-1 border border-zinc-300 dark:border-zinc-600 rounded"
                              >
                                Requeue
                              </button>
                            </form>
                          ) : (
                            "—"
                          )}
                        </td>
                      </tr>
                    ))}
                    {jobs.items.length === 0 && (
                      <tr>
                        <td className="px-3 py-4 text-zinc-500" colSpan={7}>
                          No jobs found.
                        </td>
                      </tr>
                    )}
                  </tbody>
                </table>
              </div>

              <Pagination
                basePath="/operator/jobs"
                page={page}
                perPage={PER_PAGE}
                total={jobs.total}
                searchParams={{ status: params.status }}
              />
            </>
          )
        )}
      </div>
    </main>
  );
}
