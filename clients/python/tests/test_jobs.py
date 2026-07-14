"""Hand-maintained — NOT written by scripts/gen-clients.sh (mirrors
clients/go/jobs_test.go and clients/typescript/test/jobs.test.ts)."""
from __future__ import annotations

import threading
import time

import pytest

from crucible_client import ApiError, Client
from crucible_client.jobs import JobWaitCancelledError, wait_for_job

from test_client import json_response, serve


def test_wait_for_job_succeeds():
    calls = {"n": 0}

    def handler(req):
        assert req.method == "GET"
        assert req.path.split("?")[0] == "/v1/jobs/job-1"
        calls["n"] += 1
        status = "succeeded" if calls["n"] >= 3 else "queued"
        return json_response(200, {"job_id": "job-1", "status": status, "result": {"answer": 42}})

    with serve(handler) as (base_url, _captured):
        c = Client(base_url)
        job = wait_for_job(c, "job-1", poll_interval=0.01)
        assert job["status"] == "succeeded"
        assert calls["n"] >= 3


def test_wait_for_job_failed_maps_to_api_error():
    def handler(req):
        return json_response(
            200,
            {"job_id": "job-1", "status": "failed", "error": {"code": "WORKER_BAD_RESPONSE", "message": "boom"}},
        )

    with serve(handler) as (base_url, _captured):
        c = Client(base_url)
        with pytest.raises(ApiError) as exc_info:
            wait_for_job(c, "job-1", poll_interval=0.01)
        assert exc_info.value.code == "WORKER_BAD_RESPONSE"
        assert exc_info.value.message == "boom"


def test_wait_for_job_failed_with_malformed_error_falls_back():
    def handler(req):
        return json_response(200, {"job_id": "job-1", "status": "failed"})

    with serve(handler) as (base_url, _captured):
        c = Client(base_url)
        with pytest.raises(ApiError) as exc_info:
            wait_for_job(c, "job-1", poll_interval=0.01)
        assert exc_info.value.code == "UNKNOWN"


def test_wait_for_job_times_out():
    def handler(req):
        return json_response(200, {"job_id": "job-1", "status": "queued"})

    with serve(handler) as (base_url, _captured):
        c = Client(base_url)
        start = time.monotonic()
        with pytest.raises(TimeoutError):
            wait_for_job(c, "job-1", poll_interval=0.01, timeout=0.05)
        assert time.monotonic() - start < 2.0


def test_wait_for_job_cancelled():
    calls = {"n": 0}

    def handler(req):
        calls["n"] += 1
        return json_response(200, {"job_id": "job-1", "status": "queued"})

    with serve(handler) as (base_url, _captured):
        c = Client(base_url)
        cancel_event = threading.Event()
        timer = threading.Timer(0.05, cancel_event.set)
        timer.start()
        try:
            with pytest.raises(JobWaitCancelledError):
                wait_for_job(c, "job-1", poll_interval=0.01, cancel_event=cancel_event)
        finally:
            timer.cancel()

        calls_at_cancel = calls["n"]
        time.sleep(0.1)
        assert calls["n"] == calls_at_cancel


def test_wait_for_job_propagates_get_job_error():
    def handler(req):
        return json_response(404, {"error": {"code": "NOT_FOUND", "message": "job not found"}})

    with serve(handler) as (base_url, _captured):
        c = Client(base_url)
        with pytest.raises(ApiError) as exc_info:
            wait_for_job(c, "missing")
        assert exc_info.value.code == "NOT_FOUND"
