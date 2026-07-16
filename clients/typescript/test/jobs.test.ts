import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { Client, ApiError } from "../src/index";
import { waitForJob, cancelJob, JobWaitAbortedError, JOB_STATUS_SUCCEEDED, JOB_STATUS_CANCELLED } from "../src/jobs";

function fakeFetch(handler: (url: string, init?: RequestInit) => Response): typeof globalThis.fetch {
  return (async (url: string | URL | Request, init?: RequestInit) => handler(String(url), init)) as typeof globalThis.fetch;
}

describe("waitForJob", () => {
  it("polls until succeeded and returns the result", async () => {
    let calls = 0;
    const c = new Client("http://gw.test", {
      fetch: fakeFetch(() => {
        calls++;
        const status = calls >= 3 ? "succeeded" : "queued";
        return new Response(JSON.stringify({ job_id: "job-1", status, result: { answer: 42 } }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }),
    });

    const job = await waitForJob(c, "job-1", { pollIntervalMs: 1 });
    assert.equal(job.status, JOB_STATUS_SUCCEEDED);
    assert.ok(calls >= 3, `expected at least 3 polls, got ${calls}`);
  });

  it("maps a failed job to ApiError with the job's code/message", async () => {
    const c = new Client("http://gw.test", {
      fetch: fakeFetch(
        () =>
          new Response(
            JSON.stringify({
              job_id: "job-1",
              status: "failed",
              error: { code: "WORKER_BAD_RESPONSE", message: "boom" },
            }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          ),
      ),
    });

    await assert.rejects(
      waitForJob(c, "job-1", { pollIntervalMs: 1 }),
      (err: unknown) => {
        assert.ok(err instanceof ApiError);
        assert.equal(err.code, "WORKER_BAD_RESPONSE");
        assert.equal(err.message, "boom");
        return true;
      },
    );
  });

  it("falls back to UNKNOWN/job failed when the error field is malformed", async () => {
    const c = new Client("http://gw.test", {
      fetch: fakeFetch(
        () =>
          new Response(JSON.stringify({ job_id: "job-1", status: "failed" }), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          }),
      ),
    });

    await assert.rejects(waitForJob(c, "job-1", { pollIntervalMs: 1 }), (err: unknown) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.code, "UNKNOWN");
      return true;
    });
  });

  it("stops polling when the caller's signal aborts", async () => {
    let calls = 0;
    const c = new Client("http://gw.test", {
      fetch: fakeFetch(() => {
        calls++;
        return new Response(JSON.stringify({ job_id: "job-1", status: "queued" }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }),
    });

    const controller = new AbortController();
    setTimeout(() => controller.abort(), 20);

    await assert.rejects(waitForJob(c, "job-1", { pollIntervalMs: 5, signal: controller.signal }));

    const callsAtAbort = calls;
    await new Promise((resolve) => setTimeout(resolve, 30));
    assert.equal(calls, callsAtAbort, "polling continued after abort");
  });

  it("stops polling when timeoutMs elapses", async () => {
    const c = new Client("http://gw.test", {
      fetch: fakeFetch(
        () =>
          new Response(JSON.stringify({ job_id: "job-1", status: "queued" }), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          }),
      ),
    });

    const start = Date.now();
    await assert.rejects(
      waitForJob(c, "job-1", { pollIntervalMs: 5, timeoutMs: 20 }),
      (err: unknown) => err instanceof JobWaitAbortedError,
    );
    assert.ok(Date.now() - start < 1000, "waitForJob did not respect timeoutMs");
  });

  it("propagates a getJob HTTP error", async () => {
    const c = new Client("http://gw.test", {
      fetch: fakeFetch(
        () =>
          new Response(JSON.stringify({ error: { code: "NOT_FOUND", message: "job not found" } }), {
            status: 404,
            headers: { "Content-Type": "application/json" },
          }),
      ),
    });

    await assert.rejects(waitForJob(c, "missing"), (err: unknown) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.code, "NOT_FOUND");
      return true;
    });
  });

  it("polls until cancelled and resolves (does not reject)", async () => {
    let calls = 0;
    const c = new Client("http://gw.test", {
      fetch: fakeFetch(() => {
        calls++;
        const status = calls >= 3 ? "cancelled" : "queued";
        return new Response(JSON.stringify({ job_id: "job-1", status }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }),
    });

    const job = await waitForJob(c, "job-1", { pollIntervalMs: 1 });
    assert.equal(job.status, JOB_STATUS_CANCELLED);
    assert.ok(calls >= 3, `expected at least 3 polls, got ${calls}`);
  });
});

describe("cancelJob", () => {
  it("posts to /v1/jobs/{id}/cancel and returns the cancelled job", async () => {
    let capturedURL = "";
    let capturedMethod = "";
    const c = new Client("http://gw.test", {
      apiKey: "default-key",
      fetch: fakeFetch((url, init) => {
        capturedURL = url;
        capturedMethod = init?.method ?? "";
        return new Response(JSON.stringify({ job_id: "job-1", status: "cancelled" }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }),
    });

    const job = await cancelJob(c, "job-1");
    assert.equal(job.status, JOB_STATUS_CANCELLED);
    assert.equal(job.job_id, "job-1");
    assert.equal(capturedMethod, "POST");
    assert.equal(capturedURL, "http://gw.test/v1/jobs/job-1/cancel");
  });

  it("uses the Client's default API key when none is passed", async () => {
    let capturedHeaders: RequestInit["headers"];
    const c = new Client("http://gw.test", {
      apiKey: "default-key",
      fetch: fakeFetch((_url, init) => {
        capturedHeaders = init?.headers;
        return new Response(JSON.stringify({ job_id: "job-1", status: "cancelled" }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }),
    });

    await cancelJob(c, "job-1");
    const headers = capturedHeaders as Record<string, string>;
    assert.equal(headers["X-API-Key"], "default-key");
  });

  it("propagates a 409 as ApiError with the job's stable code", async () => {
    const c = new Client("http://gw.test", {
      fetch: fakeFetch(
        () =>
          new Response(
            JSON.stringify({
              error: { code: "JOB_NOT_CANCELLABLE", message: "job cannot be cancelled in its current state" },
            }),
            { status: 409, headers: { "Content-Type": "application/json" } },
          ),
      ),
    });

    await assert.rejects(cancelJob(c, "job-1"), (err: unknown) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.code, "JOB_NOT_CANCELLABLE");
      return true;
    });
  });

  it("propagates a 404 as ApiError", async () => {
    const c = new Client("http://gw.test", {
      fetch: fakeFetch(
        () =>
          new Response(JSON.stringify({ error: { code: "NOT_FOUND", message: "job not found" } }), {
            status: 404,
            headers: { "Content-Type": "application/json" },
          }),
      ),
    });

    await assert.rejects(cancelJob(c, "missing"), (err: unknown) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.code, "NOT_FOUND");
      return true;
    });
  });
});
