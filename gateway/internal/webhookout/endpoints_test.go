package webhookout

import (
	"bytes"
	"context"
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
// openapi.webhookEndpointsPathItems) documents all three routes this PR adds.
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
}
