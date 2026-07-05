package webhookout

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/events"
	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
)

// testCustomerCtxKey carries the stand-in "authenticated customer" for tests.
// webhookout cannot import gateway/internal/auth (see CustomerIDFunc's doc
// comment for why), so tests inject a customer id through this local context
// key instead of a real auth.Key, wired up via testCustomerIDFunc below —
// exactly the seam routes.go's authCustomerID fills in production.
type testCtxKey string

const testCustomerCtxKey testCtxKey = "test-customer-id"

// newEndpointsRouter mirrors the exact nesting routes.go uses for the
// d.DB-gated /v1/webhooks block: same paths, same handlers, wired to
// testCustomerIDFunc in place of production's auth.Middleware + authCustomerID.
func newEndpointsRouter(pool *pgxpool.Pool) http.Handler {
	r := chi.NewRouter()
	r.Post("/v1/webhooks/endpoints", CreateEndpointHandler(pool, testCustomerIDFunc))
	r.Get("/v1/webhooks/endpoints", ListEndpointsHandler(pool, testCustomerIDFunc))
	r.Delete("/v1/webhooks/endpoints/{id}", DeleteEndpointHandler(pool, testCustomerIDFunc))
	r.Patch("/v1/webhooks/endpoints/{id}", UpdateEndpointSubscriptionHandler(pool, testCustomerIDFunc))
	r.Post("/v1/webhooks/endpoints/{id}/rotate-secret", RotateEndpointSecretHandler(pool, testCustomerIDFunc))
	return r
}

func testCustomerIDFunc(r *http.Request) (uuid.UUID, bool) {
	id, ok := r.Context().Value(testCustomerCtxKey).(uuid.UUID)
	return id, ok
}

func testKeyContext(customerID uuid.UUID) context.Context {
	return context.WithValue(context.Background(), testCustomerCtxKey, customerID)
}

func createEndpointReq(t *testing.T, customerID uuid.UUID, body map[string]any) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/endpoints", bytes.NewReader(raw))
	return req.WithContext(testKeyContext(customerID))
}

func TestCreateEndpointHandler_Success(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "create-ok@example.com")
	r := newEndpointsRouter(pool)

	req := createEndpointReq(t, cust, map[string]any{"url": "https://example.com/hook"})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created EndpointCreated
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.SecretHex == "" {
		t.Error("secret_hex was empty")
	}
	if len(created.SecretHex) != 64 { // 32 bytes hex-encoded
		t.Errorf("secret_hex length = %d, want 64", len(created.SecretHex))
	}
	if created.URL != "https://example.com/hook" {
		t.Errorf("url = %q", created.URL)
	}
	if !created.Active {
		t.Error("expected new endpoint to be active")
	}
	if created.SubscribedEvents != nil {
		t.Errorf("subscribed_events = %v, want nil (all events)", created.SubscribedEvents)
	}
	if created.ID == uuid.Nil {
		t.Error("expected non-nil endpoint id")
	}
}

func TestCreateEndpointHandler_ExplicitSubscribedEvents(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "create-subs@example.com")
	r := newEndpointsRouter(pool)

	req := createEndpointReq(t, cust, map[string]any{
		"url":               "https://example.com/hook",
		"subscribed_events": []string{events.QuotaExceeded, events.APIKeyRevoked},
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created EndpointCreated
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(created.SubscribedEvents) != 2 {
		t.Fatalf("subscribed_events = %v, want 2 entries", created.SubscribedEvents)
	}
}

// TestCreateEndpointHandler_DedupesSubscribedEvents asserts a subscribed_events
// array containing the same valid event type repeated is stored deduplicated,
// not verbatim.
func TestCreateEndpointHandler_DedupesSubscribedEvents(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "create-subs-dedup@example.com")
	r := newEndpointsRouter(pool)

	req := createEndpointReq(t, cust, map[string]any{
		"url":               "https://example.com/hook",
		"subscribed_events": []string{events.QuotaExceeded, events.QuotaExceeded, events.QuotaExceeded},
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created EndpointCreated
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(created.SubscribedEvents) != 1 {
		t.Fatalf("subscribed_events = %v, want exactly 1 deduplicated entry", created.SubscribedEvents)
	}
}

// TestCreateEndpointHandler_RejectsOversizedSubscribedEvents asserts a
// subscribed_events array longer than the full event catalogue is rejected
// before it ever reaches storage, rather than being persisted as an
// oversized TEXT[] that every `= ANY(subscribed_events)` filter in
// Emit/processDue would scan on every emitted event.
func TestCreateEndpointHandler_RejectsOversizedSubscribedEvents(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "create-subs-oversized@example.com")
	r := newEndpointsRouter(pool)

	oversized := make([]string, len(events.AllEventTypes)+1)
	for i := range oversized {
		oversized[i] = events.QuotaExceeded
	}
	req := createEndpointReq(t, cust, map[string]any{
		"url":               "https://example.com/hook",
		"subscribed_events": oversized,
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
}

// TestNormalizeSubscribedEvents exercises the validation/cap/dedupe function
// directly, table-style.
func TestNormalizeSubscribedEvents(t *testing.T) {
	tooMany := make([]string, len(events.AllEventTypes)+1)
	for i := range tooMany {
		tooMany[i] = events.QuotaExceeded
	}

	cases := []struct {
		name      string
		input     []string
		wantErr   bool
		wantCount int
	}{
		{"nil means all events", nil, false, 0},
		{"empty means no events", []string{}, false, 0},
		{"single valid entry", []string{events.QuotaExceeded}, false, 1},
		{"duplicates collapse", []string{events.QuotaExceeded, events.QuotaExceeded}, false, 1},
		{"unknown event type rejected", []string{"not.a.real.event"}, true, 0},
		{"too many entries rejected", tooMany, true, 0},
		{"exactly the catalogue size allowed", events.AllEventTypes, false, len(events.AllEventTypes)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := normalizeSubscribedEvents(c.input)
			if (err != nil) != c.wantErr {
				t.Fatalf("normalizeSubscribedEvents(%v) err = %v, wantErr = %v", c.input, err, c.wantErr)
			}
			if err != nil {
				return
			}
			if c.input == nil {
				if out != nil {
					t.Errorf("nil input produced non-nil output: %v", out)
				}
				return
			}
			if len(out) != c.wantCount {
				t.Errorf("normalizeSubscribedEvents(%v) = %v, want %d entries", c.input, out, c.wantCount)
			}
		})
	}
}

func TestCreateEndpointHandler_RejectsNonHTTPS(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "reject-http@example.com")
	r := newEndpointsRouter(pool)

	req := createEndpointReq(t, cust, map[string]any{"url": "http://example.com/hook"})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateEndpointHandler_RejectsPrivateIPLiteral(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "reject-private@example.com")
	r := newEndpointsRouter(pool)

	for _, url := range []string{
		"https://127.0.0.1/hook",
		"https://10.0.0.5/hook",
		"https://172.16.0.1/hook",
		"https://192.168.1.1/hook",
		"https://169.254.169.254/hook", // cloud metadata endpoint
		"https://0.0.0.0/hook",
		"https://100.64.0.1/hook", // CGNAT
		"https://[::1]/hook",
	} {
		req := createEndpointReq(t, cust, map[string]any{"url": url})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("url %q: status = %d, want 400", url, rec.Code)
		}
	}
}

func TestCreateEndpointHandler_RejectsLocalhost(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "reject-localhost@example.com")
	r := newEndpointsRouter(pool)

	req := createEndpointReq(t, cust, map[string]any{"url": "https://localhost/hook"})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateEndpointHandler_RejectsCredentialsInURL(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "reject-creds@example.com")
	r := newEndpointsRouter(pool)

	req := createEndpointReq(t, cust, map[string]any{"url": "https://user:pass@example.com/hook"})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateEndpointHandler_RejectsUnknownEventType(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "reject-event@example.com")
	r := newEndpointsRouter(pool)

	req := createEndpointReq(t, cust, map[string]any{
		"url":               "https://example.com/hook",
		"subscribed_events": []string{"not.a.real.event"},
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateEndpointHandler_NoAuth(t *testing.T) {
	pool := newTestPostgres(t)
	r := newEndpointsRouter(pool)

	raw, _ := json.Marshal(map[string]any{"url": "https://example.com/hook"})
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/endpoints", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestListEndpointsHandler_SecretNeverSerialized asserts the list response's
// row shape has no secret field at all — not just an empty one — proving the
// secret can never leak back out after the creating request.
func TestListEndpointsHandler_SecretNeverSerialized(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "list-no-secret@example.com")
	r := newEndpointsRouter(pool)

	createReq := createEndpointReq(t, cust, map[string]any{"url": "https://example.com/hook"})
	createRec := httptest.NewRecorder()
	r.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createRec.Code, createRec.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v1/webhooks/endpoints", nil).WithContext(testKeyContext(cust))
	listRec := httptest.NewRecorder()
	r.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listRec.Code, listRec.Body.String())
	}

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(listRec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	for _, field := range []string{"secret_hex", "secret"} {
		if _, present := rows[0][field]; present {
			t.Errorf("list response row contains forbidden field %q", field)
		}
	}
	if strings.Contains(listRec.Body.String(), "secret") {
		t.Errorf("list response body mentions \"secret\" at all: %s", listRec.Body.String())
	}
}

func TestListEndpointsHandler_ScopedToCustomer(t *testing.T) {
	pool := newTestPostgres(t)
	custA := seedCustomer(t, pool, "list-scope-a@example.com")
	custB := seedCustomer(t, pool, "list-scope-b@example.com")
	r := newEndpointsRouter(pool)

	for _, cust := range []uuid.UUID{custA, custB} {
		req := createEndpointReq(t, cust, map[string]any{"url": "https://example.com/hook"})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create for %s: status = %d", cust, rec.Code)
		}
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v1/webhooks/endpoints", nil).WithContext(testKeyContext(custA))
	listRec := httptest.NewRecorder()
	r.ServeHTTP(listRec, listReq)

	var rows []Endpoint
	if err := json.Unmarshal(listRec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("customer A sees %d endpoints, want exactly their own 1", len(rows))
	}
}

func TestDeleteEndpointHandler_OwnedByOtherCustomer_404(t *testing.T) {
	pool := newTestPostgres(t)
	owner := seedCustomer(t, pool, "delete-owner@example.com")
	attacker := seedCustomer(t, pool, "delete-attacker@example.com")
	r := newEndpointsRouter(pool)

	createReq := createEndpointReq(t, owner, map[string]any{"url": "https://example.com/hook"})
	createRec := httptest.NewRecorder()
	r.ServeHTTP(createRec, createReq)
	var created EndpointCreated
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/v1/webhooks/endpoints/"+created.ID.String(), nil).
		WithContext(testKeyContext(attacker))
	delRec := httptest.NewRecorder()
	r.ServeHTTP(delRec, delReq)

	if delRec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (IDOR-safe)", delRec.Code)
	}

	// The endpoint must still be active — the cross-customer delete must not
	// have taken effect.
	listReq := httptest.NewRequest(http.MethodGet, "/v1/webhooks/endpoints", nil).WithContext(testKeyContext(owner))
	listRec := httptest.NewRecorder()
	r.ServeHTTP(listRec, listReq)
	var rows []Endpoint
	if err := json.Unmarshal(listRec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("owner sees %d endpoints after failed cross-customer delete, want 1", len(rows))
	}
}

func TestDeleteEndpointHandler_Success(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "delete-ok@example.com")
	r := newEndpointsRouter(pool)

	createReq := createEndpointReq(t, cust, map[string]any{"url": "https://example.com/hook"})
	createRec := httptest.NewRecorder()
	r.ServeHTTP(createRec, createReq)
	var created EndpointCreated
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/v1/webhooks/endpoints/"+created.ID.String(), nil).
		WithContext(testKeyContext(cust))
	delRec := httptest.NewRecorder()
	r.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body = %s", delRec.Code, delRec.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v1/webhooks/endpoints", nil).WithContext(testKeyContext(cust))
	listRec := httptest.NewRecorder()
	r.ServeHTTP(listRec, listReq)
	var rows []Endpoint
	if err := json.Unmarshal(listRec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("deleted endpoint still appears in list: %v", rows)
	}
}

func TestDeleteEndpointHandler_NotFound(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "delete-notfound@example.com")
	r := newEndpointsRouter(pool)

	delReq := httptest.NewRequest(http.MethodDelete, "/v1/webhooks/endpoints/"+uuid.New().String(), nil).
		WithContext(testKeyContext(cust))
	delRec := httptest.NewRecorder()
	r.ServeHTTP(delRec, delReq)

	if delRec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", delRec.Code)
	}
}

func TestDeleteEndpointHandler_InvalidID(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "delete-invalid-id@example.com")
	r := newEndpointsRouter(pool)

	delReq := httptest.NewRequest(http.MethodDelete, "/v1/webhooks/endpoints/not-a-uuid", nil).
		WithContext(testKeyContext(cust))
	delRec := httptest.NewRecorder()
	r.ServeHTTP(delRec, delReq)

	if delRec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", delRec.Code)
	}
}

// patchEndpointReq builds a PATCH /v1/webhooks/endpoints/{id} request carrying
// body as its JSON payload, in customerID's auth context.
func patchEndpointReq(t *testing.T, customerID uuid.UUID, id uuid.UUID, body map[string]any) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/v1/webhooks/endpoints/"+id.String(), bytes.NewReader(raw))
	return req.WithContext(testKeyContext(customerID))
}

// seedDeliveryForEvent inserts a webhook_deliveries row with an explicit
// event_type and status, for exercising PATCH's stale-row cleanup — unlike
// replay_test.go's seedDelivery, which hardcodes event_type to "test.event".
func seedDeliveryForEvent(t *testing.T, pool *pgxpool.Pool, endpointID uuid.UUID, eventType, status string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO webhook_deliveries (event_id, event_type, endpoint_id, payload, status)
		VALUES ($1, $2, $3, '{}'::jsonb, $4)
		RETURNING id
	`, uuid.NewString(), eventType, endpointID, status).Scan(&id)
	if err != nil {
		t.Fatalf("seedDeliveryForEvent: %v", err)
	}
	return id
}

// deliveryExists reports whether a webhook_deliveries row with id still exists.
func deliveryExists(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM webhook_deliveries WHERE id = $1)`, id,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("deliveryExists(%d): %v", id, err)
	}
	return exists
}

// storedSubscribedEvents reads back webhook_endpoints.subscribed_events directly.
func storedSubscribedEvents(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) []string {
	t.Helper()
	var events []string
	err := pool.QueryRow(context.Background(),
		`SELECT subscribed_events FROM webhook_endpoints WHERE id = $1`, id,
	).Scan(&events)
	if err != nil {
		t.Fatalf("storedSubscribedEvents(%s): %v", id, err)
	}
	return events
}

// storedSecret reads back webhook_endpoints.secret directly.
func storedSecret(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) []byte {
	t.Helper()
	var secret []byte
	err := pool.QueryRow(context.Background(),
		`SELECT secret FROM webhook_endpoints WHERE id = $1`, id,
	).Scan(&secret)
	if err != nil {
		t.Fatalf("storedSecret(%s): %v", id, err)
	}
	return secret
}

func TestUpdateEndpointSubscriptionHandler_Persists(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "patch-persist@example.com")
	r := newEndpointsRouter(pool)

	createRec := httptest.NewRecorder()
	r.ServeHTTP(createRec, createEndpointReq(t, cust, map[string]any{"url": "https://example.com/hook"}))
	var created EndpointCreated
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	patchRec := httptest.NewRecorder()
	r.ServeHTTP(patchRec, patchEndpointReq(t, cust, created.ID, map[string]any{
		"subscribed_events": []string{events.QuotaExceeded, events.APIKeyRevoked},
	}))
	if patchRec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body = %s", patchRec.Code, patchRec.Body.String())
	}

	got := storedSubscribedEvents(t, pool, created.ID)
	if len(got) != 2 {
		t.Fatalf("subscribed_events = %v, want 2 entries", got)
	}
}

// TestUpdateEndpointSubscriptionHandler_ResubscribeAll asserts that an omitted
// (or explicit null) subscribed_events reverts a previously-narrowed endpoint
// back to nil — subscribed to every event type — mirroring
// dashboard/lib/db.ts:677's NULL-means-all convention.
func TestUpdateEndpointSubscriptionHandler_ResubscribeAll(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "patch-resub-all@example.com")
	r := newEndpointsRouter(pool)

	createRec := httptest.NewRecorder()
	r.ServeHTTP(createRec, createEndpointReq(t, cust, map[string]any{
		"url":               "https://example.com/hook",
		"subscribed_events": []string{events.QuotaExceeded},
	}))
	var created EndpointCreated
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	patchRec := httptest.NewRecorder()
	r.ServeHTTP(patchRec, patchEndpointReq(t, cust, created.ID, map[string]any{"subscribed_events": nil}))
	if patchRec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body = %s", patchRec.Code, patchRec.Body.String())
	}

	if got := storedSubscribedEvents(t, pool, created.ID); got != nil {
		t.Errorf("subscribed_events = %v, want nil (all events)", got)
	}
}

// TestUpdateEndpointSubscriptionHandler_PrunesStaleDeliveries asserts that
// narrowing subscribed_events deletes pending/dead_letter deliveries for
// event types no longer subscribed to, but leaves deliveries for still-
// subscribed event types (and non-pending/dead_letter statuses) untouched —
// mirroring dashboard/lib/db.ts:685-693.
func TestUpdateEndpointSubscriptionHandler_PrunesStaleDeliveries(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "patch-prune@example.com")
	epID := seedEndpoint(t, pool, cust, "https://example.com/hook")

	stalePending := seedDeliveryForEvent(t, pool, epID, events.APIKeyRevoked, "pending")
	staleDeadLetter := seedDeliveryForEvent(t, pool, epID, events.APIKeyRevoked, "dead_letter")
	keptPending := seedDeliveryForEvent(t, pool, epID, events.QuotaExceeded, "pending")
	inFlight := seedDeliveryForEvent(t, pool, epID, events.APIKeyRevoked, "delivering")

	r := newEndpointsRouter(pool)
	patchRec := httptest.NewRecorder()
	r.ServeHTTP(patchRec, patchEndpointReq(t, cust, epID, map[string]any{
		"subscribed_events": []string{events.QuotaExceeded},
	}))
	if patchRec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body = %s", patchRec.Code, patchRec.Body.String())
	}

	if deliveryExists(t, pool, stalePending) {
		t.Error("stale pending delivery for unsubscribed event type should have been deleted")
	}
	if deliveryExists(t, pool, staleDeadLetter) {
		t.Error("stale dead_letter delivery for unsubscribed event type should have been deleted")
	}
	if !deliveryExists(t, pool, keptPending) {
		t.Error("pending delivery for a still-subscribed event type should NOT have been deleted")
	}
	if !deliveryExists(t, pool, inFlight) {
		t.Error("in-flight (delivering) delivery should NOT have been deleted, even for an unsubscribed event type")
	}
}

func TestUpdateEndpointSubscriptionHandler_OwnedByOtherCustomer_404(t *testing.T) {
	pool := newTestPostgres(t)
	owner := seedCustomer(t, pool, "patch-owner@example.com")
	attacker := seedCustomer(t, pool, "patch-attacker@example.com")
	epID := seedEndpoint(t, pool, owner, "https://example.com/hook")
	r := newEndpointsRouter(pool)

	patchRec := httptest.NewRecorder()
	r.ServeHTTP(patchRec, patchEndpointReq(t, attacker, epID, map[string]any{
		"subscribed_events": []string{events.QuotaExceeded},
	}))

	if patchRec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (IDOR-safe)", patchRec.Code)
	}
	if got := storedSubscribedEvents(t, pool, epID); got != nil {
		t.Errorf("subscribed_events = %v, cross-customer PATCH must not have taken effect", got)
	}
}

func TestUpdateEndpointSubscriptionHandler_NotFound(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "patch-notfound@example.com")
	r := newEndpointsRouter(pool)

	patchRec := httptest.NewRecorder()
	r.ServeHTTP(patchRec, patchEndpointReq(t, cust, uuid.New(), map[string]any{
		"subscribed_events": []string{events.QuotaExceeded},
	}))

	if patchRec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", patchRec.Code)
	}
}

func TestUpdateEndpointSubscriptionHandler_InvalidID(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "patch-invalid-id@example.com")
	r := newEndpointsRouter(pool)

	req := httptest.NewRequest(http.MethodPatch, "/v1/webhooks/endpoints/not-a-uuid", bytes.NewReader([]byte(`{}`))).
		WithContext(testKeyContext(cust))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestUpdateEndpointSubscriptionHandler_NoAuth(t *testing.T) {
	pool := newTestPostgres(t)
	r := newEndpointsRouter(pool)

	req := httptest.NewRequest(http.MethodPatch, "/v1/webhooks/endpoints/"+uuid.New().String(), bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestUpdateEndpointSubscriptionHandler_RejectsUnknownEventType(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "patch-unknown-event@example.com")
	epID := seedEndpoint(t, pool, cust, "https://example.com/hook")
	r := newEndpointsRouter(pool)

	patchRec := httptest.NewRecorder()
	r.ServeHTTP(patchRec, patchEndpointReq(t, cust, epID, map[string]any{
		"subscribed_events": []string{"not.a.real.event"},
	}))

	if patchRec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", patchRec.Code)
	}
}

func TestRotateEndpointSecretHandler_Success(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "rotate-ok@example.com")
	r := newEndpointsRouter(pool)

	createRec := httptest.NewRecorder()
	r.ServeHTTP(createRec, createEndpointReq(t, cust, map[string]any{"url": "https://example.com/hook"}))
	var created EndpointCreated
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	originalSecret := storedSecret(t, pool, created.ID)

	rotateReq := httptest.NewRequest(http.MethodPost, "/v1/webhooks/endpoints/"+created.ID.String()+"/rotate-secret", nil).
		WithContext(testKeyContext(cust))
	rotateRec := httptest.NewRecorder()
	r.ServeHTTP(rotateRec, rotateReq)
	if rotateRec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rotateRec.Code, rotateRec.Body.String())
	}
	if got := rotateRec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-store")
	}

	var rotated struct {
		SecretHex string `json:"secret_hex"`
	}
	if err := json.Unmarshal(rotateRec.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("decode rotate response: %v", err)
	}
	if len(rotated.SecretHex) != 64 { // 32 bytes hex-encoded
		t.Errorf("secret_hex length = %d, want 64", len(rotated.SecretHex))
	}
	if rotated.SecretHex == created.SecretHex {
		t.Error("rotated secret_hex must differ from the original")
	}

	newSecret := storedSecret(t, pool, created.ID)
	if hex.EncodeToString(newSecret) != rotated.SecretHex {
		t.Error("stored secret does not match the returned secret_hex")
	}
	if hex.EncodeToString(originalSecret) == hex.EncodeToString(newSecret) {
		t.Error("stored secret was not actually rotated")
	}
	// The old secret must no longer verify against a signature.
	if Sign(originalSecret, "123", []byte("body")) == Sign(newSecret, "123", []byte("body")) {
		t.Error("signatures computed with the old and new secret must differ")
	}
}

func TestRotateEndpointSecretHandler_OwnedByOtherCustomer_404(t *testing.T) {
	pool := newTestPostgres(t)
	owner := seedCustomer(t, pool, "rotate-owner@example.com")
	attacker := seedCustomer(t, pool, "rotate-attacker@example.com")
	epID := seedEndpoint(t, pool, owner, "https://example.com/hook")
	originalSecret := storedSecret(t, pool, epID)
	r := newEndpointsRouter(pool)

	rotateReq := httptest.NewRequest(http.MethodPost, "/v1/webhooks/endpoints/"+epID.String()+"/rotate-secret", nil).
		WithContext(testKeyContext(attacker))
	rotateRec := httptest.NewRecorder()
	r.ServeHTTP(rotateRec, rotateReq)

	if rotateRec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (IDOR-safe)", rotateRec.Code)
	}
	if got := storedSecret(t, pool, epID); hex.EncodeToString(got) != hex.EncodeToString(originalSecret) {
		t.Error("cross-customer rotate-secret must not have taken effect")
	}
}

func TestRotateEndpointSecretHandler_NotFound(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "rotate-notfound@example.com")
	r := newEndpointsRouter(pool)

	rotateReq := httptest.NewRequest(http.MethodPost, "/v1/webhooks/endpoints/"+uuid.New().String()+"/rotate-secret", nil).
		WithContext(testKeyContext(cust))
	rotateRec := httptest.NewRecorder()
	r.ServeHTTP(rotateRec, rotateReq)

	if rotateRec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rotateRec.Code)
	}
}

func TestRotateEndpointSecretHandler_InvalidID(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "rotate-invalid-id@example.com")
	r := newEndpointsRouter(pool)

	rotateReq := httptest.NewRequest(http.MethodPost, "/v1/webhooks/endpoints/not-a-uuid/rotate-secret", nil).
		WithContext(testKeyContext(cust))
	rotateRec := httptest.NewRecorder()
	r.ServeHTTP(rotateRec, rotateReq)

	if rotateRec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rotateRec.Code)
	}
}

func TestRotateEndpointSecretHandler_NoAuth(t *testing.T) {
	pool := newTestPostgres(t)
	r := newEndpointsRouter(pool)

	rotateReq := httptest.NewRequest(http.MethodPost, "/v1/webhooks/endpoints/"+uuid.New().String()+"/rotate-secret", nil)
	rotateRec := httptest.NewRecorder()
	r.ServeHTTP(rotateRec, rotateReq)

	if rotateRec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rotateRec.Code)
	}
}

// TestValidateEndpointURL exercises the validation function directly, table-style.
func TestValidateEndpointURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"valid https", "https://example.com/webhook", false},
		{"valid https with path and query", "https://example.com/webhook?x=1", false},
		{"http rejected", "http://example.com/webhook", true},
		{"empty rejected", "", true},
		{"malformed rejected", "://not-a-url", true},
		{"localhost rejected", "https://localhost/webhook", true},
		{"loopback ipv4 rejected", "https://127.0.0.1/webhook", true},
		{"loopback ipv6 rejected", "https://[::1]/webhook", true},
		{"private 10/8 rejected", "https://10.1.2.3/webhook", true},
		{"private 172.16/12 rejected", "https://172.20.1.1/webhook", true},
		{"private 192.168/16 rejected", "https://192.168.0.1/webhook", true},
		{"link-local rejected", "https://169.254.1.1/webhook", true},
		{"unspecified rejected", "https://0.0.0.0/webhook", true},
		{"cgnat rejected", "https://100.64.1.1/webhook", true},
		{"credentials rejected", "https://user:pass@example.com/webhook", true},
		{"too long rejected", "https://example.com/" + strings.Repeat("a", maxEndpointURLLength), true},
		{"public ip literal allowed", "https://8.8.8.8/webhook", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateEndpointURL(c.url)
			if (err != nil) != c.wantErr {
				t.Errorf("ValidateEndpointURL(%q) err = %v, wantErr = %v", c.url, err, c.wantErr)
			}
		})
	}
}

// TestOpenAPI_WebhookEndpointsPathsDocumented asserts the served /openapi.json
// (openapi.Handler's output, not Build()'s own return value — these are
// framework infra layered on the same way as /v1/usage, see
// openapi.webhookEndpointsPathItems) documents every webhook-endpoint-lifecycle
// route: the original create/list/delete tier plus this PR's PATCH subscription
// update and POST rotate-secret.
func TestOpenAPI_WebhookEndpointsPathsDocumented(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	openapi.Handler(nil)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var doc struct {
		Paths map[string]struct {
			Get    json.RawMessage `json:"get"`
			Post   json.RawMessage `json:"post"`
			Patch  json.RawMessage `json:"patch"`
			Delete json.RawMessage `json:"delete"`
		} `json:"paths"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&doc); err != nil {
		t.Fatalf("decode /openapi.json: %v", err)
	}

	endpoints, ok := doc.Paths["/v1/webhooks/endpoints"]
	if !ok {
		t.Fatal("served /openapi.json missing /v1/webhooks/endpoints path")
	}
	if endpoints.Post == nil {
		t.Error("/v1/webhooks/endpoints has no POST operation documented")
	}
	if endpoints.Get == nil {
		t.Error("/v1/webhooks/endpoints has no GET operation documented")
	}

	byID, ok := doc.Paths["/v1/webhooks/endpoints/{id}"]
	if !ok {
		t.Fatal("served /openapi.json missing /v1/webhooks/endpoints/{id} path")
	}
	if byID.Delete == nil {
		t.Error("/v1/webhooks/endpoints/{id} has no DELETE operation documented")
	}
	if byID.Patch == nil {
		t.Error("/v1/webhooks/endpoints/{id} has no PATCH operation documented")
	}

	rotate, ok := doc.Paths["/v1/webhooks/endpoints/{id}/rotate-secret"]
	if !ok {
		t.Fatal("served /openapi.json missing /v1/webhooks/endpoints/{id}/rotate-secret path")
	}
	if rotate.Post == nil {
		t.Error("/v1/webhooks/endpoints/{id}/rotate-secret has no POST operation documented")
	}
}
