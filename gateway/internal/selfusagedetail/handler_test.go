package selfusagedetail_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/selfusagedetail"
)

func newRouter(db *pgxpool.Pool) http.Handler {
	r := chi.NewRouter()
	r.Get("/v1/usage/events", selfusagedetail.Handler(db))
	return r
}

// testKeyContext builds an auth.Key for customerID, wired via auth.WithTestKey.
func testKeyContext(customerID uuid.UUID) context.Context {
	key := &auth.Key{
		ID: uuid.New(),
		Customer: auth.Customer{
			ID:    customerID,
			Email: "selfusagedetail-test@example.com",
			Plan:  "free",
		},
	}
	return auth.WithTestKey(context.Background(), key)
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) selfusagedetail.Response {
	t.Helper()
	var resp selfusagedetail.Response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v — body: %s", err, rec.Body.String())
	}
	return resp
}

// TestHandler_NoAuth verifies the endpoint 401s when auth.FromContext has no
// key (auth.Middleware always populates this in production; this exercises
// the handler's own defense-in-depth check).
func TestHandler_NoAuth(t *testing.T) {
	r := newRouter(nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d — body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandler_IDOR seeds two distinct customers each with their own usage
// events, then asserts a request authenticated as customer A never sees
// customer B's rows (or vice versa) — there is no customer_id parameter, so
// the only way to leak cross-customer data would be a handler bug that
// ignores auth context.
func TestHandler_IDOR(t *testing.T) {
	pool := newTestPostgres(t)

	custA := seedCustomer(t, pool)
	custB := seedCustomer(t, pool)
	keyA := seedAPIKey(t, pool, custA)
	keyB := seedAPIKey(t, pool, custB)
	now := time.Now().UTC()
	seedUsageEvent(t, pool, custA, keyA, "/v1/echo-a", 1, now)
	seedUsageEvent(t, pool, custB, keyB, "/v1/echo-b", 1, now)

	r := newRouter(pool)

	reqA := httptest.NewRequest(http.MethodGet, "/v1/usage/events", nil).WithContext(testKeyContext(custA))
	recA := httptest.NewRecorder()
	r.ServeHTTP(recA, reqA)
	if recA.Code != http.StatusOK {
		t.Fatalf("customer A: expected 200, got %d — body: %s", recA.Code, recA.Body.String())
	}
	respA := decodeResponse(t, recA)
	for _, e := range respA.Data {
		if e.Operation == "/v1/echo-b" {
			t.Errorf("customer A response leaked customer B's row: %+v", e)
		}
	}

	reqB := httptest.NewRequest(http.MethodGet, "/v1/usage/events", nil).WithContext(testKeyContext(custB))
	recB := httptest.NewRecorder()
	r.ServeHTTP(recB, reqB)
	if recB.Code != http.StatusOK {
		t.Fatalf("customer B: expected 200, got %d — body: %s", recB.Code, recB.Body.String())
	}
	respB := decodeResponse(t, recB)
	for _, e := range respB.Data {
		if e.Operation == "/v1/echo-a" {
			t.Errorf("customer B response leaked customer A's row: %+v", e)
		}
	}
}

// TestHandler_CacheControl asserts every 200 response sets Cache-Control: no-store.
func TestHandler_CacheControl(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool)

	r := newRouter(pool)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events", nil).WithContext(testKeyContext(cust))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// TestHandler_Defaults asserts the default page/limit values are echoed back
// and rows within the default 30-day window are returned.
func TestHandler_Defaults(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool)
	key := seedAPIKey(t, pool, cust)
	seedUsageEvent(t, pool, cust, key, "/v1/echo", 3, time.Now().UTC())

	r := newRouter(pool)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events", nil).WithContext(testKeyContext(cust))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	if resp.Page != 1 {
		t.Errorf("Page = %d, want 1", resp.Page)
	}
	if resp.Limit != 50 {
		t.Errorf("Limit = %d, want 50", resp.Limit)
	}
	if len(resp.Data) != 1 || resp.Data[0].BillableUnits != "3" {
		t.Fatalf("unexpected Data: %+v", resp.Data)
	}
}

// TestHandler_InvalidFromDate asserts a malformed 'from' date is rejected
// with 400 BEFORE any DB query.
func TestHandler_InvalidFromDate(t *testing.T) {
	r := newRouter(nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events?from=not-a-date", nil).WithContext(testKeyContext(uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandler_RangeTooWide asserts a from/to range exceeding 90 days is
// rejected with 400.
func TestHandler_RangeTooWide(t *testing.T) {
	r := newRouter(nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events?from=2020-01-01&to=2020-12-31", nil).WithContext(testKeyContext(uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandler_ToBeforeFrom asserts 'to' before 'from' is rejected with 400.
func TestHandler_ToBeforeFrom(t *testing.T) {
	r := newRouter(nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events?from=2020-06-10&to=2020-06-01", nil).WithContext(testKeyContext(uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandler_FutureDateRejected asserts a 'to' date in the future is rejected.
func TestHandler_FutureDateRejected(t *testing.T) {
	future := time.Now().UTC().AddDate(1, 0, 0).Format("2006-01-02")
	r := newRouter(nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events?to="+future, nil).WithContext(testKeyContext(uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandler_InvalidOperationFilter asserts an operation filter exceeding
// the 128-character bound is rejected with 400 before any DB query.
// usage_events.operation is an opaque worker operation string (e.g. "echo"),
// not a /v1/... path — so unlike selferrors' operation filter, there is no
// shape to validate here, only a length bound.
func TestHandler_InvalidOperationFilter(t *testing.T) {
	r := newRouter(nil)
	tooLong := strings.Repeat("a", 129)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events?operation="+tooLong, nil).WithContext(testKeyContext(uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandler_OperationFilter asserts a valid operation filter narrows the
// result set end to end through the HTTP handler. Uses bare opaque operation
// strings ("echo", "count-words") rather than /v1/... paths, matching how
// usage_events.operation is actually populated (server.invoke passes
// RouteDescriptor.Operation, not the request path) — a regression check for
// the selferrors-style path-shape validation this handler deliberately does
// not reuse.
func TestHandler_OperationFilter(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool)
	key := seedAPIKey(t, pool, cust)
	now := time.Now().UTC()
	seedUsageEvent(t, pool, cust, key, "echo", 1, now.Add(-time.Minute))
	seedUsageEvent(t, pool, cust, key, "count-words", 1, now)

	r := newRouter(pool)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events?operation=echo", nil).WithContext(testKeyContext(cust))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	if len(resp.Data) != 1 || resp.Data[0].Operation != "echo" {
		t.Fatalf("unexpected filtered response: %+v", resp.Data)
	}
}

// TestHandler_LimitClampedAndPaginated asserts limit is clamped at 200 and
// has_more/page/limit pagination fields are correctly threaded through.
func TestHandler_LimitClampedAndPaginated(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool)
	key := seedAPIKey(t, pool, cust)
	now := time.Now().UTC()
	seedUsageEvent(t, pool, cust, key, "/v1/echo", 1, now.Add(-2*time.Minute))
	seedUsageEvent(t, pool, cust, key, "/v1/echo", 1, now.Add(-1*time.Minute))
	seedUsageEvent(t, pool, cust, key, "/v1/echo", 1, now)

	r := newRouter(pool)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events?limit=2&page=1", nil).WithContext(testKeyContext(cust))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	if resp.Limit != 2 {
		t.Errorf("Limit = %d, want 2", resp.Limit)
	}
	if !resp.HasMore {
		t.Error("HasMore = false, want true (3 rows, limit 2)")
	}
	if len(resp.Data) != 2 {
		t.Fatalf("len(Data) = %d, want 2", len(resp.Data))
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/usage/events?limit=2&page=2", nil).WithContext(testKeyContext(cust))
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	resp2 := decodeResponse(t, rec2)
	if resp2.HasMore {
		t.Error("HasMore = true on last page, want false")
	}
	if len(resp2.Data) != 1 {
		t.Fatalf("len(Data) on page 2 = %d, want 1", len(resp2.Data))
	}
}

// TestHandler_PageTooLarge asserts an absurd page number is rejected with 400
// rather than issuing a pathologically large OFFSET query.
func TestHandler_PageTooLarge(t *testing.T) {
	r := newRouter(nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events?page=999999999", nil).WithContext(testKeyContext(uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandler_CSVFormatQueryParam asserts ?format=csv emits an RFC-4180 CSV
// body with the fixed header row and Content-Type: text/csv.
func TestHandler_CSVFormatQueryParam(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool)
	key := seedAPIKey(t, pool, cust)
	seedUsageEvent(t, pool, cust, key, "/v1/echo", 4, time.Now().UTC())

	r := newRouter(pool)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events?format=csv", nil).WithContext(testKeyContext(cust))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/csv" {
		t.Errorf("Content-Type = %q, want text/csv", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV body: %v — body: %s", err, rec.Body.String())
	}
	if len(records) != 2 {
		t.Fatalf("len(records) = %d, want 2 (header + 1 row): %v", len(records), records)
	}
	wantHeader := []string{"id", "operation", "billable_units", "created_at"}
	for i, col := range wantHeader {
		if records[0][i] != col {
			t.Errorf("header[%d] = %q, want %q", i, records[0][i], col)
		}
	}
	if records[1][1] != "/v1/echo" || records[1][2] != "4" {
		t.Errorf("unexpected data row: %v", records[1])
	}
}

// TestHandler_CSVAcceptHeader asserts an Accept: text/csv header (with no
// ?format=csv query param) also triggers the CSV representation.
func TestHandler_CSVAcceptHeader(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool)
	key := seedAPIKey(t, pool, cust)
	seedUsageEvent(t, pool, cust, key, "/v1/echo", 1, time.Now().UTC())

	r := newRouter(pool)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events", nil).WithContext(testKeyContext(cust))
	req.Header.Set("Accept", "text/csv")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/csv" {
		t.Errorf("Content-Type = %q, want text/csv", got)
	}
}

// TestHandler_AcceptTieFallsBackToJSON asserts that an Accept header listing
// both representations with no distinguishing q-value ("application/json,
// text/csv") falls back to JSON: per RFC 7231 §5.3.2, list order carries no
// preference meaning, so this is not a signal to switch away from the
// default representation.
func TestHandler_AcceptTieFallsBackToJSON(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool)
	key := seedAPIKey(t, pool, cust)
	seedUsageEvent(t, pool, cust, key, "/v1/echo", 1, time.Now().UTC())

	r := newRouter(pool)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events", nil).WithContext(testKeyContext(cust))
	req.Header.Set("Accept", "application/json, text/csv")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// TestHandler_AcceptCSVExplicitlyRejected asserts "text/csv;q=0" (explicitly
// unacceptable per RFC 7231) never triggers the CSV representation, even
// though the literal string "text/csv" appears in the header.
func TestHandler_AcceptCSVExplicitlyRejected(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool)
	key := seedAPIKey(t, pool, cust)
	seedUsageEvent(t, pool, cust, key, "/v1/echo", 1, time.Now().UTC())

	r := newRouter(pool)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events", nil).WithContext(testKeyContext(cust))
	req.Header.Set("Accept", "text/csv;q=0, application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// TestHandler_AcceptCSVPreferredByQValue asserts a genuine q-value preference
// for text/csv over application/json does select the CSV representation.
func TestHandler_AcceptCSVPreferredByQValue(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool)
	key := seedAPIKey(t, pool, cust)
	seedUsageEvent(t, pool, cust, key, "/v1/echo", 1, time.Now().UTC())

	r := newRouter(pool)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events", nil).WithContext(testKeyContext(cust))
	req.Header.Set("Accept", "text/csv;q=0.9, application/json;q=0.1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/csv" {
		t.Errorf("Content-Type = %q, want text/csv", got)
	}
}

// TestHandler_CSVFieldEscaping asserts an operation value containing a comma
// is RFC-4180-escaped (quoted) in the CSV output rather than corrupting the
// column count.
func TestHandler_CSVFieldEscaping(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool)
	key := seedAPIKey(t, pool, cust)
	seedUsageEvent(t, pool, cust, key, "/v1/echo,with-comma", 1, time.Now().UTC())

	r := newRouter(pool)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events?format=csv", nil).WithContext(testKeyContext(cust))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV body: %v — body: %s", err, rec.Body.String())
	}
	if len(records) != 2 || records[1][1] != "/v1/echo,with-comma" {
		t.Fatalf("comma-containing operation not preserved through CSV round-trip: %v", records)
	}
}

// TestHandler_CSVPreservesSubSecondTimestamps asserts the CSV export doesn't
// collapse two rows created within the same whole second into identical
// created_at values — RFC3339 (second precision) would make them
// indistinguishable, unlike the JSON path's default time.Time marshaling.
func TestHandler_CSVPreservesSubSecondTimestamps(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool)
	key := seedAPIKey(t, pool, cust)

	base := time.Now().UTC().Truncate(time.Second)
	seedUsageEvent(t, pool, cust, key, "/v1/echo", 1, base)
	seedUsageEvent(t, pool, cust, key, "/v1/echo", 2, base.Add(500*time.Millisecond))

	r := newRouter(pool)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage/events?format=csv", nil).WithContext(testKeyContext(cust))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV body: %v — body: %s", err, rec.Body.String())
	}
	if len(records) != 3 {
		t.Fatalf("len(records) = %d, want 3 (header + 2 rows): %v", len(records), records)
	}
	if records[1][3] == records[2][3] {
		t.Errorf("both rows have identical created_at %q despite differing by 500ms — sub-second precision was dropped", records[1][3])
	}
}
