package usage

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Reconciler runs aggregate-only scans against the DB to expose billing flush
// observability signals. It never writes or mutates any row — pure reads only.
type Reconciler struct {
	db *pgxpool.Pool
}

// NewReconciler returns a Reconciler backed by the given pool.
func NewReconciler(db *pgxpool.Pool) *Reconciler {
	return &Reconciler{db: db}
}

// BacklogStats returns aggregate counts for unflushed usage_events rows that the flusher
// can actually process: total billable_units, row count, and the age in seconds of the
// oldest unflushed row. Only rows for customers with a stripe_customer_id are counted,
// mirroring the flusher's own AND c.stripe_customer_id IS NOT NULL filter. Permanently
// unbillable rows (no stripe_customer_id) are tracked separately by UnbillableUsage.
// Returns zeros for all three values when no unflushed Stripe-linked rows exist;
// oldestAgeSecs is also zero when the backlog is empty (COALESCE on NULL interval).
func (r *Reconciler) BacklogStats(ctx context.Context) (units, rows int64, oldestAgeSecs float64, err error) {
	// COUNT(*) is correct here: usage_events.id is BIGSERIAL PRIMARY KEY so each
	// JOIN row is distinct. COALESCE returns 0 for an empty backlog; for non-empty
	// backlogs EXTRACT returns a positive epoch value unless the host clock skewed
	// MIN(created_at) into the future, in which case the caller clamps negatives.
	row := r.db.QueryRow(ctx, `
		SELECT
		    COALESCE(SUM(u.billable_units), 0)::bigint,
		    COUNT(*)::bigint,
		    COALESCE(EXTRACT(EPOCH FROM (NOW() - MIN(u.created_at))), 0)::float8
		FROM usage_events u
		JOIN customers c ON c.id = u.customer_id
		WHERE u.flushed_to_stripe = FALSE
		  AND c.stripe_customer_id IS NOT NULL
	`)
	if err = row.Scan(&units, &rows, &oldestAgeSecs); err != nil {
		return 0, 0, 0, fmt.Errorf("backlog stats: %w", err)
	}
	return units, rows, oldestAgeSecs, nil
}

// UnbillableUsage returns aggregate counts for unflushed usage_events rows whose
// customer has no stripe_customer_id. These rows are permanently excluded by the
// flusher's AND c.stripe_customer_id IS NOT NULL filter and will never reach Stripe
// unless the customer is linked — a silent revenue leak.
// Returns zeros (and no error) when no such rows exist.
func (r *Reconciler) UnbillableUsage(ctx context.Context) (units, rows int64, err error) {
	row := r.db.QueryRow(ctx, `
		SELECT
		    COALESCE(SUM(u.billable_units), 0)::bigint,
		    COUNT(*)::bigint
		FROM usage_events u
		JOIN customers c ON c.id = u.customer_id
		WHERE u.flushed_to_stripe = FALSE
		  AND c.stripe_customer_id IS NULL
	`)
	if err = row.Scan(&units, &rows); err != nil {
		return 0, 0, fmt.Errorf("unbillable usage: %w", err)
	}
	return units, rows, nil
}
