"""jobs.py provides wait_for_job, a poll helper for Crucible async jobs.
Hand-maintained — NOT written by scripts/gen-clients.sh (mirrors
clients/go/jobs.go and clients/typescript/src/jobs.ts, which are also
excluded from their respective generators' write scope).
"""
from __future__ import annotations

import threading
import time
from typing import Any, Optional

from .client import Client, GetJobResponse
from .errors import ApiError

#: Terminal status values returned by Client.get_job. Mirror the gateway's
#: asyncJobResponse.status wire contract (gateway/internal/server/routes.go) —
#: part of the frozen contract, changes only alongside it.
JOB_STATUS_SUCCEEDED = "succeeded"
JOB_STATUS_FAILED = "failed"

#: Default delay between get_job polls, in seconds, used when wait_for_job's
#: poll_interval is left at its default.
DEFAULT_POLL_INTERVAL = 1.0


class JobWaitCancelledError(Exception):
    """Raised by wait_for_job when cancel_event is set before the job reaches a terminal status."""


def _job_error_to_api_error(raw: Any) -> ApiError:
    """Maps GetJobResponse["error"] (the gateway's asyncJobError JSON shape:
    {"code":"...","message":"..."}) to the SDK's typed ApiError, so a failed
    job surfaces through the same error type as an HTTP-level failure rather
    than a raw status string. Falls back to a generic description if the
    field is missing or shaped unexpectedly.
    """
    obj = raw if isinstance(raw, dict) else {}
    code = obj.get("code") if isinstance(obj.get("code"), str) and obj.get("code") else "UNKNOWN"
    message = obj.get("message") if isinstance(obj.get("message"), str) and obj.get("message") else "job failed"
    return ApiError(code, message, False, "", 0)


def wait_for_job(
    client: Client,
    job_id: str,
    api_key: Optional[str] = None,
    poll_interval: float = DEFAULT_POLL_INTERVAL,
    timeout: Optional[float] = None,
    cancel_event: Optional[threading.Event] = None,
) -> GetJobResponse:
    """Polls client.get_job(job_id) until the job reaches a terminal status,
    cancel_event is set, or timeout seconds elapse — whichever comes first.

    On "succeeded" returns the job's final GetJobResponse. On "failed" raises
    an ApiError built from the job's recorded error code/message. Raises
    TimeoutError if timeout elapses first, or JobWaitCancelledError if
    cancel_event is set first. No new HTTP route is introduced: every poll is
    a plain get_job call.
    """
    deadline = time.monotonic() + timeout if timeout is not None else None

    while True:
        if cancel_event is not None and cancel_event.is_set():
            raise JobWaitCancelledError("wait_for_job: cancelled")

        job = client.get_job(job_id, api_key)
        status = job.get("status")
        if status == JOB_STATUS_SUCCEEDED:
            return job
        if status == JOB_STATUS_FAILED:
            raise _job_error_to_api_error(job.get("error"))

        remaining = poll_interval
        if deadline is not None:
            time_left = deadline - time.monotonic()
            if time_left <= 0:
                raise TimeoutError(f"wait_for_job: timed out after {timeout}s")
            remaining = min(remaining, time_left)

        if cancel_event is not None:
            if cancel_event.wait(remaining):
                raise JobWaitCancelledError("wait_for_job: cancelled")
        else:
            time.sleep(remaining)
