// Package jobs implements the gateway's durable async job queue: a
// Postgres-backed Store claimed via FOR UPDATE SKIP LOCKED (mirroring
// webhookout's webhook_deliveries claiming pattern), and a bounded
// worker-pool Executor that invokes the existing worker contract
// (proxy.InvokeRequest/InvokeResponse) exactly as the synchronous /v1 path
// does, then meters through the existing usage.Recorder. Quota reservation
// happens the same way it does for the synchronous path: quota.Middleware
// already wraps every /v1 route, including the async enqueue handler.
package jobs

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
)

// Status values for an async_jobs row.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// Job is one async_jobs row.
type Job struct {
	ID             uuid.UUID
	CustomerID     uuid.UUID
	APIKeyID       uuid.UUID
	Operation      string
	RequestID      string
	Plan           string
	Payload        json.RawMessage
	TimeoutSeconds int
	Status         string
	Result         json.RawMessage
	UnitsLabel     string
	BillableUnits  uint64
	ErrorCode      string
	ErrorMessage   string
	// Attempts is the number of worker-invocation attempts made so far,
	// populated by Claim for use by Executor's retry-vs-dead-letter decision.
	Attempts  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ValidBillableUnits reports whether units satisfies the gateway's
// trust-boundary contract: a successful worker response MUST report
// billable_units >= 1 (invariant #2). Both the synchronous /v1 invoke
// handler (server/routes.go) and the async Executor call this single
// predicate so the rule is defined exactly once, not duplicated.
func ValidBillableUnits(units uint64) bool {
	return units >= 1
}

// SanitizeWorkerError applies the gateway's WORKER_ERROR_EXPOSURE policy to a
// worker-reported structured error, mirroring the synchronous /v1 invoke
// handler's behavior (server/routes.go) exactly — both call this single
// function so the sanitization rule is defined once, not duplicated. Any
// exposure value other than "full" (the "sanitized" default, "", or an
// unrecognized value) always returns (apierror.WORKER_UNREACHABLE, "worker
// unavailable") regardless of what the worker reported, hiding internal
// details per operator configuration. In "full" mode the worker's own
// code/message pass through, with an empty code mapped to
// WORKER_BAD_RESPONSE (apierror.UNKNOWN is reserved for Prometheus metric
// labels, never a customer-facing code).
func SanitizeWorkerError(exposure, code, message string) (string, string) {
	if exposure != "full" {
		return apierror.WORKER_UNREACHABLE, "worker unavailable"
	}
	if code == "" {
		code = apierror.WORKER_BAD_RESPONSE
	}
	return code, message
}
