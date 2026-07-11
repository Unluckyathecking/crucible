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
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ValidBillableUnits reports whether units satisfies the gateway's
// trust-boundary contract: a successful worker response MUST report
// billable_units >= 1 (invariant #2). Both the synchronous /v1 invoke
// handler (server/routes.go) and the async Executor call this single
// predicate so the rule is defined exactly once, not duplicated.
func ValidBillableUnits(units uint64) bool {
	return units >= 1
}
