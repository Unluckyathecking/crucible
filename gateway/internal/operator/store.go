// Package operator provides the read-only operator/admin query layer for the Crucible gateway.
// Every Store method is SELECT-only: it never mutates customer, plan, API-key, or billing
// state, and secret columns (api_keys.hash, etc.) are never selected.
//
// The one exception is the Enterprise Edition operator-token management path (tokens_ee.go):
// TokenStore creates and revokes the operator's OWN admin credentials in the operator_tokens
// table, gated at runtime by the deployment license. That path never touches customer/plan/
// key/billing data, so the read-only guarantee over the tenant data model above still holds.
package operator

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
)

// Store exposes SELECT-only views of the Crucible data model for operator use.
type Store struct {
	db *pgxpool.Pool
}

// NewStore returns a Store backed by db.
func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

// Customer is the operator-visible projection of a customers row.
// api_keys and other secret columns are never included.
type Customer struct {
	ID               uuid.UUID `json:"id"`
	Email            string    `json:"email"`
	StripeCustomerID *string   `json:"stripe_customer_id,omitempty"`
	PlanID           string    `json:"plan_id"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// CustomersFilter constrains the Customers query.
type CustomersFilter struct {
	PlanID  string // empty = no filter
	Page    int    // 1-based; < 1 treated as 1
	PerPage int    // <= 0 or > 100 defaults to 20
}

func (f *CustomersFilter) normalize() {
	f.Page, f.PerPage = paging.Clamp(f.Page, f.PerPage, 20, 100)
}

// Customers returns a paginated list of customers, optionally filtered by
// plan. Returns paging.ErrPageTooLarge if f.Page/f.PerPage would push the
// OFFSET past paging.MaxOffset.
func (s *Store) Customers(ctx context.Context, f CustomersFilter) (paging.Page[Customer], error) {
	f.normalize()
	offset, err := paging.Offset(f.Page, f.PerPage)
	if err != nil {
		return paging.Page[Customer]{}, err
	}

	var planID *string
	if f.PlanID != "" {
		planID = &f.PlanID
	}

	var total int64
	if err := s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM customers
		WHERE ($1::text IS NULL OR plan_id = $1)
	`, planID).Scan(&total); err != nil {
		return paging.Page[Customer]{}, err
	}

	rows, err := s.db.Query(ctx, `
		SELECT id, email, stripe_customer_id, plan_id, created_at, updated_at
		FROM customers
		WHERE ($1::text IS NULL OR plan_id = $1)
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, planID, f.PerPage, offset)
	if err != nil {
		return paging.Page[Customer]{}, err
	}
	defer rows.Close()

	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Customer])
	if err != nil {
		return paging.Page[Customer]{}, err
	}
	if items == nil {
		items = []Customer{}
	}
	return paging.Page[Customer]{Items: items, Total: total}, nil
}

// CustomerByID returns a single customer by UUID.
// Returns pgx.ErrNoRows when the customer does not exist.
func (s *Store) CustomerByID(ctx context.Context, id uuid.UUID) (*Customer, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, email, stripe_customer_id, plan_id, created_at, updated_at
		FROM customers
		WHERE id = $1
	`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	c, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[Customer])
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// OperationUsage is one entry in the per-operation usage breakdown.
type OperationUsage struct {
	Operation  string `json:"operation"`
	TotalUnits int64  `json:"total_units"`
	Calls      int64  `json:"calls"`
}

// CustomerUsageResult is the response for per-customer usage queries.
type CustomerUsageResult struct {
	PeriodStart time.Time        `json:"period_start"`
	PeriodEnd   time.Time        `json:"period_end"`
	TotalUnits  int64            `json:"total_units"`
	TotalCalls  int64            `json:"total_calls"`
	Breakdown   []OperationUsage `json:"breakdown"`
}

// CustomerUsage returns usage_events aggregated by operation for the given customer
// within [start, end). When start or end is zero, the current UTC calendar month is used.
func (s *Store) CustomerUsage(ctx context.Context, id uuid.UUID, start, end time.Time) (CustomerUsageResult, error) {
	if start.IsZero() || end.IsZero() {
		now := time.Now().UTC()
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, 0)
	}

	rows, err := s.db.Query(ctx, `
		SELECT operation, SUM(billable_units) AS total_units, COUNT(*) AS calls
		FROM usage_events
		WHERE customer_id = $1
		  AND created_at >= $2
		  AND created_at < $3
		GROUP BY operation
		ORDER BY total_units DESC
	`, id, start, end)
	if err != nil {
		return CustomerUsageResult{}, err
	}
	defer rows.Close()

	var (
		breakdown  []OperationUsage
		totalUnits int64
		totalCalls int64
	)
	for rows.Next() {
		var op OperationUsage
		if err := rows.Scan(&op.Operation, &op.TotalUnits, &op.Calls); err != nil {
			return CustomerUsageResult{}, err
		}
		breakdown = append(breakdown, op)
		totalUnits += op.TotalUnits
		totalCalls += op.Calls
	}
	if err := rows.Err(); err != nil {
		return CustomerUsageResult{}, err
	}
	if breakdown == nil {
		breakdown = []OperationUsage{}
	}
	return CustomerUsageResult{
		PeriodStart: start,
		PeriodEnd:   end,
		TotalUnits:  totalUnits,
		TotalCalls:  totalCalls,
		Breakdown:   breakdown,
	}, nil
}

// AuditEvent is the operator-visible projection of an audit_log row.
type AuditEvent struct {
	ID         int64           `json:"id"`
	ActorType  string          `json:"actor_type"`
	ActorID    *string         `json:"actor_id,omitempty"`
	Action     string          `json:"action"`
	TargetType *string         `json:"target_type,omitempty"`
	TargetID   *string         `json:"target_id,omitempty"`
	Details    json.RawMessage `json:"details,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// AuditFilter constrains the AuditEvents query.
type AuditFilter struct {
	CustomerID string     // matches actor_id OR target_id (excluding self-loops); empty = no filter
	Action     string     // empty = no filter
	Start      *time.Time // nil = no lower bound
	End        *time.Time // nil = no upper bound
	Page       int
	PerPage    int
}

func (f *AuditFilter) normalize() {
	f.Page, f.PerPage = paging.Clamp(f.Page, f.PerPage, 20, 100)
}

// AuditEvents returns a paginated list of audit_log rows, most-recent first.
// Returns paging.ErrPageTooLarge if f.Page/f.PerPage would push the OFFSET
// past paging.MaxOffset.
func (s *Store) AuditEvents(ctx context.Context, f AuditFilter) (paging.Page[AuditEvent], error) {
	f.normalize()
	offset, err := paging.Offset(f.Page, f.PerPage)
	if err != nil {
		return paging.Page[AuditEvent]{}, err
	}

	var customerID, action *string
	if f.CustomerID != "" {
		customerID = &f.CustomerID
	}
	if f.Action != "" {
		action = &f.Action
	}

	var total int64
	if err := s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM audit_log
		WHERE ($1::text        IS NULL OR actor_id = $1
		                               OR (target_id = $1 AND (actor_id IS NULL OR actor_id != $1)))
		  AND ($2::text        IS NULL OR action     = $2)
		  AND ($3::timestamptz IS NULL OR created_at >= $3)
		  AND ($4::timestamptz IS NULL OR created_at <= $4)
	`, customerID, action, f.Start, f.End).Scan(&total); err != nil {
		return paging.Page[AuditEvent]{}, err
	}

	rows, err := s.db.Query(ctx, `
		SELECT id, actor_type, actor_id, action, target_type, target_id, details, created_at
		FROM audit_log
		WHERE ($1::text        IS NULL OR actor_id = $1
		                               OR (target_id = $1 AND (actor_id IS NULL OR actor_id != $1)))
		  AND ($2::text        IS NULL OR action     = $2)
		  AND ($3::timestamptz IS NULL OR created_at >= $3)
		  AND ($4::timestamptz IS NULL OR created_at <= $4)
		ORDER BY created_at DESC
		LIMIT $5 OFFSET $6
	`, customerID, action, f.Start, f.End, f.PerPage, offset)
	if err != nil {
		return paging.Page[AuditEvent]{}, err
	}
	defer rows.Close()

	items := []AuditEvent{}
	for rows.Next() {
		var ev AuditEvent
		var rawDetails []byte
		if err := rows.Scan(&ev.ID, &ev.ActorType, &ev.ActorID, &ev.Action,
			&ev.TargetType, &ev.TargetID, &rawDetails, &ev.CreatedAt); err != nil {
			return paging.Page[AuditEvent]{}, err
		}
		ev.Details = json.RawMessage(rawDetails)
		items = append(items, ev)
	}
	if err := rows.Err(); err != nil {
		return paging.Page[AuditEvent]{}, err
	}
	return paging.Page[AuditEvent]{Items: items, Total: total}, nil
}

// Plan is the operator-visible projection of a plans row.
type Plan struct {
	ID                 string    `json:"id"`
	DisplayName        string    `json:"display_name"`
	StripePriceID      *string   `json:"stripe_price_id,omitempty"`
	RateLimitPerMinute int       `json:"rate_limit_per_minute"`
	MonthlyUnitCap     *int64    `json:"monthly_unit_cap,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

// Plans returns all plan rows ordered by creation time.
func (s *Store) Plans(ctx context.Context) ([]Plan, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, display_name, stripe_price_id, rate_limit_per_minute, monthly_unit_cap, created_at
		FROM plans
		ORDER BY created_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	plans, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Plan])
	if err != nil {
		return nil, err
	}
	if plans == nil {
		plans = []Plan{}
	}
	return plans, nil
}
