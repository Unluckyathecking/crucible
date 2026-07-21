// Per-endpoint delivery-health accounting: caps unbounded webhook_deliveries
// growth against a permanently-dead customer endpoint by auto-disabling it
// after WEBHOOK_ENDPOINT_FAILURE_THRESHOLD consecutive terminal dead-letters,
// and lets the customer bring it back once the underlying problem is fixed.
package webhookout

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DisabledReasonDeliveryFailures is the disabled_reason value set by
// recordDeliveryFailure when an endpoint crosses WEBHOOK_ENDPOINT_FAILURE_THRESHOLD.
// It is the only disabled_reason this framework ever writes today; EnableEndpoint
// keys off its presence (rather than a bare active = FALSE check) to distinguish
// an auto-disabled endpoint from a customer soft-deleted one (DeleteEndpoint),
// which leaves disabled_reason NULL.
const DisabledReasonDeliveryFailures = "delivery_failures"

// recordDeliverySuccess resets an endpoint's consecutive dead-letter counter
// after a delivery reaches the 'delivered' terminal state. The WHERE guard
// avoids a write on the (overwhelmingly common) already-healthy case, where
// the counter is already zero; the caller skips the call altogether while
// auto-disable is off. A nil db is impossible here — callers are Emitter
// methods that already nil-check e.db before reaching this — but the
// signature takes db explicitly so health.go has no Emitter-shaped dependency.
func recordDeliverySuccess(ctx context.Context, db *pgxpool.Pool, endpointID uuid.UUID) error {
	_, err := db.Exec(ctx, `
		UPDATE webhook_endpoints SET consecutive_failures = 0
		WHERE id = $1 AND consecutive_failures <> 0
	`, endpointID)
	if err != nil {
		return fmt.Errorf("webhookout: record delivery success: %w", err)
	}
	return nil
}

// recordDeliveryFailure increments endpointID's consecutive dead-letter
// counter and, if the endpoint was active and this failure crosses
// threshold (> 0), auto-disables it — active = FALSE, disabled_at = NOW(),
// disabled_reason = DisabledReasonDeliveryFailures — all in the same
// statement. Combining "increment" and "maybe disable" into one UPDATE
// (rather than a read-then-write pair) closes a race where two concurrent
// delivery-worker replicas dead-lettering the same chronically-failing
// endpoint at once could both read a pre-threshold count and neither would
// flip active, letting the endpoint limp past threshold indefinitely.
//
// The `old` CTE takes a row lock (FOR UPDATE) and captures the pre-update
// snapshot; every reference to old.* in the UPDATE — including inside
// RETURNING — reads that locked snapshot, not the row's new post-update
// values, which is what lets justDisabled report "this call is the one that
// crossed the line" rather than "the endpoint happens to be inactive now"
// (already-disabled and customer-soft-deleted endpoints both leave
// old.active = false, so the CASE conditions correctly no-op for them).
// threshold <= 0 disables auto-disable entirely (WEBHOOK_ENDPOINT_FAILURE_THRESHOLD's
// zero-config-safe default) — justDisabled is always false in that case.
//
// Returns customerID = uuid.Nil, justDisabled = false, err = nil if
// endpointID no longer exists (deleted via the customers FK CASCADE between
// claim and this call) — nothing to record.
func recordDeliveryFailure(ctx context.Context, db *pgxpool.Pool, endpointID uuid.UUID, threshold int) (customerID uuid.UUID, justDisabled bool, err error) {
	err = db.QueryRow(ctx, `
		WITH old AS (
			SELECT customer_id, active, consecutive_failures
			FROM webhook_endpoints WHERE id = $1 FOR UPDATE
		), upd AS (
			UPDATE webhook_endpoints we
			SET consecutive_failures = old.consecutive_failures + 1,
			    active = CASE WHEN old.active AND old.consecutive_failures + 1 >= $2 AND $2 > 0
			                  THEN FALSE ELSE old.active END,
			    disabled_at = CASE WHEN old.active AND old.consecutive_failures + 1 >= $2 AND $2 > 0
			                       THEN NOW() ELSE we.disabled_at END,
			    disabled_reason = CASE WHEN old.active AND old.consecutive_failures + 1 >= $2 AND $2 > 0
			                           THEN $3 ELSE we.disabled_reason END
			FROM old
			WHERE we.id = $1
			RETURNING old.customer_id AS customer_id,
			          (old.active AND old.consecutive_failures + 1 >= $2 AND $2 > 0) AS just_disabled
		)
		SELECT customer_id, just_disabled FROM upd
	`, endpointID, threshold, DisabledReasonDeliveryFailures).Scan(&customerID, &justDisabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.UUID{}, false, nil
		}
		return uuid.UUID{}, false, fmt.Errorf("webhookout: record delivery failure: %w", err)
	}
	return customerID, justDisabled, nil
}

// EnableEndpoint re-enables an auto-disabled endpoint owned by customerID,
// resetting its failure counter so a fresh run of consecutive dead-letters is
// required before it can auto-disable again. Only a row with
// disabled_reason = DisabledReasonDeliveryFailures matches — a customer
// soft-deleted endpoint (DeleteEndpoint, active = FALSE, disabled_reason
// NULL) is deliberately not revivable through this path, and returns
// ErrEndpointNotFound like every other not-found/wrong-owner case here
// (IDOR-safe: a caller can't distinguish "doesn't exist", "belongs to
// someone else", and "exists but soft-deleted, not auto-disabled").
func EnableEndpoint(ctx context.Context, db *pgxpool.Pool, id, customerID uuid.UUID) error {
	tag, err := db.Exec(ctx, `
		UPDATE webhook_endpoints
		SET active = TRUE, disabled_at = NULL, disabled_reason = NULL, consecutive_failures = 0
		WHERE id = $1 AND customer_id = $2 AND active = FALSE AND disabled_reason = $3
	`, id, customerID, DisabledReasonDeliveryFailures)
	if err != nil {
		return fmt.Errorf("webhookout: enable endpoint: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrEndpointNotFound
	}
	return nil
}
