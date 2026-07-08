package operator_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/license"
	"github.com/Unluckyathecking/crucible/gateway/internal/operator"
)

const testSalt = "test-salt-at-least-32-bytes-long-xxxxx"

// licensed returns a License granting the operator-tokens feature.
func licensed() *license.License {
	return &license.License{Features: []string{license.FeatureOperatorTokens}}
}

// --- TokenStore (Postgres-gated) ---

func TestTokenStore_CreateListRevokeVerify(t *testing.T) {
	pool := newTestPostgres(t)
	ts := operator.NewTokenStore(pool)
	ctx := context.Background()

	tok, full, err := ts.Create(ctx, "ci-runner", testSalt)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM operator_tokens WHERE id = $1`, tok.ID)
	})
	if !strings.HasPrefix(full, "opt_") {
		t.Errorf("token prefix: got %q, want opt_…", full)
	}
	if tok.Name != "ci-runner" {
		t.Errorf("name: got %q", tok.Name)
	}

	// Verify accepts the correct token, rejects wrong ones (constant-time compare
	// over the stored SHA-256 hash — a same-length differing token must fail).
	ok, err := ts.Verify(ctx, testSalt, full)
	if err != nil || !ok {
		t.Fatalf("Verify(correct): ok=%v err=%v", ok, err)
	}
	wrong := "opt_" + strings.Repeat("A", len(full)-4)
	ok, err = ts.Verify(ctx, testSalt, wrong)
	if err != nil || ok {
		t.Fatalf("Verify(wrong, same len): ok=%v err=%v, want reject", ok, err)
	}
	ok, _ = ts.Verify(ctx, "different-salt-different-salt-xxxx", full)
	if ok {
		t.Error("Verify with wrong salt should reject")
	}

	// List returns the token metadata, never the hash.
	page, err := ts.List(ctx, operator.TokensFilter{Page: 1, PerPage: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, it := range page.Items {
		if it.ID == tok.ID {
			found = true
		}
	}
	if !found {
		t.Error("List did not include the created token")
	}
	b, _ := json.Marshal(page)
	if strings.Contains(string(b), "hash") {
		t.Errorf("List JSON leaked a hash field: %s", b)
	}

	// Revoke takes effect immediately: Verify must reject afterward.
	revoked, err := ts.Revoke(ctx, tok.ID)
	if err != nil || !revoked {
		t.Fatalf("Revoke: found=%v err=%v", revoked, err)
	}
	ok, _ = ts.Verify(ctx, testSalt, full)
	if ok {
		t.Error("Verify accepted a revoked token")
	}

	// Revoking an unknown id reports not-found.
	found2, err := ts.Revoke(ctx, uuid.New())
	if err != nil {
		t.Fatalf("Revoke(unknown): %v", err)
	}
	if found2 {
		t.Error("Revoke(unknown) reported found")
	}
}

// --- TokenMiddleware ---

func middlewareRouter(mw func(http.Handler) http.Handler) http.Handler {
	r := chi.NewRouter()
	r.Use(mw)
	r.Get("/v1/admin/plans", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return r
}

func do(h http.Handler, bearer string) int {
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/plans", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// TestTokenMiddleware_StaticUnlicensed: the static OPERATOR_TOKEN works even
// with a nil (community) license and no token store — the community path must
// never regress.
func TestTokenMiddleware_StaticUnlicensed(t *testing.T) {
	h := middlewareRouter(operator.TokenMiddleware("static-secret", nil, testSalt, nil))
	if code := do(h, "static-secret"); code != http.StatusOK {
		t.Errorf("static token unlicensed: got %d, want 200", code)
	}
	if code := do(h, "wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", code)
	}
}

func TestTokenMiddleware_DBToken_UnlicensedRejected_LicensedAccepted(t *testing.T) {
	pool := newTestPostgres(t)
	ts := operator.NewTokenStore(pool)
	tok, full, err := ts.Create(context.Background(), "mw-token", testSalt)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM operator_tokens WHERE id = $1`, tok.ID) })

	// Unlicensed (nil license): DB token must be rejected — the DB is never consulted.
	unlic := middlewareRouter(operator.TokenMiddleware("static-secret", ts, testSalt, nil))
	if code := do(unlic, full); code != http.StatusUnauthorized {
		t.Errorf("DB token unlicensed: got %d, want 401", code)
	}

	// Licensed: DB token accepted.
	lic := middlewareRouter(operator.TokenMiddleware("static-secret", ts, testSalt, licensed()))
	if code := do(lic, full); code != http.StatusOK {
		t.Errorf("DB token licensed: got %d, want 200", code)
	}
	// Static token still works on the licensed middleware too.
	if code := do(lic, "static-secret"); code != http.StatusOK {
		t.Errorf("static token on licensed mw: got %d, want 200", code)
	}
}

func TestTokenMiddleware_RevokedDBTokenRejected(t *testing.T) {
	pool := newTestPostgres(t)
	ts := operator.NewTokenStore(pool)
	tok, full, err := ts.Create(context.Background(), "mw-revoke", testSalt)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM operator_tokens WHERE id = $1`, tok.ID) })

	lic := middlewareRouter(operator.TokenMiddleware("static-secret", ts, testSalt, licensed()))
	if code := do(lic, full); code != http.StatusOK {
		t.Fatalf("pre-revoke: got %d, want 200", code)
	}
	if _, err := ts.Revoke(context.Background(), tok.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if code := do(lic, full); code != http.StatusUnauthorized {
		t.Errorf("revoked token: got %d, want 401", code)
	}
}

// --- Handlers: license gate (no DB needed) ---

func TestTokenHandlers_Unlicensed403(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		path    string
		pattern string
		h       http.HandlerFunc
	}{
		{"create", http.MethodPost, "/v1/admin/tokens", "/v1/admin/tokens", operator.CreateTokenHandler(nil, nil, testSalt, nil)},
		{"list", http.MethodGet, "/v1/admin/tokens", "/v1/admin/tokens", operator.ListTokensHandler(nil, nil)},
		{"revoke", http.MethodDelete, "/v1/admin/tokens/" + uuid.New().String(), "/v1/admin/tokens/{id}", operator.RevokeTokenHandler(nil, nil, nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			r.Method(tc.method, tc.pattern, tc.h)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"name":"x"}`))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("got %d, want 403 — body: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.NewDecoder(rec.Body).Decode(&body)
			if body.Error.Code != "FEATURE_NOT_LICENSED" {
				t.Errorf("code: got %q, want FEATURE_NOT_LICENSED", body.Error.Code)
			}
		})
	}
}

// --- Handlers: CRUD happy path + audit (Postgres-gated) ---

func TestTokenHandlers_CRUD_Audit(t *testing.T) {
	pool := newTestPostgres(t)
	ts := operator.NewTokenStore(pool)

	r := chi.NewRouter()
	r.Post("/v1/admin/tokens", operator.CreateTokenHandler(ts, pool, testSalt, licensed()))
	r.Get("/v1/admin/tokens", operator.ListTokensHandler(ts, licensed()))
	r.Delete("/v1/admin/tokens/{id}", operator.RevokeTokenHandler(ts, pool, licensed()))

	// Create.
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/tokens", strings.NewReader(`{"name":"deploy-bot"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201 — body: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	id := uuid.MustParse(created.ID)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM operator_tokens WHERE id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM audit_log WHERE target_type = 'operator_token' AND target_id = $1`, created.ID)
	})
	if created.Token == "" || !strings.HasPrefix(created.Token, "opt_") {
		t.Errorf("token not returned once: %q", created.Token)
	}

	// audit: operator_token.created row exists.
	if !auditExists(t, pool, "operator_token.created", created.ID) {
		t.Error("no operator_token.created audit row")
	}

	// List does not echo token material.
	lrec := httptest.NewRecorder()
	r.ServeHTTP(lrec, httptest.NewRequest(http.MethodGet, "/v1/admin/tokens", nil))
	if lrec.Code != http.StatusOK {
		t.Fatalf("list: got %d", lrec.Code)
	}
	if strings.Contains(lrec.Body.String(), created.Token) {
		t.Error("list leaked token material")
	}

	// Revoke.
	drec := httptest.NewRecorder()
	r.ServeHTTP(drec, httptest.NewRequest(http.MethodDelete, "/v1/admin/tokens/"+created.ID, nil))
	if drec.Code != http.StatusNoContent {
		t.Fatalf("revoke: got %d, want 204 — body: %s", drec.Code, drec.Body.String())
	}
	if !auditExists(t, pool, "operator_token.revoked", created.ID) {
		t.Error("no operator_token.revoked audit row")
	}

	// Revoking an unknown id → 404.
	nrec := httptest.NewRecorder()
	r.ServeHTTP(nrec, httptest.NewRequest(http.MethodDelete, "/v1/admin/tokens/"+uuid.New().String(), nil))
	if nrec.Code != http.StatusNotFound {
		t.Errorf("revoke unknown: got %d, want 404", nrec.Code)
	}
}

func auditExists(t *testing.T, pool *pgxpool.Pool, action, targetID string) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(context.Background(), `
		SELECT EXISTS(
			SELECT 1 FROM audit_log
			WHERE action = $1 AND target_type = 'operator_token' AND target_id = $2
		)
	`, action, targetID).Scan(&exists)
	if err != nil {
		t.Fatalf("auditExists query: %v", err)
	}
	return exists
}
