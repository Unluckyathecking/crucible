package webhookout

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/operator"
)

const testOperatorToken = "test-operator-token-adminhttp"

// newDeadLetterRouter mirrors the exact nesting routes.go uses for the
// operator-token-gated /v1/admin subrouter: operator.Middleware wraps the new
// dead-letter routes exactly as it wraps the existing read-only operator routes.
func newDeadLetterRouter(pool *pgxpool.Pool) http.Handler {
	r := chi.NewRouter()
	r.Route("/v1/admin", func(r chi.Router) {
		r.Use(operator.Middleware(testOperatorToken))
		r.Get("/webhooks/deadletters", ListDeadLettersHandler(pool))
		r.Post("/webhooks/deadletters/{id}/replay", ReplaySingleHandler(pool))
		r.Post("/webhooks/deadletters/replay", ReplayBulkHandler(pool))
	})
	return r
}

func doRequest(t *testing.T, h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func countAuditRows(t *testing.T, pool *pgxpool.Pool, action string, targetID int64) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_log WHERE action = $1 AND target_type = 'webhook_delivery' AND target_id = $2`,
		action, strconv.FormatInt(targetID, 10),
	).Scan(&n)
	if err != nil {
		t.Fatalf("countAuditRows: %v", err)
	}
	return n
}

func TestListDeadLettersHandler_OK(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "adminhttp-list@example.com")
	epID := seedEndpoint(t, pool, custID, "https://example.com/hook")
	id := seedDelivery(t, pool, epID, "dead_letter", seedDeliveryOpts{attempts: 7})

	h := newDeadLetterRouter(pool)
	rec := doRequest(t, h, http.MethodGet, "/v1/admin/webhooks/deadletters", testOperatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var page Page[DeadLetterDelivery]
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	found := false
	for _, it := range page.Items {
		if it.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("seeded delivery id %d not present in listing", id)
	}
}

func TestReplaySingleHandler_OK(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "adminhttp-single-ok@example.com")
	epID := seedEndpoint(t, pool, custID, "https://example.com/hook")
	id := seedDelivery(t, pool, epID, "dead_letter", seedDeliveryOpts{attempts: 7})

	h := newDeadLetterRouter(pool)
	path := fmt.Sprintf("/v1/admin/webhooks/deadletters/%d/replay", id)
	rec := doRequest(t, h, http.MethodPost, path, testOperatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body requeuedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Requeued != 1 {
		t.Errorf("requeued: got %d, want 1", body.Requeued)
	}

	if s := fetchDelivery(t, pool, id); s.status != "pending" {
		t.Errorf("delivery status after replay: got %q, want pending", s.status)
	}
	if n := countAuditRows(t, pool, ActionDeliveryReplayed, id); n != 1 {
		t.Errorf("audit rows for delivery %d: got %d, want 1", id, n)
	}
}

func TestReplaySingleHandler_InvalidID(t *testing.T) {
	pool := newTestPostgres(t)
	h := newDeadLetterRouter(pool)
	rec := doRequest(t, h, http.MethodPost, "/v1/admin/webhooks/deadletters/not-a-number/replay", testOperatorToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestReplaySingleHandler_NotFound(t *testing.T) {
	pool := newTestPostgres(t)
	h := newDeadLetterRouter(pool)
	rec := doRequest(t, h, http.MethodPost, "/v1/admin/webhooks/deadletters/999999999/replay", testOperatorToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestReplaySingleHandler_NonDeadLetterRow_NotFound(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "adminhttp-single-nonletter@example.com")
	epID := seedEndpoint(t, pool, custID, "https://example.com/hook")
	id := seedDelivery(t, pool, epID, "delivered", seedDeliveryOpts{attempts: 1})

	h := newDeadLetterRouter(pool)
	path := fmt.Sprintf("/v1/admin/webhooks/deadletters/%d/replay", id)
	rec := doRequest(t, h, http.MethodPost, path, testOperatorToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if s := fetchDelivery(t, pool, id); s.status != "delivered" {
		t.Errorf("delivered row was mutated by replay attempt: got status %q", s.status)
	}
}

func TestReplayBulkHandler_OK(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "adminhttp-bulk-ok@example.com")
	epID := seedEndpoint(t, pool, custID, "https://example.com/hook")
	id1 := seedDelivery(t, pool, epID, "dead_letter", seedDeliveryOpts{attempts: 7})
	id2 := seedDelivery(t, pool, epID, "dead_letter", seedDeliveryOpts{attempts: 7})

	h := newDeadLetterRouter(pool)
	path := "/v1/admin/webhooks/deadletters/replay?endpoint_id=" + epID.String()
	rec := doRequest(t, h, http.MethodPost, path, testOperatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body requeuedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Requeued != 2 {
		t.Errorf("requeued: got %d, want 2", body.Requeued)
	}
	if n := countAuditRows(t, pool, ActionDeliveryReplayed, id1); n != 1 {
		t.Errorf("audit rows for delivery %d: got %d, want 1", id1, n)
	}
	if n := countAuditRows(t, pool, ActionDeliveryReplayed, id2); n != 1 {
		t.Errorf("audit rows for delivery %d: got %d, want 1", id2, n)
	}
}

func TestReplayBulkHandler_MissingEndpointID(t *testing.T) {
	pool := newTestPostgres(t)
	h := newDeadLetterRouter(pool)
	rec := doRequest(t, h, http.MethodPost, "/v1/admin/webhooks/deadletters/replay", testOperatorToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestReplayBulkHandler_InvalidEndpointID(t *testing.T) {
	pool := newTestPostgres(t)
	h := newDeadLetterRouter(pool)
	rec := doRequest(t, h, http.MethodPost, "/v1/admin/webhooks/deadletters/replay?endpoint_id=not-a-uuid", testOperatorToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestReplayBulkHandler_NoMatches(t *testing.T) {
	pool := newTestPostgres(t)
	h := newDeadLetterRouter(pool)
	path := "/v1/admin/webhooks/deadletters/replay?endpoint_id=" + uuid.New().String()
	rec := doRequest(t, h, http.MethodPost, path, testOperatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body requeuedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Requeued != 0 {
		t.Errorf("requeued: got %d, want 0", body.Requeued)
	}
}

// TestDeadLetterRoutes_MissingOperatorToken verifies all three routes are gated
// by the same operator.Middleware used for the existing read-only /v1/admin routes:
// a missing or wrong bearer returns 401 before any handler logic runs.
func TestDeadLetterRoutes_MissingOperatorToken(t *testing.T) {
	pool := newTestPostgres(t)
	h := newDeadLetterRouter(pool)

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"list", http.MethodGet, "/v1/admin/webhooks/deadletters"},
		{"single_replay", http.MethodPost, "/v1/admin/webhooks/deadletters/1/replay"},
		{"bulk_replay", http.MethodPost, "/v1/admin/webhooks/deadletters/replay?endpoint_id=" + uuid.New().String()},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/no_token", func(t *testing.T) {
			rec := doRequest(t, h, tc.method, tc.path, "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status: got %d, want 401; body=%s", rec.Code, rec.Body.String())
			}
		})
		t.Run(tc.name+"/wrong_token", func(t *testing.T) {
			rec := doRequest(t, h, tc.method, tc.path, "wrong-token")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status: got %d, want 401; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
