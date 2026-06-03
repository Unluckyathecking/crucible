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

// maxUsageRangeDays mirrors dashboard/lib/db.ts MAX_USAGE_RANGE_DAYS: callers must not request more than 90 days.
const maxUsageRangeDays = 90

// maxOperationLength mirrors dashboard/lib/db.ts MAX_OPERATION_LENGTH.
const maxOperationLength = 128

// OperationAggregate holds per-operation totals from usage_events.
type OperationAggregate struct {
	Operation          string
	TotalBillableUnits int64
	EventCount         int64
}

// QueryByOperation returns per-operation aggregates from usage_events for customerID
// within [from, to) — from is inclusive, to is exclusive. Pass a non-empty operation to filter to one operation only.
// from and to are normalized to UTC on entry so that Sub() measures wall-clock seconds rather than
// local-timezone offsets; callers should pass UTC midnight values to get exact calendar-day boundaries.
func QueryByOperation(ctx context.Context, db *pgxpool.Pool, customerID uuid.UUID, from, to time.Time, operation string) ([]OperationAggregate, error) {
	// Normalize to UTC so Sub() measures wall-clock seconds, not local-timezone offsets.
	from = from.UTC()
	to = to.UTC()
	if from.IsZero() || to.IsZero() {
		return nil, fmt.Errorf("from and to must be non-zero")
	}
	// from == to is a valid empty interval [t, t) returning zero rows — not an error.
	if from.After(to) {
		return nil, fmt.Errorf("from must not be after to")
	}
	// Enforce a 90×24h wall-clock duration limit. With UTC midnight inputs this equals exactly
	// 90 calendar days because UTC has no daylight saving time (every UTC day is 24h).
	if to.Sub(from) > maxUsageRangeDays*24*time.Hour {
		return nil, fmt.Errorf("date range exceeds maximum of %d days", maxUsageRangeDays)
	}
	operationTrimmed := strings.TrimSpace(operation)
	if operationTrimmed != "" && utf8.RuneCountInString(operationTrimmed) > maxOperationLength {
		return nil, fmt.Errorf("operation too long (max %d characters)", maxOperationLength)
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
		return nil, fmt.Errorf("query usage by operation: %w", err)
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
