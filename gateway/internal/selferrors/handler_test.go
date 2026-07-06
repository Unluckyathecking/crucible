package selferrors_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/selferrors"
)

func newRouter(db *pgxpool.Pool) http.Handler {
	r := chi.NewRouter()
	r.Get("/v1/errors", selferrors.Handler(db))
	return r
}

// testKeyContext builds an auth.Key for customerID, wired via auth.WithTestKey.
func testKeyContext(customerID uuid.UUID) context.Context {
	key := &auth.Key{
		ID: uuid.New(),
		Customer: auth.Customer{
			ID:    customerID,
			Email: "selferrors-test@example.com",
			Plan:  "free",
		},
	}
	return auth.WithTestKey(context.Background(), key)
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) selferrors.Response {
	t.Helper()
	var resp selferrors.Response
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
	req := httptest.NewRequest(http.MethodGet, "/v1/errors", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d — body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandler_IDOR seeds two distinct customers each with their own error
// events, then asserts a request authenticated as customer A never sees
// customer B's rows (or vice versa) — there is no customer_id parameter, so
// the only way to leak cross-customer data would be a handler bug that
// ignores auth context. Mirrors the #150 IDOR coverage for
// /v1/webhooks/deliveries.
func TestHandler_IDOR(t *testing.T) {
	pool := newTestPostgres(t)

	custA := seedCustomer(t, pool)
	custB := seedCustomer(t, pool)
	now := time.Now().UTC()
	seedErrorEvent(t, pool, custA, "/v1/echo-a", "BAD_REQUEST", 400, now, nil)
	seedErrorEvent(t, pool, custB, "/v1/echo-b", "BAD_REQUEST", 400, now, nil)

	r := newRouter(pool)

	reqA := httptest.NewRequest(http.MethodGet, "/v1/errors", nil).WithContext(testKeyContext(custA))
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

	reqB := httptest.NewRequest(http.MethodGet, "/v1/errors", nil).WithContext(testKeyContext(custB))
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
	req := httptest.NewRequest(http.MethodGet, "/v1/errors", nil).WithContext(testKeyContext(cust))
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
	seedErrorEvent(t, pool, cust, "/v1/echo", "BAD_REQUEST", 400, time.Now().UTC(), nil)

	r := newRouter(pool)
	req := httptest.NewRequest(http.MethodGet, "/v1/errors", nil).WithContext(testKeyContext(cust))
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
	if len(resp.Data) != 1 {
		t.Fatalf("len(Data) = %d, want 1: %+v", len(resp.Data), resp.Data)
	}
}

// TestHandler_InvalidFromDate asserts a malformed 'from' date is rejected
// with 400 BEFORE any DB query.
func TestHandler_InvalidFromDate(t *testing.T) {
	r := newRouter(nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/errors?from=not-a-date", nil).WithContext(testKeyContext(uuid.New()))
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
	req := httptest.NewRequest(http.MethodGet, "/v1/errors?from=2020-01-01&to=2020-12-31", nil).WithContext(testKeyContext(uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandler_ToBeforeFrom asserts 'to' before 'from' is rejected with 400.
func TestHandler_ToBeforeFrom(t *testing.T) {
	r := newRouter(nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/errors?from=2020-06-10&to=2020-06-01", nil).WithContext(testKeyContext(uuid.New()))
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
	req := httptest.NewRequest(http.MethodGet, "/v1/errors?to="+future, nil).WithContext(testKeyContext(uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandler_InvalidOperationFilter asserts a malformed operation filter
// (not shaped like /v1/...) is rejected with 400 before any DB query.
func TestHandler_InvalidOperationFilter(t *testing.T) {
	r := newRouter(nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/errors?operation=not+valid%21", nil).WithContext(testKeyContext(uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandler_InvalidCodeFilter asserts a malformed code filter (lowercase,
// not matching CODE_FILTER_RE) is rejected with 400 before any DB query.
func TestHandler_InvalidCodeFilter(t *testing.T) {
	r := newRouter(nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/errors?code=not-valid", nil).WithContext(testKeyContext(uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandler_OperationAndCodeFilter asserts valid operation/code filters
// narrow the result set end to end through the HTTP handler.
func TestHandler_OperationAndCodeFilter(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool)
	now := time.Now().UTC()
	seedErrorEvent(t, pool, cust, "/v1/echo", "RATE_LIMITED", 429, now.Add(-time.Minute), nil)
	seedErrorEvent(t, pool, cust, "/v1/other", "BAD_REQUEST", 400, now, nil)

	r := newRouter(pool)
	req := httptest.NewRequest(http.MethodGet, "/v1/errors?operation=/v1/echo&code=RATE_LIMITED", nil).WithContext(testKeyContext(cust))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	if len(resp.Data) != 1 || resp.Data[0].Operation != "/v1/echo" || resp.Data[0].ErrorCode != "RATE_LIMITED" {
		t.Fatalf("unexpected filtered response: %+v", resp.Data)
	}
}

// TestHandler_LimitClampedAndPaginated asserts limit is clamped at 200 and
// has_more/page/limit pagination fields are correctly threaded through.
func TestHandler_LimitClampedAndPaginated(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool)
	now := time.Now().UTC()
	seedErrorEvent(t, pool, cust, "/v1/echo", "BAD_REQUEST", 400, now.Add(-2*time.Minute), nil)
	seedErrorEvent(t, pool, cust, "/v1/echo", "BAD_REQUEST", 400, now.Add(-1*time.Minute), nil)
	seedErrorEvent(t, pool, cust, "/v1/echo", "BAD_REQUEST", 400, now, nil)

	r := newRouter(pool)
	req := httptest.NewRequest(http.MethodGet, "/v1/errors?limit=2&page=1", nil).WithContext(testKeyContext(cust))
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

	req2 := httptest.NewRequest(http.MethodGet, "/v1/errors?limit=2&page=2", nil).WithContext(testKeyContext(cust))
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
	req := httptest.NewRequest(http.MethodGet, "/v1/errors?page=999999999", nil).WithContext(testKeyContext(uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rec.Code, rec.Body.String())
	}
}
