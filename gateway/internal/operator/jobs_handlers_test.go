package operator_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/jobs"
	"github.com/Unluckyathecking/crucible/gateway/internal/operator"
)

const testJobsOperatorToken = "test-operator-token-jobs-admin"

// jobsAdminTestAdapter mirrors server.jobsAdminAdapter exactly (see its doc
// comment for why operator can't import jobs directly): a small translation
// shim from the real jobs.Store to operator.JobsAdminStore, so these tests
// exercise the actual admin store queries rather than a hand-rolled fake.
type jobsAdminTestAdapter struct{ store *jobs.Store }

func (a jobsAdminTestAdapter) AdminList(ctx context.Context, status *string, limit, offset int) ([]operator.AdminJob, int64, error) {
	rows, total, err := a.store.AdminList(ctx, status, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	out := make([]operator.AdminJob, len(rows))
	for i, j := range rows {
		out[i] = adminJobFromRow(j)
	}
	return out, total, nil
}

func (a jobsAdminTestAdapter) AdminGet(ctx context.Context, id uuid.UUID) (operator.AdminJob, bool, error) {
	j, ok, err := a.store.AdminGet(ctx, id)
	if err != nil || !ok {
		return operator.AdminJob{}, ok, err
	}
	return adminJobFromRow(j), true, nil
}

func (a jobsAdminTestAdapter) Requeue(ctx context.Context, id uuid.UUID) error {
	return a.store.Requeue(ctx, id)
}

func (a jobsAdminTestAdapter) ReleaseClaimed(ctx context.Context, instanceID uuid.UUID) (int64, error) {
	return a.store.ReleaseClaimed(ctx, instanceID)
}

func adminJobFromRow(j jobs.AdminJob) operator.AdminJob {
	return operator.AdminJob{
		ID: j.ID, CustomerID: j.CustomerID, Operation: j.Operation, Status: j.Status,
		Result: j.Result, UnitsLabel: j.UnitsLabel, BillableUnits: j.BillableUnits,
		ErrorCode: j.ErrorCode, ErrorMessage: j.ErrorMessage,
		ClaimedBy: j.ClaimedBy, ClaimedAt: j.ClaimedAt,
		CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt,
	}
}

// newJobsAdminRouter mirrors the exact nesting routes.go uses for the
// operator-token-gated /v1/admin subrouter: operator.Middleware wraps the
// jobs admin routes exactly as it wraps every other /v1/admin route.
func newJobsAdminRouter(pool *pgxpool.Pool) http.Handler {
	adapter := jobsAdminTestAdapter{store: jobs.NewStore(pool)}
	r := chi.NewRouter()
	r.Route("/v1/admin", func(r chi.Router) {
		r.Use(operator.Middleware(testJobsOperatorToken))
		r.Get("/jobs", operator.AdminListJobsHandler(adapter))
		r.Get("/jobs/{id}", operator.AdminGetJobHandler(adapter))
		r.Post("/jobs/{id}/requeue", operator.AdminRequeueJobHandler(adapter, pool))
		r.Post("/jobs/release", operator.AdminReleaseJobsHandler(adapter, pool))
	})
	return r
}

func doJobsAdminRequest(t *testing.T, h http.Handler, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// seedJobsCustomer inserts a minimal customers + api_keys row pair, needed to
// satisfy async_jobs' FK constraints (mirrors jobs.seedCustomer, duplicated
// here since that helper is unexported to package jobs).
func seedJobsCustomer(t *testing.T, pool *pgxpool.Pool, email string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var custID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO customers (email, plan_id) VALUES ($1, 'free') RETURNING id`, email,
	).Scan(&custID); err != nil {
		t.Fatalf("seedJobsCustomer: %v", err)
	}
	var keyID uuid.UUID
	prefix := "cru_test_" + uuid.New().String()[:8]
	if err := pool.QueryRow(ctx,
		`INSERT INTO api_keys (customer_id, prefix, hash) VALUES ($1, $2, '\x00') RETURNING id`, custID, prefix,
	).Scan(&keyID); err != nil {
		t.Fatalf("seedJobsCustomer: insert api_key: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM async_jobs WHERE customer_id = $1`, custID)
		_, _ = pool.Exec(ctx, `DELETE FROM customers WHERE id = $1`, custID)
	})
	return custID, keyID
}

func countJobsAuditRows(t *testing.T, pool *pgxpool.Pool, action, targetType, targetID string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_log WHERE action = $1 AND target_type = $2 AND target_id = $3`,
		action, targetType, targetID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("countJobsAuditRows: %v", err)
	}
	return n
}

func TestAdminListJobsHandler_CrossCustomer(t *testing.T) {
	pool := newTestPostgres(t)
	custA, keyA := seedJobsCustomer(t, pool, "jobs-admin-http-list-a-"+uuid.New().String()+"@example.com")
	custB, keyB := seedJobsCustomer(t, pool, "jobs-admin-http-list-b-"+uuid.New().String()+"@example.com")
	store := jobs.NewStore(pool)
	ctx := context.Background()
	idA, err := store.Enqueue(ctx, custA, keyA, "echo", "req-a", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue (custA): %v", err)
	}
	idB, err := store.Enqueue(ctx, custB, keyB, "echo", "req-b", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue (custB): %v", err)
	}

	h := newJobsAdminRouter(pool)
	rec := doJobsAdminRequest(t, h, http.MethodGet, "/v1/admin/jobs?per_page=100", testJobsOperatorToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var page struct {
		Items []struct {
			JobID      string `json:"job_id"`
			CustomerID string `json:"customer_id"`
		} `json:"items"`
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	seen := map[string]bool{}
	for _, it := range page.Items {
		seen[it.JobID] = true
	}
	if !seen[idA.String()] || !seen[idB.String()] {
		t.Errorf("cross-customer list missing jobs: seen=%v want %s and %s", seen, idA, idB)
	}
}

func TestAdminListJobsHandler_InvalidStatus(t *testing.T) {
	pool := newTestPostgres(t)
	h := newJobsAdminRouter(pool)
	rec := doJobsAdminRequest(t, h, http.MethodGet, "/v1/admin/jobs?status=bogus", testJobsOperatorToken, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminGetJobHandler_OK(t *testing.T) {
	pool := newTestPostgres(t)
	custA, keyA := seedJobsCustomer(t, pool, "jobs-admin-http-get-"+uuid.New().String()+"@example.com")
	store := jobs.NewStore(pool)
	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	h := newJobsAdminRouter(pool)
	rec := doJobsAdminRequest(t, h, http.MethodGet, "/v1/admin/jobs/"+id.String(), testJobsOperatorToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	var body struct {
		JobID      string `json:"job_id"`
		CustomerID string `json:"customer_id"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.JobID != id.String() || body.CustomerID != custA.String() || body.Status != jobs.StatusQueued {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestAdminGetJobHandler_NotFound(t *testing.T) {
	pool := newTestPostgres(t)
	h := newJobsAdminRouter(pool)
	rec := doJobsAdminRequest(t, h, http.MethodGet, "/v1/admin/jobs/"+uuid.New().String(), testJobsOperatorToken, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminGetJobHandler_InvalidID(t *testing.T) {
	pool := newTestPostgres(t)
	h := newJobsAdminRouter(pool)
	rec := doJobsAdminRequest(t, h, http.MethodGet, "/v1/admin/jobs/not-a-uuid", testJobsOperatorToken, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminRequeueJobHandler_OK(t *testing.T) {
	pool := newTestPostgres(t)
	custA, keyA := seedJobsCustomer(t, pool, "jobs-admin-http-requeue-"+uuid.New().String()+"@example.com")
	store := jobs.NewStore(pool)
	ctx := context.Background()
	id, err := store.Enqueue(ctx, custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Put the job into terminal 'failed' state — the legitimate requeue path.
	// Claim first (Fail requires a running row in practice) then fail it.
	if _, err := store.Claim(ctx, 10, uuid.New()); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.Fail(ctx, id, "TEST_ERROR", "seeded for requeue test"); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	h := newJobsAdminRouter(pool)
	rec := doJobsAdminRequest(t, h, http.MethodPost, "/v1/admin/jobs/"+id.String()+"/requeue", testJobsOperatorToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != jobs.StatusQueued {
		t.Errorf("status = %q, want %q", body.Status, jobs.StatusQueued)
	}

	job, ok, err := store.Get(ctx, id, custA)
	if err != nil || !ok {
		t.Fatalf("Get after requeue: ok=%v err=%v", ok, err)
	}
	if job.Status != jobs.StatusQueued {
		t.Errorf("stored status after requeue = %q, want %q", job.Status, jobs.StatusQueued)
	}
	if n := countJobsAuditRows(t, pool, operator.ActionJobRequeued, "async_job", id.String()); n != 1 {
		t.Errorf("audit rows for job %s: got %d, want 1", id, n)
	}
}

func TestAdminRequeueJobHandler_RejectRunning(t *testing.T) {
	pool := newTestPostgres(t)
	custA, keyA := seedJobsCustomer(t, pool, "jobs-admin-http-requeue-running-"+uuid.New().String()+"@example.com")
	store := jobs.NewStore(pool)
	ctx := context.Background()
	id, err := store.Enqueue(ctx, custA, keyA, "echo", "req-running", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := store.Claim(ctx, 10, uuid.New()); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	h := newJobsAdminRouter(pool)
	rec := doJobsAdminRequest(t, h, http.MethodPost, "/v1/admin/jobs/"+id.String()+"/requeue", testJobsOperatorToken, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Code != "JOB_NOT_REQUEUABLE" {
		t.Errorf("error code = %q, want JOB_NOT_REQUEUABLE", body.Code)
	}
	if n := countJobsAuditRows(t, pool, operator.ActionJobRequeued, "async_job", id.String()); n != 0 {
		t.Errorf("audit rows after rejected requeue = %d, want 0", n)
	}
}

func TestAdminRequeueJobHandler_RejectSucceeded(t *testing.T) {
	pool := newTestPostgres(t)
	custA, keyA := seedJobsCustomer(t, pool, "jobs-admin-http-requeue-succeeded-"+uuid.New().String()+"@example.com")
	store := jobs.NewStore(pool)
	ctx := context.Background()
	id, err := store.Enqueue(ctx, custA, keyA, "echo", "req-succeeded", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Force the job directly to 'succeeded' via SQL — jobs.Store has no exported
	// Complete that accepts a pool-backed call without a real worker response.
	if _, err := pool.Exec(ctx, `UPDATE async_jobs SET status = 'succeeded', claimed_at = NULL, claimed_by = NULL WHERE id = $1`, id); err != nil {
		t.Fatalf("force succeeded: %v", err)
	}

	h := newJobsAdminRouter(pool)
	rec := doJobsAdminRequest(t, h, http.MethodPost, "/v1/admin/jobs/"+id.String()+"/requeue", testJobsOperatorToken, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Code != "JOB_NOT_REQUEUABLE" {
		t.Errorf("error code = %q, want JOB_NOT_REQUEUABLE", body.Code)
	}
	if n := countJobsAuditRows(t, pool, operator.ActionJobRequeued, "async_job", id.String()); n != 0 {
		t.Errorf("audit rows after rejected requeue = %d, want 0", n)
	}
}

func TestAdminRequeueJobHandler_NotFound(t *testing.T) {
	pool := newTestPostgres(t)
	h := newJobsAdminRouter(pool)
	rec := doJobsAdminRequest(t, h, http.MethodPost, "/v1/admin/jobs/"+uuid.New().String()+"/requeue", testJobsOperatorToken, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminRequeueJobHandler_InvalidID(t *testing.T) {
	pool := newTestPostgres(t)
	h := newJobsAdminRouter(pool)
	rec := doJobsAdminRequest(t, h, http.MethodPost, "/v1/admin/jobs/not-a-uuid/requeue", testJobsOperatorToken, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestAdminReleaseJobsHandler_ScopedToInstance mirrors
// jobs.TestStore_ReleaseClaimed_ScopedToInstance at the HTTP layer: releasing
// one instance's claimed jobs must never touch another instance's in-flight work.
func TestAdminReleaseJobsHandler_ScopedToInstance(t *testing.T) {
	pool := newTestPostgres(t)
	custA, keyA := seedJobsCustomer(t, pool, "jobs-admin-http-release-"+uuid.New().String()+"@example.com")
	store := jobs.NewStore(pool)
	ctx := context.Background()

	idA, err := store.Enqueue(ctx, custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	idB, err := store.Enqueue(ctx, custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	instanceA := uuid.New()
	instanceB := uuid.New()
	if _, err := store.Claim(ctx, 1, instanceA); err != nil {
		t.Fatalf("Claim A: %v", err)
	}
	if _, err := store.Claim(ctx, 1, instanceB); err != nil {
		t.Fatalf("Claim B: %v", err)
	}

	h := newJobsAdminRouter(pool)
	reqBody, _ := json.Marshal(map[string]string{"instance_id": instanceA.String()})
	rec := doJobsAdminRequest(t, h, http.MethodPost, "/v1/admin/jobs/release", testJobsOperatorToken, reqBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Released int64 `json:"released"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Released != 1 {
		t.Errorf("released = %d, want 1", body.Released)
	}

	var queuedCount, runningCount int
	for _, id := range []uuid.UUID{idA, idB} {
		job, ok, err := store.Get(ctx, id, custA)
		if err != nil || !ok {
			t.Fatalf("Get(%s): ok=%v err=%v", id, ok, err)
		}
		switch job.Status {
		case jobs.StatusQueued:
			queuedCount++
		case jobs.StatusRunning:
			runningCount++
		default:
			t.Errorf("job %s has unexpected status %q", id, job.Status)
		}
	}
	if queuedCount != 1 || runningCount != 1 {
		t.Errorf("queued=%d running=%d, want 1 and 1", queuedCount, runningCount)
	}
	if n := countJobsAuditRows(t, pool, operator.ActionJobsReleased, "gateway_instance", instanceA.String()); n != 1 {
		t.Errorf("audit rows for instance %s: got %d, want 1", instanceA, n)
	}
}

func TestAdminReleaseJobsHandler_NoMatches(t *testing.T) {
	pool := newTestPostgres(t)
	h := newJobsAdminRouter(pool)
	reqBody, _ := json.Marshal(map[string]string{"instance_id": uuid.New().String()})
	rec := doJobsAdminRequest(t, h, http.MethodPost, "/v1/admin/jobs/release", testJobsOperatorToken, reqBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Released int64 `json:"released"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Released != 0 {
		t.Errorf("released = %d, want 0", body.Released)
	}
}

func TestAdminReleaseJobsHandler_InvalidBody(t *testing.T) {
	pool := newTestPostgres(t)
	h := newJobsAdminRouter(pool)

	cases := []struct {
		name string
		body []byte
	}{
		{"malformed_json", []byte(`{"instance_id":`)},
		{"missing_instance_id", []byte(`{}`)},
		{"invalid_instance_id", []byte(`{"instance_id":"not-a-uuid"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJobsAdminRequest(t, h, http.MethodPost, "/v1/admin/jobs/release", testJobsOperatorToken, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestJobsAdminRoutes_MissingOperatorToken mirrors
// webhookout.TestDeadLetterRoutes_MissingOperatorToken: every /v1/admin/jobs*
// route is gated by the same operator.Middleware used elsewhere under
// /v1/admin — a missing or wrong bearer returns 401 before any handler logic runs.
func TestJobsAdminRoutes_MissingOperatorToken(t *testing.T) {
	pool := newTestPostgres(t)
	h := newJobsAdminRouter(pool)

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"list", http.MethodGet, "/v1/admin/jobs"},
		{"get", http.MethodGet, "/v1/admin/jobs/" + uuid.New().String()},
		{"requeue", http.MethodPost, fmt.Sprintf("/v1/admin/jobs/%s/requeue", uuid.New())},
		{"release", http.MethodPost, "/v1/admin/jobs/release"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/no_token", func(t *testing.T) {
			rec := doJobsAdminRequest(t, h, tc.method, tc.path, "", nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status: got %d, want 401; body=%s", rec.Code, rec.Body.String())
			}
		})
		t.Run(tc.name+"/wrong_token", func(t *testing.T) {
			rec := doJobsAdminRequest(t, h, tc.method, tc.path, "wrong-token", nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status: got %d, want 401; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
