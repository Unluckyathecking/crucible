// jobs.ts provides waitForJob, a poll helper for Crucible async jobs. It
// complements the generated client in client.ts (same package) —
// hand-maintained, NOT written by scripts/gen-clients.sh, exactly like
// webhook.ts.
import { ApiError } from "./errors";
import type { Client, GetJobResponse } from "./client";

/** Terminal status values returned by getJob. Mirror the gateway's
 * asyncJobResponse.status wire contract (gateway/internal/server/routes.go) —
 * part of the frozen contract, changes only alongside it. */
export const JOB_STATUS_SUCCEEDED = "succeeded";
export const JOB_STATUS_FAILED = "failed";

/** Default delay between getJob polls, used when WaitForJobOptions.pollIntervalMs is omitted. */
export const DEFAULT_POLL_INTERVAL_MS = 1000;

/** Thrown by waitForJob when the caller's signal aborts or timeoutMs elapses before the job reaches a terminal status. */
export class JobWaitAbortedError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "JobWaitAbortedError";
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

export interface WaitForJobOptions {
  /** Delay between getJob polls, in ms. Defaults to DEFAULT_POLL_INTERVAL_MS. */
  pollIntervalMs?: number;
  /** Total time budget for the wait, in ms. Omit for no additional timeout. */
  timeoutMs?: number;
  /** Caller-supplied cancellation signal; aborting it stops polling immediately. */
  signal?: AbortSignal;
  /** Override the default API key for the underlying getJob calls. */
  apiKey?: string;
}

// mergeAbortSignals returns a signal that aborts as soon as any input signal
// aborts. Implemented manually (not AbortSignal.any) because that API landed
// in Node 20; this SDK's engines floor is Node >=18 (package.json).
function mergeAbortSignals(signals: AbortSignal[]): AbortSignal {
  const controller = new AbortController();
  for (const s of signals) {
    if (s.aborted) {
      controller.abort(s.reason);
      break;
    }
    s.addEventListener("abort", () => controller.abort(s.reason), { once: true });
  }
  return controller.signal;
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(signal.reason);
      return;
    }
    const onAbort = () => {
      clearTimeout(timer);
      reject(signal.reason);
    };
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

// jobErrorToApiError maps GetJobResponse.error (the gateway's asyncJobError
// JSON shape: {"code":"...","message":"..."}) to the SDK's typed ApiError, so
// a failed job surfaces through the same error type as an HTTP-level failure
// rather than a raw status string. Falls back to a generic description if the
// field is missing or shaped unexpectedly.
function jobErrorToApiError(raw: unknown): ApiError {
  const obj = (raw && typeof raw === "object" ? raw : {}) as { code?: unknown; message?: unknown };
  const code = typeof obj.code === "string" && obj.code ? obj.code : "UNKNOWN";
  const message = typeof obj.message === "string" && obj.message ? obj.message : "job failed";
  return new ApiError(0, code, message);
}

/**
 * waitForJob polls client.getJob(jobId) until the job reaches a terminal
 * status, options.signal aborts, or options.timeoutMs elapses — whichever
 * comes first. On "succeeded" it resolves with the job's final
 * GetJobResponse. On "failed" it rejects with an ApiError built from the
 * job's recorded error code/message. No new HTTP route is introduced: every
 * poll is a plain getJob call.
 */
export async function waitForJob(
  client: Client,
  jobId: string,
  options: WaitForJobOptions = {},
): Promise<GetJobResponse> {
  const pollIntervalMs = options.pollIntervalMs ?? DEFAULT_POLL_INTERVAL_MS;

  const signals: AbortSignal[] = [];
  if (options.signal) signals.push(options.signal);
  let timeoutTimer: ReturnType<typeof setTimeout> | undefined;
  if (options.timeoutMs !== undefined) {
    const timeoutController = new AbortController();
    timeoutTimer = setTimeout(
      () => timeoutController.abort(new JobWaitAbortedError(`waitForJob: timed out after ${options.timeoutMs}ms`)),
      options.timeoutMs,
    );
    signals.push(timeoutController.signal);
  }
  const combined = signals.length > 0 ? mergeAbortSignals(signals) : new AbortController().signal;

  try {
    for (;;) {
      if (combined.aborted) {
        throw combined.reason ?? new JobWaitAbortedError("waitForJob: aborted");
      }
      const job = await client.getJob(jobId, options.apiKey);
      if (job.status === JOB_STATUS_SUCCEEDED) {
        return job;
      }
      if (job.status === JOB_STATUS_FAILED) {
        throw jobErrorToApiError(job.error);
      }
      await sleep(pollIntervalMs, combined);
    }
  } finally {
    if (timeoutTimer !== undefined) clearTimeout(timeoutTimer);
  }
}
