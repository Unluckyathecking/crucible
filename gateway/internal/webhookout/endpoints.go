// Customer-facing CRUD for webhook_endpoints: the write-counterpart to the
// delivery log (routes.go's webhookDeliveriesHandler) and the dead-letter
// replay routes (adminhttp.go). Endpoint lifecycle was previously reachable
// only through the NextAuth dashboard (dashboard/app/api/webhooks); this gives
// an API-key-only customer the same registration capability.
package webhookout

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/egress"
	"github.com/Unluckyathecking/crucible/gateway/internal/events"
)

// maxEndpointURLLength bounds the url column against adversarial input,
// mirroring dashboard/app/api/webhooks/route.ts's MAX_URL_LENGTH.
const maxEndpointURLLength = 2048

// ErrEndpointNotFound is returned by DeleteEndpoint when no active endpoint
// with the given id is owned by customerID — whether because the id doesn't
// exist, was already deleted, or belongs to a different customer. Collapsing
// all three into one error (rather than distinguishing "not found" from
// "forbidden") keeps DeleteEndpoint IDOR-safe: a caller can never learn that
// an id belongs to someone else.
var ErrEndpointNotFound = errors.New("webhookout: endpoint not found")

// validationError marks an error returned by CreateEndpoint as caused by
// client input, so CreateEndpointHandler can return 400 instead of 500.
type validationError struct{ err error }

func (v *validationError) Error() string { return v.err.Error() }
func (v *validationError) Unwrap() error { return v.err }

// Endpoint is the customer-visible projection of a webhook_endpoints row.
// The secret is intentionally absent here — it is only ever carried on
// EndpointCreated, and only on the response to the creating request.
type Endpoint struct {
	ID     uuid.UUID `json:"id"`
	URL    string    `json:"url"`
	Active bool      `json:"active"`
	// SubscribedEvents is nil when the endpoint receives every catalogue event
	// type (the 0017_webhook_subscriptions.sql default); non-nil (including an
	// explicitly empty slice) restricts delivery to that subset.
	SubscribedEvents []string  `json:"subscribed_events"`
	CreatedAt        time.Time `json:"created_at"`
}

// EndpointCreated is Endpoint plus the signing secret. Returned exactly once,
// from CreateEndpointHandler's response body — never re-derivable afterward,
// since only the SHA-256 hash equivalent (the raw secret itself) is stored and
// no separate plaintext copy is retained anywhere.
type EndpointCreated struct {
	Endpoint
	SecretHex string `json:"secret_hex"`
}

// ValidateEndpointURL rejects any registration target that cannot be a safe
// outbound-webhook destination: non-https schemes, embedded credentials, and
// loopback/private/link-local IP literals (via egress.Blocked, the Go mirror
// of dashboard/app/api/webhooks/route.ts's isPrivateHostname). A hostname that
// only resolves to a private address at DNS time (rather than being a literal)
// is still caught at delivery time by egress.GuardedTransport — this check is
// registration-time early feedback, not the sole enforcement point.
func ValidateEndpointURL(raw string) error {
	if raw == "" {
		return errors.New("url is required")
	}
	if len(raw) > maxEndpointURLLength {
		return fmt.Errorf("url exceeds maximum length of %d", maxEndpointURLLength)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("invalid url")
	}
	if u.Scheme != "https" {
		return errors.New("url must use https://")
	}
	if u.User != nil {
		return errors.New("url must not contain credentials")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("invalid url")
	}
	if strings.EqualFold(host, "localhost") {
		return errors.New("url hostname not allowed")
	}
	if ip := net.ParseIP(host); ip != nil && egress.Blocked(ip) {
		return errors.New("url hostname not allowed")
	}
	return nil
}

// normalizeSubscribedEvents validates, caps, and deduplicates a customer-
// supplied subscribed_events array before it reaches storage. nil (omitted)
// passes through unchanged — meaning "every event type". Without the cap, a
// caller could submit a near-body-limit array of repeated valid event types;
// that oversized TEXT[] would then be scanned by every `= ANY(subscribed_events)`
// filter in Emit/processDue on every event this gateway ever emits. Mirrors
// dashboard/lib/db.ts's parseSubscribedEvents (cap at the catalogue size,
// dedupe via a set) so both registration paths enforce the same bound.
func normalizeSubscribedEvents(input []string) ([]string, error) {
	if input == nil {
		return nil, nil
	}
	if len(input) > len(events.AllEventTypes) {
		return nil, fmt.Errorf("subscribed_events must not exceed %d entries", len(events.AllEventTypes))
	}
	if err := ValidateSubscribedEvents(input); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(input))
	out := make([]string, 0, len(input))
	for _, e := range input {
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out, nil
}

// CreateEndpoint validates rawURL and subscribedEvents, mints a fresh signing
// secret, and inserts a new webhook_endpoints row owned by customerID. The
// returned SecretHex is the only time the secret is ever surfaced.
func CreateEndpoint(ctx context.Context, db *pgxpool.Pool, customerID uuid.UUID, rawURL string, subscribedEvents []string) (EndpointCreated, error) {
	if err := ValidateEndpointURL(rawURL); err != nil {
		return EndpointCreated{}, &validationError{err}
	}
	subscribedEvents, err := normalizeSubscribedEvents(subscribedEvents)
	if err != nil {
		return EndpointCreated{}, &validationError{err}
	}

	secret, err := GenerateSecret()
	if err != nil {
		return EndpointCreated{}, fmt.Errorf("webhookout: create endpoint: %w", err)
	}

	var out EndpointCreated
	err = db.QueryRow(ctx, `
		INSERT INTO webhook_endpoints (customer_id, url, secret, subscribed_events)
		VALUES ($1, $2, $3, $4)
		RETURNING id, url, active, subscribed_events, created_at
	`, customerID, rawURL, secret, subscribedEvents).Scan(
		&out.ID, &out.URL, &out.Active, &out.SubscribedEvents, &out.CreatedAt,
	)
	if err != nil {
		return EndpointCreated{}, fmt.Errorf("webhookout: insert endpoint: %w", err)
	}
	out.SecretHex = hex.EncodeToString(secret)
	return out, nil
}

// ListEndpoints returns customerID's active webhook endpoints, most-recently
// created first. Never selects the secret column.
func ListEndpoints(ctx context.Context, db *pgxpool.Pool, customerID uuid.UUID) ([]Endpoint, error) {
	rows, err := db.Query(ctx, `
		SELECT id, url, active, subscribed_events, created_at
		FROM webhook_endpoints
		WHERE customer_id = $1 AND active = TRUE
		ORDER BY created_at DESC
	`, customerID)
	if err != nil {
		return nil, fmt.Errorf("webhookout: list endpoints: %w", err)
	}
	defer rows.Close()

	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Endpoint])
	if err != nil {
		return nil, fmt.Errorf("webhookout: scan endpoints: %w", err)
	}
	if items == nil {
		items = []Endpoint{}
	}
	return items, nil
}

// DeleteEndpoint deactivates (soft-deletes) an endpoint owned by customerID.
// Soft-delete — rather than a hard DELETE — preserves webhook_deliveries
// history (FK is ON DELETE CASCADE) and mirrors the active-flag convention
// every other webhookout query already keys off (Emit, processDue, replay).
// Returns ErrEndpointNotFound both when id doesn't exist and when it belongs
// to a different customer, so the HTTP handler can return 404 in both cases
// without leaking cross-customer existence (IDOR-safe).
func DeleteEndpoint(ctx context.Context, db *pgxpool.Pool, id, customerID uuid.UUID) error {
	tag, err := db.Exec(ctx, `
		UPDATE webhook_endpoints SET active = FALSE
		WHERE id = $1 AND customer_id = $2 AND active = TRUE
	`, id, customerID)
	if err != nil {
		return fmt.Errorf("webhookout: delete endpoint: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrEndpointNotFound
	}
	return nil
}
