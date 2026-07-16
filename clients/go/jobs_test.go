package crucible_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	crucible "github.com/Unluckyathecking/crucible/clients/go"
)

func TestWaitForJob_succeeds(t *testing.T) {
	calls := 0
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/jobs/job-1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		calls++
		status := "queued"
		if calls >= 3 {
			status = "succeeded"
		}
		writeJSON(w, map[string]any{
			"job_id": "job-1",
			"status": status,
			"result": map[string]any{"answer": 42},
		})
	})

	resp, err := c.WaitForJob(context.Background(), "test-key", "job-1", &crucible.WaitForJobOptions{
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("WaitForJob: %v", err)
	}
	if resp.Status != "succeeded" {
		t.Errorf("Status = %q, want succeeded", resp.Status)
	}
	if calls < 3 {
		t.Errorf("calls = %d, want >= 3 (polled until terminal)", calls)
	}
}

func TestWaitForJob_cancelled(t *testing.T) {
	calls := 0
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		status := "queued"
		if calls >= 3 {
			status = "cancelled"
		}
		writeJSON(w, map[string]any{
			"job_id": "job-1",
			"status": status,
		})
	})

	resp, err := c.WaitForJob(context.Background(), "test-key", "job-1", &crucible.WaitForJobOptions{
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("WaitForJob: %v", err)
	}
	if resp.Status != crucible.JobStatusCancelled {
		t.Errorf("Status = %q, want %q", resp.Status, crucible.JobStatusCancelled)
	}
	if calls < 3 {
		t.Errorf("calls = %d, want >= 3 (polled until terminal)", calls)
	}
}

func TestWaitForJob_failed(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"job_id": "job-1",
			"status": "failed",
			"error":  map[string]any{"code": "WORKER_BAD_RESPONSE", "message": "boom"},
		})
	})

	_, err := c.WaitForJob(context.Background(), "test-key", "job-1", &crucible.WaitForJobOptions{
		PollInterval: time.Millisecond,
	})
	var jobErr *crucible.JobFailedError
	if !errors.As(err, &jobErr) {
		t.Fatalf("err = %v, want *JobFailedError", err)
	}
	if jobErr.Code != "WORKER_BAD_RESPONSE" || jobErr.Message != "boom" {
		t.Errorf("jobErr = %+v, want Code=WORKER_BAD_RESPONSE Message=boom", jobErr)
	}
}

func TestWaitForJob_failedWithMalformedError(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"job_id": "job-1",
			"status": "failed",
		})
	})

	_, err := c.WaitForJob(context.Background(), "test-key", "job-1", nil)
	var jobErr *crucible.JobFailedError
	if !errors.As(err, &jobErr) {
		t.Fatalf("err = %v, want *JobFailedError", err)
	}
	if jobErr.Code != "UNKNOWN" {
		t.Errorf("Code = %q, want UNKNOWN fallback", jobErr.Code)
	}
}

func TestWaitForJob_contextTimeoutStopsPolling(t *testing.T) {
	calls := 0
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeJSON(w, map[string]any{"job_id": "job-1", "status": "queued"})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := c.WaitForJob(ctx, "test-key", "job-1", &crucible.WaitForJobOptions{
		PollInterval: 5 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}

	callsAtTimeout := calls
	time.Sleep(30 * time.Millisecond)
	if calls != callsAtTimeout {
		t.Errorf("calls kept increasing after context deadline: %d -> %d", callsAtTimeout, calls)
	}
}

func TestWaitForJob_optionsTimeoutStopsPolling(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"job_id": "job-1", "status": "queued"})
	})

	start := time.Now()
	_, err := c.WaitForJob(context.Background(), "test-key", "job-1", &crucible.WaitForJobOptions{
		PollInterval: 5 * time.Millisecond,
		Timeout:      20 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("WaitForJob took %v, want bounded by Timeout", elapsed)
	}
}

func TestWaitForJob_getJobErrorPropagates(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "NOT_FOUND", "message": "job not found"},
		})
	})

	_, err := c.WaitForJob(context.Background(), "test-key", "missing", nil)
	var apiErr *crucible.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Code != "NOT_FOUND" {
		t.Errorf("Code = %q, want NOT_FOUND", apiErr.Code)
	}
}
