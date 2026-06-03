package usage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OperationAggregate holds per-operation totals from usage_events.
type OperationAggregate struct {
	Operation          string
	TotalBillableUnits int64
	EventCount         int64
}

// QueryByOperation returns per-operation aggregates from usage_events for customerID
// within [from, to) — from is inclusive, to is exclusive. Pass a non-empty operation to filter to one operation only.
func QueryByOperation(ctx context.Context, db *pgxpool.Pool, customerID uuid.UUID, from, to time.Time, operation string) ([]OperationAggregate, error) {
	if from.IsZero() || to.IsZero() {
		return nil, fmt.Errorf("from and to must be non-zero")
	}
	if from.After(to) {
		return nil, fmt.Errorf("from must not be after to")
	}
	if len(operation) > 128 {
		return nil, fmt.Errorf("operation too long (max 128 characters)")
	}
	// Half-open interval [from, to): from is inclusive, to is exclusive.
	q := `SELECT operation, SUM(billable_units)::bigint, COUNT(*)::bigint
	      FROM usage_events
	      WHERE customer_id = $1 AND created_at >= $2 AND created_at < $3`
	args := []any{customerID, from, to}
	if operation != "" {
		args = append(args, operation)
		q += fmt.Sprintf(` AND operation = $%d`, len(args))
	}
	q += ` GROUP BY operation ORDER BY operation`

	rows, err := db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []OperationAggregate
	for rows.Next() {
		var a OperationAggregate
		if err := rows.Scan(&a.Operation, &a.TotalBillableUnits, &a.EventCount); err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}
