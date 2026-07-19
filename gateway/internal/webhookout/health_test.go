package webhookout

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Unluckyathecking/crucible/gateway/internal/events"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
)

// endpointHealthSnapshot reads back webhook_endpoints' health-accounting
// columns for assertions.
type endpointHealthSnapshot struct {
	active              bool
	consecutiveFailures int
	disabledAt          *time.Time
	disabledReason      *string
}

func fetchEndpointHealth(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) endpointHealthSnapshot {
	t.Helper()
	var s endpointHealthSnapshot
	err := pool.QueryRow(context.Background(), `
		SELECT active, consecutive_failures, disabled_at, disabled_reason
		FROM webhook_endpoints WHERE id = $1
	`, id).Scan(&s.active, &s.consecutiveFailures, &s.disabledAt, &s.disabledReason)
	if err != nil {
		t.Fatalf("fetchEndpointHealth(%s): %v", id, err)
	}
	return s
}

// forceDisable directly sets an endpoint's row into the auto-disabled state,
// bypassing recordDeliveryFailure — used to set up EnableEndpoint tests
// without driving a real threshold crossing.
func forceDisable(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		UPDATE webhook_endpoints
		SET active = FALSE, disabled_at = NOW(), disabled_reason = $2, consecutive_failures = 5
		WHERE id = $1
	`, id, DisabledReasonDeliveryFailures)
	if err != nil {
		t.Fatalf("forceDisable(%s): %v", id, err)
	}
}

// pendingEndpointDisabledDeliveries counts webhook_deliveries rows enqueued
// for endpointID with event_type = events.EndpointDisabled.
func pendingEndpointDisabledDeliveries(t *testing.T, pool *pgxpool.Pool, endpointID uuid.UUID) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM webhook_deliveries
		WHERE endpoint_id = $1 AND event_type = $2
	`, endpointID, events.EndpointDisabled).Scan(&n)
	if err != nil {
		t.Fatalf("pendingEndpointDisabledDeliveries(%s): %v", endpointID, err)
	}
	return n
}

func TestRecordDeliverySuccess_ResetsCounter(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "health-success-"+uuid.New().String()+"@example.com")
	ep := seedEndpoint(t, pool, cust, "https://example.com/hook")

	if _, _, err := recordDeliveryFailure(context.Background(), pool, ep, 0); err != nil {
		t.Fatalf("seed failure: %v", err)
	}
	if got := fetchEndpointHealth(t, pool, ep).consecutiveFailures; got != 1 {
		t.Fatalf("precondition: consecutive_failures = %d, want 1", got)
	}

	if err := recordDeliverySuccess(context.Background(), pool, ep); err != nil {
		t.Fatalf("recordDeliverySuccess: %v", err)
	}
	if got := fetchEndpointHealth(t, pool, ep).consecutiveFailures; got != 0 {
		t.Errorf("consecutive_failures after success = %d, want 0", got)
	}
}

// TestRecordDeliveryFailure_IncrementsAndDisablesAtThreshold is the
// acceptance test for markDeadLetter's terminal path: N consecutive
// dead-letters flip active/disabled_at/disabled_reason together, in the
// call that crosses threshold — not before, not silently on a later call.
func TestRecordDeliveryFailure_IncrementsAndDisablesAtThreshold(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "health-threshold-"+uuid.New().String()+"@example.com")
	ep := seedEndpoint(t, pool, cust, "https://example.com/hook")
	const threshold = 3

	for i := 1; i < threshold; i++ {
		custID, justDisabled, err := recordDeliveryFailure(context.Background(), pool, ep, threshold)
		if err != nil {
			t.Fatalf("recordDeliveryFailure #%d: %v", i, err)
		}
		if justDisabled {
			t.Fatalf("recordDeliveryFailure #%d: justDisabled = true, want false (below threshold)", i)
		}
		if custID != cust {
			t.Fatalf("recordDeliveryFailure #%d: customerID = %s, want %s", i, custID, cust)
		}
		snap := fetchEndpointHealth(t, pool, ep)
		if !snap.active {
			t.Fatalf("recordDeliveryFailure #%d: endpoint went inactive below threshold", i)
		}
		if snap.consecutiveFailures != i {
			t.Fatalf("recordDeliveryFailure #%d: consecutive_failures = %d, want %d", i, snap.consecutiveFailures, i)
		}
	}

	// The threshold-th call crosses the line.
	custID, justDisabled, err := recordDeliveryFailure(context.Background(), pool, ep, threshold)
	if err != nil {
		t.Fatalf("recordDeliveryFailure (crossing): %v", err)
	}
	if !justDisabled {
		t.Fatal("recordDeliveryFailure (crossing): justDisabled = false, want true")
	}
	if custID != cust {
		t.Fatalf("recordDeliveryFailure (crossing): customerID = %s, want %s", custID, cust)
	}
	snap := fetchEndpointHealth(t, pool, ep)
	if snap.active {
		t.Fatal("endpoint still active after crossing threshold")
	}
	if snap.disabledAt == nil {
		t.Error("disabled_at is nil after crossing threshold")
	}
	if snap.disabledReason == nil || *snap.disabledReason != DisabledReasonDeliveryFailures {
		t.Errorf("disabled_reason = %v, want %q", snap.disabledReason, DisabledReasonDeliveryFailures)
	}
	if snap.consecutiveFailures != threshold {
		t.Errorf("consecutive_failures = %d, want %d", snap.consecutiveFailures, threshold)
	}

	// A further call against the now-disabled endpoint must not re-fire
	// (old.active is already false, so the CASE no-ops) and must not error.
	_, justDisabledAgain, err := recordDeliveryFailure(context.Background(), pool, ep, threshold)
	if err != nil {
		t.Fatalf("recordDeliveryFailure (post-disable): %v", err)
	}
	if justDisabledAgain {
		t.Error("recordDeliveryFailure fired justDisabled again on an already-disabled endpoint")
	}
}

// TestRecordDeliveryFailure_ThresholdZero_DisabledByDefault is the
// acceptance test for WEBHOOK_ENDPOINT_FAILURE_THRESHOLD's zero-config-safe
// default: threshold <= 0 must never auto-disable, no matter how many
// consecutive failures accumulate.
func TestRecordDeliveryFailure_ThresholdZero_DisabledByDefault(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "health-threshold-zero-"+uuid.New().String()+"@example.com")
	ep := seedEndpoint(t, pool, cust, "https://example.com/hook")

	for i := 1; i <= 10; i++ {
		_, justDisabled, err := recordDeliveryFailure(context.Background(), pool, ep, 0)
		if err != nil {
			t.Fatalf("recordDeliveryFailure #%d: %v", i, err)
		}
		if justDisabled {
			t.Fatalf("recordDeliveryFailure #%d: justDisabled = true with threshold=0", i)
		}
	}
	snap := fetchEndpointHealth(t, pool, ep)
	if !snap.active {
		t.Error("endpoint disabled despite threshold=0")
	}
	if snap.consecutiveFailures != 10 {
		t.Errorf("consecutive_failures = %d, want 10 (still counted, just never acted on)", snap.consecutiveFailures)
	}
}

func TestRecordDeliveryFailure_UnknownEndpoint_NoOp(t *testing.T) {
	pool := newTestPostgres(t)
	custID, justDisabled, err := recordDeliveryFailure(context.Background(), pool, uuid.New(), 1)
	if err != nil {
		t.Fatalf("recordDeliveryFailure: %v", err)
	}
	if justDisabled {
		t.Error("justDisabled = true for a nonexistent endpoint")
	}
	if custID != uuid.Nil {
		t.Errorf("customerID = %s, want uuid.Nil", custID)
	}
}

func TestEnableEndpoint_ReEnablesAutoDisabled(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "health-enable-ok-"+uuid.New().String()+"@example.com")
	ep := seedEndpoint(t, pool, cust, "https://example.com/hook")
	forceDisable(t, pool, ep)

	if err := EnableEndpoint(context.Background(), pool, ep, cust); err != nil {
		t.Fatalf("EnableEndpoint: %v", err)
	}

	snap := fetchEndpointHealth(t, pool, ep)
	if !snap.active {
		t.Error("endpoint still inactive after EnableEndpoint")
	}
	if snap.disabledAt != nil {
		t.Errorf("disabled_at = %v, want nil", snap.disabledAt)
	}
	if snap.disabledReason != nil {
		t.Errorf("disabled_reason = %v, want nil", snap.disabledReason)
	}
	if snap.consecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want 0", snap.consecutiveFailures)
	}
}

// TestEnableEndpoint_RejectsSoftDeleted asserts DeleteEndpoint's soft-delete
// (active = FALSE, disabled_reason NULL) is not revivable through
// EnableEndpoint — the two active=FALSE states must stay distinguishable.
func TestEnableEndpoint_RejectsSoftDeleted(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "health-enable-deleted-"+uuid.New().String()+"@example.com")
	ep := seedEndpoint(t, pool, cust, "https://example.com/hook")
	if err := DeleteEndpoint(context.Background(), pool, ep, cust); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}

	err := EnableEndpoint(context.Background(), pool, ep, cust)
	if err != ErrEndpointNotFound {
		t.Fatalf("EnableEndpoint on soft-deleted endpoint: err = %v, want ErrEndpointNotFound", err)
	}
	snap := fetchEndpointHealth(t, pool, ep)
	if snap.active {
		t.Error("soft-deleted endpoint became active via EnableEndpoint")
	}
}

func TestEnableEndpoint_OwnedByOtherCustomer_NotFound(t *testing.T) {
	pool := newTestPostgres(t)
	owner := seedCustomer(t, pool, "health-enable-owner-"+uuid.New().String()+"@example.com")
	attacker := seedCustomer(t, pool, "health-enable-attacker-"+uuid.New().String()+"@example.com")
	ep := seedEndpoint(t, pool, owner, "https://example.com/hook")
	forceDisable(t, pool, ep)

	if err := EnableEndpoint(context.Background(), pool, ep, attacker); err != ErrEndpointNotFound {
		t.Fatalf("EnableEndpoint (wrong customer): err = %v, want ErrEndpointNotFound", err)
	}
	if fetchEndpointHealth(t, pool, ep).active {
		t.Error("cross-customer EnableEndpoint must not have taken effect")
	}
}

func TestEnableEndpoint_NotFound(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "health-enable-notfound-"+uuid.New().String()+"@example.com")
	if err := EnableEndpoint(context.Background(), pool, uuid.New(), cust); err != ErrEndpointNotFound {
		t.Fatalf("EnableEndpoint: err = %v, want ErrEndpointNotFound", err)
	}
}

// TestMarkDeadLetter_AutoDisablesAtThreshold_FansOutEvent drives markDeadLetter
// directly (bypassing HTTP delivery and the multi-hour backoff schedule) to
// exercise the exact code path the real delivery worker takes: N consecutive
// dead-letters flip the endpoint's row, emit endpoint.disabled to the
// customer's other active endpoint (never to the one just disabled), and
// bump crucible_webhook_endpoints_disabled_total by exactly one.
func TestMarkDeadLetter_AutoDisablesAtThreshold_FansOutEvent(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "health-deadletter-"+uuid.New().String()+"@example.com")
	target := seedEndpoint(t, pool, cust, "https://example.com/hook-target")
	sibling := seedEndpoint(t, pool, cust, "https://example.com/hook-sibling")

	e := &Emitter{db: pool}
	e.SetFailureThreshold(2)

	before := testutil.ToFloat64(observability.WebhookEndpointsDisabledTotal)

	id1 := seedDelivery(t, pool, target, "delivering", seedDeliveryOpts{attempts: maxAttempts - 1})
	e.markDeadLetter(id1, maxAttempts, nil)
	if !fetchEndpointHealth(t, pool, target).active {
		t.Fatal("endpoint disabled after only 1 of 2 threshold dead-letters")
	}

	id2 := seedDelivery(t, pool, target, "delivering", seedDeliveryOpts{attempts: maxAttempts - 1})
	e.markDeadLetter(id2, maxAttempts, nil)

	snap := fetchEndpointHealth(t, pool, target)
	if snap.active {
		t.Fatal("endpoint still active after crossing the failure threshold")
	}
	if snap.disabledReason == nil || *snap.disabledReason != DisabledReasonDeliveryFailures {
		t.Errorf("disabled_reason = %v, want %q", snap.disabledReason, DisabledReasonDeliveryFailures)
	}
	if got := fetchDelivery(t, pool, id2).status; got != "dead_letter" {
		t.Errorf("delivery status = %q, want dead_letter", got)
	}

	if got := pendingEndpointDisabledDeliveries(t, pool, sibling); got != 1 {
		t.Errorf("sibling endpoint.disabled deliveries = %d, want 1", got)
	}
	if got := pendingEndpointDisabledDeliveries(t, pool, target); got != 0 {
		t.Errorf("just-disabled endpoint received its own endpoint.disabled delivery: count = %d, want 0", got)
	}

	after := testutil.ToFloat64(observability.WebhookEndpointsDisabledTotal)
	if after-before != 1 {
		t.Errorf("WebhookEndpointsDisabledTotal increment = %v, want 1", after-before)
	}
}

// TestMarkDeadLetter_DefaultThresholdZero_NeverDisables is the acceptance
// test proving WEBHOOK_ENDPOINT_FAILURE_THRESHOLD's zero-config-safe
// default (an Emitter with no SetFailureThreshold call, matching every
// existing production Emitter before this change) leaves dead-lettering
// behaviour byte-identical: no auto-disable, no endpoint.disabled fan-out,
// no matter how many consecutive dead-letters accumulate.
func TestMarkDeadLetter_DefaultThresholdZero_NeverDisables(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "health-deadletter-zero-"+uuid.New().String()+"@example.com")
	target := seedEndpoint(t, pool, cust, "https://example.com/hook-target")
	sibling := seedEndpoint(t, pool, cust, "https://example.com/hook-sibling")

	e := &Emitter{db: pool} // failureThreshold left at its zero value

	for i := 0; i < 10; i++ {
		id := seedDelivery(t, pool, target, "delivering", seedDeliveryOpts{attempts: maxAttempts - 1})
		e.markDeadLetter(id, maxAttempts, nil)
	}

	snap := fetchEndpointHealth(t, pool, target)
	if !snap.active {
		t.Error("endpoint auto-disabled despite WEBHOOK_ENDPOINT_FAILURE_THRESHOLD=0 (default)")
	}
	if snap.consecutiveFailures != 10 {
		t.Errorf("consecutive_failures = %d, want 10 (still counted, just never acted on)", snap.consecutiveFailures)
	}
	if got := pendingEndpointDisabledDeliveries(t, pool, sibling); got != 0 {
		t.Errorf("endpoint.disabled fan-out fired with threshold=0: count = %d, want 0", got)
	}
}

func TestMarkDelivered_ResetsConsecutiveFailures(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "health-delivered-"+uuid.New().String()+"@example.com")
	ep := seedEndpoint(t, pool, cust, "https://example.com/hook")
	if _, _, err := recordDeliveryFailure(context.Background(), pool, ep, 0); err != nil {
		t.Fatalf("seed failure: %v", err)
	}

	e := &Emitter{db: pool}
	id := seedDelivery(t, pool, ep, "delivering", seedDeliveryOpts{attempts: 0})
	e.markDelivered(id, ep, 1, 200)

	if got := fetchEndpointHealth(t, pool, ep).consecutiveFailures; got != 0 {
		t.Errorf("consecutive_failures after markDelivered = %d, want 0", got)
	}
	if got := fetchDelivery(t, pool, id).status; got != "delivered" {
		t.Errorf("delivery status = %q, want delivered", got)
	}
}

// --- HTTP handler tests -----------------------------------------------------

// newEnableEndpointRouter wires just the routes EnableEndpointHandler tests
// need: create (to seed an endpoint through the same path production traffic
// uses) and enable itself.
func newEnableEndpointRouter(pool *pgxpool.Pool) http.Handler {
	r := chi.NewRouter()
	r.Post("/v1/webhooks/endpoints", CreateEndpointHandler(pool, testCustomerIDFunc))
	r.Post("/v1/webhooks/endpoints/{id}/enable", EnableEndpointHandler(pool, testCustomerIDFunc))
	return r
}

func enableEndpointReq(customerID, id uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/endpoints/"+id.String()+"/enable", nil)
	return req.WithContext(testKeyContext(customerID))
}

func TestEnableEndpointHandler_Success(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "enable-http-ok@example.com")
	r := newEnableEndpointRouter(pool)

	createRec := httptest.NewRecorder()
	raw, _ := json.Marshal(map[string]any{"url": "https://example.com/hook"})
	r.ServeHTTP(createRec, httptest.NewRequest(http.MethodPost, "/v1/webhooks/endpoints", bytes.NewReader(raw)).WithContext(testKeyContext(cust)))
	var created EndpointCreated
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	forceDisable(t, pool, created.ID)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, enableEndpointReq(cust, created.ID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body = %s", rec.Code, rec.Body.String())
	}
	if !fetchEndpointHealth(t, pool, created.ID).active {
		t.Error("endpoint still inactive after successful enable request")
	}
}

func TestEnableEndpointHandler_NotCurrentlyDisabled_404(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "enable-http-active@example.com")
	ep := seedEndpoint(t, pool, cust, "https://example.com/hook")
	r := newEnableEndpointRouter(pool)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, enableEndpointReq(cust, ep))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (endpoint is active, not auto-disabled)", rec.Code)
	}
}

func TestEnableEndpointHandler_OwnedByOtherCustomer_404(t *testing.T) {
	pool := newTestPostgres(t)
	owner := seedCustomer(t, pool, "enable-http-owner@example.com")
	attacker := seedCustomer(t, pool, "enable-http-attacker@example.com")
	ep := seedEndpoint(t, pool, owner, "https://example.com/hook")
	forceDisable(t, pool, ep)
	r := newEnableEndpointRouter(pool)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, enableEndpointReq(attacker, ep))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (IDOR-safe)", rec.Code)
	}
	if fetchEndpointHealth(t, pool, ep).active {
		t.Error("cross-customer enable request must not have taken effect")
	}
}

func TestEnableEndpointHandler_InvalidID(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "enable-http-invalid@example.com")
	r := newEnableEndpointRouter(pool)

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/endpoints/not-a-uuid/enable", nil).WithContext(testKeyContext(cust))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestEnableEndpointHandler_NoAuth(t *testing.T) {
	pool := newTestPostgres(t)
	r := newEnableEndpointRouter(pool)

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/endpoints/"+uuid.New().String()+"/enable", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
