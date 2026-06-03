package usage

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxUsageOperationsLimit caps distinct operation rows per query, matching dashboard/lib/db.ts MAX_USAGE_OPERATIONS_LIMIT.
const maxUsageOperationsLimit = 1000

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
	operationTrimmed := strings.TrimSpace(operation)
	if operationTrimmed != "" && utf8.RuneCountInString(operationTrimmed) > 128 {
		return nil, fmt.Errorf("operation too long (max 128 characters)")
	}

	// Two static queries with fixed $N placeholders avoid any dynamic SQL construction.
	// All caller-supplied values reach the DB exclusively through the args slice.
	var (
		q    string
		args []any
	)
	if operationTrimmed == "" {
		q = `SELECT operation, COALESCE(SUM(billable_units), 0)::bigint, COUNT(*)::bigint
		     FROM usage_events
		     WHERE customer_id = $1 AND created_at >= $2 AND created_at < $3
		     GROUP BY operation ORDER BY operation LIMIT $4`
		args = []any{customerID, from, to, maxUsageOperationsLimit}
	} else {
		q = `SELECT operation, COALESCE(SUM(billable_units), 0)::bigint, COUNT(*)::bigint
		     FROM usage_events
		     WHERE customer_id = $1 AND created_at >= $2 AND created_at < $3 AND operation = $4
		     GROUP BY operation ORDER BY operation LIMIT $5`
		args = []any{customerID, from, to, operationTrimmed, maxUsageOperationsLimit}
	}
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
