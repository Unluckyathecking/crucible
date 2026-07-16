// jobs.go provides WaitForJob, a poll helper for Crucible async jobs. It
// complements the generated client in client.go (same package, crucible) —
// hand-maintained, NOT written by scripts/gen-clients.sh, exactly like
// webhook.go.
package crucible

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Terminal status values returned by GetJob. Mirror gateway/internal/jobs.Job's
// Status constants (StatusSucceeded, StatusFailed, StatusCancelled) — the
// client package cannot import the gateway's internal package, so the
// strings are duplicated here; they are part of the frozen wire contract
// (asyncJobResponse.Status in gateway/internal/server/routes.go) and change
// only alongside it.
const (
	JobStatusSucceeded = "succeeded"
	JobStatusFailed    = "failed"
	JobStatusCancelled = "cancelled"
)

// CancelJob calls POST /v1/jobs/{id}/cancel: withdraws apiKey's own
// still-queued job. Hand-maintained here rather than generated into
// client.go: scripts/gen-clients.sh emits one method per operationId
// declared in clients/openapi.json, but this file already hosts the SDK's
// other hand-maintained job helpers (WaitForJob below), and CancelJob's
// response is byte-identical in shape to GetJob's (the gateway's
// jobsCancelHandler returns the same asyncJobResponse envelope
// jobsGetHandler does), so it reuses GetJobResponse rather than
// introducing a duplicate type. Uses the same c.do/checkError primitives
// client.go's generated methods use — same package, so both are visible
// here.
func (c *Client) CancelJob(ctx context.Context, apiKey, jobID string) (*GetJobResponse, error) {
	path := fmt.Sprintf("/v1/jobs/%s/cancel", url.PathEscape(jobID))
	resp, err := c.do(ctx, http.MethodPost, path, apiKey, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkError(resp); err != nil {
		return nil, err
	}
	var out GetJobResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&out); decErr != nil {
		return nil, fmt.Errorf("crucible: decode response: %w", decErr)
	}
	return &out, nil
}

// DefaultPollInterval is used by WaitForJob when WaitForJobOptions is nil or
// its PollInterval is zero.
const DefaultPollInterval = 1 * time.Second

// WaitForJobOptions configures WaitForJob. The zero value is valid: it polls
// every DefaultPollInterval with no additional timeout beyond ctx.
type WaitForJobOptions struct {
	// PollInterval is the delay between GetJob calls. Zero means DefaultPollInterval.
	PollInterval time.Duration
	// Timeout bounds the total wait, in addition to whatever deadline ctx
	// already carries. Zero means no additional timeout is applied.
	Timeout time.Duration
}

// JobFailedError is returned by WaitForJob when the job reaches the "failed"
// terminal status. It wraps the same code/message the gateway recorded on the
// job row (SanitizeWorkerError-filtered — see gateway/internal/jobs.Job), so
// callers get a typed error rather than a raw status string.
type JobFailedError struct {
	Code    string
	Message string
}

func (e *JobFailedError) Error() string {
	return "crucible: job failed: " + e.Code + ": " + e.Message
}

// WaitForJob polls GetJob(ctx, apiKey, jobID) until the job reaches a terminal
// status, ctx is cancelled/expires, or opts.Timeout elapses — whichever comes
// first. On "succeeded" or "cancelled" it returns the job's final
// GetJobResponse (callers distinguish the two via Status); on "failed" it
// returns a *JobFailedError built from the job's recorded error code/message.
// No new HTTP route is introduced: every poll is a plain GetJob call.
func (c *Client) WaitForJob(ctx context.Context, apiKey, jobID string, opts *WaitForJobOptions) (*GetJobResponse, error) {
	interval := DefaultPollInterval
	var timeout time.Duration
	if opts != nil {
		if opts.PollInterval > 0 {
			interval = opts.PollInterval
		}
		timeout = opts.Timeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	for {
		job, err := c.GetJob(ctx, apiKey, jobID)
		if err != nil {
			return nil, err
		}
		switch job.Status {
		case JobStatusSucceeded, JobStatusCancelled:
			return job, nil
		case JobStatusFailed:
			code, message := jobErrorDetails(job.Error)
			return nil, &JobFailedError{Code: code, Message: message}
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// jobErrorDetails extracts code/message from GetJobResponse.Error, which
// decodes as map[string]any (the gateway's asyncJobError JSON shape:
// {"code":"...","message":"..."}). Falls back to a generic description if the
// field is missing or shaped unexpectedly, so WaitForJob never panics on a
// malformed/absent error payload.
func jobErrorDetails(raw any) (code, message string) {
	code, message = "UNKNOWN", "job failed"
	m, ok := raw.(map[string]any)
	if !ok {
		return code, message
	}
	if c, ok := m["code"].(string); ok && c != "" {
		code = c
	}
	if msg, ok := m["message"].(string); ok && msg != "" {
		message = msg
	}
	return code, message
}
