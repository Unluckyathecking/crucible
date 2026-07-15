// Operator-facing cross-customer reads for async_jobs. Kept separate from
// store.go's customer-scoped Get/List (both of which enforce customer_id at
// the SQL level, invariant load-bearing for IDOR-safety on the customer
// /v1/jobs* routes) so that scoping split is never at risk of an accidental
// merge/edit letting a customer-facing query drop its WHERE customer_id = $N.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AdminJob is the operator-visible, cross-customer projection of an
// async_jobs row. Unlike Job (see store.go), it surfaces claimed_by/claimed_at
// so an operator can identify which gateway instance owns a 'running' row —
// the information ReleaseClaimed's per-instance force-release needs to be
// used safely (confirm the claiming instance is actually dead first).
type AdminJob struct {
	ID             uuid.UUID
	CustomerID     uuid.UUID
	APIKeyID       uuid.UUID
	Operation      string
	RequestID      string
	Plan           string
	Payload        json.RawMessage
	Status         string
	Result         json.RawMessage
	UnitsLabel     string
	BillableUnits  uint64
	ErrorCode      string
	ErrorMessage   string
	ClaimedBy      *uuid.UUID
	ClaimedAt      *time.Time
	TimeoutSeconds int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// AdminGet returns a single async_jobs row by id, unscoped by customer —
// the cross-customer counterpart to Get. Returns ok=false (not an error)
// when id does not exist, mirroring Get's not-found signature.
func (s *Store) AdminGet(ctx context.Context, id uuid.UUID) (AdminJob, bool, error) {
	if s == nil {
		return AdminJob{}, false, nil
	}
	var j AdminJob
	err := s.db.QueryRow(ctx, `
		SELECT id, customer_id, api_key_id, operation, request_id, plan, payload,
		       status, result, units_label, billable_units, error_code, error_message,
		       claimed_by, claimed_at, timeout_seconds, created_at, updated_at
		FROM async_jobs
		WHERE id = $1
	`, id).Scan(
		&j.ID, &j.CustomerID, &j.APIKeyID, &j.Operation, &j.RequestID, &j.Plan, &j.Payload,
		&j.Status, &j.Result, &j.UnitsLabel, &j.BillableUnits, &j.ErrorCode, &j.ErrorMessage,
		&j.ClaimedBy, &j.ClaimedAt, &j.TimeoutSeconds, &j.CreatedAt, &j.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return AdminJob{}, false, nil
		}
		return AdminJob{}, false, fmt.Errorf("jobs: admin get: %w", err)
	}
	return j, true, nil
}

// AdminList returns a page of async_jobs rows across every customer,
// newest-first, optionally narrowed by an exact status match — the
// cross-customer counterpart to List. status nil means "no filter".
//
// Deliberately omits payload/result (mirrors List's own omission for the
// same reason: a page of up to 100 rows would otherwise force Postgres to
// read, and pgx to allocate, however large those blobs happen to be on
// every list call). AdminGet still selects them for the single-job view.
func (s *Store) AdminList(ctx context.Context, status *string, limit, offset int) ([]AdminJob, int64, error) {
	if s == nil {
		return nil, 0, nil
	}

	var total int64
	if err := s.db.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM async_jobs
		WHERE ($1::text IS NULL OR status = $1)
	`, status).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("jobs: admin list count: %w", err)
	}

	rows, err := s.db.Query(ctx, `
		SELECT id, customer_id, api_key_id, operation, request_id, plan,
		       status, units_label, billable_units, error_code, error_message,
		       claimed_by, claimed_at, timeout_seconds, created_at, updated_at
		FROM async_jobs
		WHERE ($1::text IS NULL OR status = $1)
		ORDER BY created_at DESC, id DESC
		LIMIT $2 OFFSET $3
	`, status, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("jobs: admin list query: %w", err)
	}
	defer rows.Close()

	adminJobs := []AdminJob{}
	for rows.Next() {
		var j AdminJob
		if err := rows.Scan(
			&j.ID, &j.CustomerID, &j.APIKeyID, &j.Operation, &j.RequestID, &j.Plan,
			&j.Status, &j.UnitsLabel, &j.BillableUnits, &j.ErrorCode, &j.ErrorMessage,
			&j.ClaimedBy, &j.ClaimedAt, &j.TimeoutSeconds, &j.CreatedAt, &j.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("jobs: admin list scan: %w", err)
		}
		adminJobs = append(adminJobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("jobs: admin list rows: %w", err)
	}
	return adminJobs, total, nil
}
