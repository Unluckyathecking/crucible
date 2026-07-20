package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
)

// newDeliveriesTestPostgres returns a pool for the local test database, or
// skips the test when Postgres is unavailable. Failures are fatal when
// TEST_DATABASE_URL or POSTGRES_DSN is set (CI environment).
func newDeliveriesTestPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	explicit := dsn != ""
	if !explicit {
		if v := os.Getenv("POSTGRES_DSN"); v != "" {
			dsn = v
			explicit = true
		} else {
			dsn = "postgres://crucible@localhost:5432/crucible?sslmode=disable"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		if explicit {
			t.Fatalf("TEST_DATABASE_URL set but postgres unavailable: %v", err)
		}
		t.Skipf("postgres unavailable, skipping: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		if explicit {
			t.Fatalf("TEST_DATABASE_URL set but postgres ping failed: %v", err)
		}
		t.Skipf("postgres ping failed, skipping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedDeliveriesCustomer inserts a minimal customers row and registers cleanup.
func seedDeliveriesCustomer(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	email := fmt.Sprintf("deliveries-test-%s@example.com", uuid.New().String()[:8])
	var id uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO customers (email, plan_id) VALUES ($1, 'free') RETURNING id`,
		email,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seedDeliveriesCustomer: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM customers WHERE id = $1`, id)
	})
	return id
}

// seedDeliveriesEndpoint inserts a webhook_endpoints row for the given customer.
func seedDeliveriesEndpoint(t *testing.T, pool *pgxpool.Pool, customerID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO webhook_endpoints (customer_id, url, secret)
		 VALUES ($1, 'https://example.com/hook', decode('cafebabe', 'hex'))
		 RETURNING id`,
		customerID,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seedDeliveriesEndpoint: %v", err)
	}
	return id
}

// seedDelivery inserts one webhook_deliveries row under endpointID and returns
// the row's string ID (matching d.id::text from the handler query).
func seedDelivery(t *testing.T, pool *pgxpool.Pool, endpointID uuid.UUID, eventID string) string {
	t.Helper()
	ctx := context.Background()
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO webhook_deliveries (event_id, endpoint_id, payload)
		 VALUES ($1, $2, '{}')
		 RETURNING id`,
		eventID, endpointID,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seedDelivery: %v", err)
	}
	return fmt.Sprintf("%d", id)
}

// deliveriesAuthContext returns an http.Request with an auth.Key context
// for the given customer ID, simulating the auth middleware upstream.
func deliveriesAuthContext(r *http.Request, customerID uuid.UUID) *http.Request {
	key := &auth.Key{
		ID: uuid.New(),
		Customer: auth.Customer{
			ID:    customerID,
			Email: "test@example.com",
			Plan:  "free",
		},
	}
	return r.WithContext(auth.WithTestKey(r.Context(), key))
}

// deliveriesPage is the shape returned by webhookDeliveriesHandler.
type deliveriesPage struct {
	Items []json.RawMessage `json:"items"`
	Total int64             `json:"total"`
}

// TestWebhookDeliveriesPagination verifies that page/per_page paging returns
// the correct slice and Total, and that the response carries Cache-Control: no-store.
func TestWebhookDeliveriesPagination(t *testing.T) {
	pool := newDeliveriesTestPostgres(t)
	customerID := seedDeliveriesCustomer(t, pool)
	endpointID := seedDeliveriesEndpoint(t, pool, customerID)

	const n = 5
	for i := 0; i < n; i++ {
		seedDelivery(t, pool, endpointID, fmt.Sprintf("evt-%d", i))
	}

	h := webhookDeliveriesHandler(pool)

	t.Run("cache_control_no_store", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/webhooks/deliveries", nil)
		req = deliveriesAuthContext(req, customerID)
		w := httptest.NewRecorder()
		h(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want \"no-store\"", got)
		}
	})

	t.Run("default_page_returns_all_items_and_total", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/webhooks/deliveries", nil)
		req = deliveriesAuthContext(req, customerID)
		w := httptest.NewRecorder()
		h(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		var resp deliveriesPage
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.Total < n {
			t.Errorf("total = %d, want >= %d", resp.Total, n)
		}
	})

	t.Run("per_page_1_returns_one_item_and_correct_total", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/webhooks/deliveries?per_page=1", nil)
		req = deliveriesAuthContext(req, customerID)
		w := httptest.NewRecorder()
		h(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		var resp deliveriesPage
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if len(resp.Items) != 1 {
			t.Errorf("items len = %d, want 1", len(resp.Items))
		}
		if resp.Total < n {
			t.Errorf("total = %d, want >= %d", resp.Total, n)
		}
	})

	t.Run("page_2_per_page_2_returns_second_slice", func(t *testing.T) {
		// Fetch page 1 and page 2 with per_page=2; the sets must be disjoint.
		fetch := func(page int) deliveriesPage {
			t.Helper()
			req := httptest.NewRequest(http.MethodGet,
				fmt.Sprintf("/v1/webhooks/deliveries?page=%d&per_page=2", page), nil)
			req = deliveriesAuthContext(req, customerID)
			w := httptest.NewRecorder()
			h(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("page=%d: status=%d body=%s", page, w.Code, w.Body.String())
			}
			var resp deliveriesPage
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("page=%d decode: %v", page, err)
			}
			return resp
		}

		p1 := fetch(1)
		p2 := fetch(2)

		if len(p1.Items) == 0 {
			t.Error("page 1 returned no items")
		}
		if len(p2.Items) == 0 {
			t.Error("page 2 returned no items")
		}
		// Total must agree across pages.
		if p1.Total != p2.Total {
			t.Errorf("total mismatch across pages: p1=%d p2=%d", p1.Total, p2.Total)
		}
		// Items must be disjoint (different raw JSON — different row IDs).
		p1s := make(map[string]bool, len(p1.Items))
		for _, it := range p1.Items {
			p1s[string(it)] = true
		}
		for _, it := range p2.Items {
			if p1s[string(it)] {
				t.Errorf("page 1 and page 2 share item: %s", it)
			}
		}
	})

	t.Run("oversized_page_returns_400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/webhooks/deliveries?page=9999999999999", nil)
		req = deliveriesAuthContext(req, customerID)
		w := httptest.NewRecorder()
		h(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("response_envelope_has_items_and_total", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/webhooks/deliveries", nil)
		req = deliveriesAuthContext(req, customerID)
		w := httptest.NewRecorder()
		h(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		for _, key := range []string{"items", "total"} {
			if _, ok := top[key]; !ok {
				t.Errorf("response envelope missing key %q", key)
			}
		}
	})
}

// TestWebhookDeliveriesCacheControlNoStoreOnError verifies that error responses
// from webhookDeliveriesHandler also carry Cache-Control: no-store (via apierror.Write).
func TestWebhookDeliveriesCacheControlNoStoreOnError(t *testing.T) {
	// No DB needed: missing auth context triggers the unauthorized path.
	h := webhookDeliveriesHandler(nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/webhooks/deliveries", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want \"no-store\" on error response", got)
	}
}

// TestWebhookDeliveriesPageTooLargeReturnsBadRequest uses a real handler
// with nil DB to confirm the paging guard runs before any DB access.
func TestWebhookDeliveriesPageTooLargeReturnsBadRequest(t *testing.T) {
	h := webhookDeliveriesHandler(nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/webhooks/deliveries?page=9999999999999", nil)
	customerID := uuid.New()
	req = deliveriesAuthContext(req, customerID)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for oversized page", w.Code)
	}
}

// TestWebhookDeliveriesPagingConsistencyWithCrossEndpointSuite ensures the
// deliveries handler follows the same paging.Page[T] envelope contract used
// by every other /v1 list endpoint: {items: [...], total: N}.
func TestWebhookDeliveriesPagingConsistencyWithCrossEndpointSuite(t *testing.T) {
	pool := newDeliveriesTestPostgres(t)
	customerID := seedDeliveriesCustomer(t, pool)

	h := webhookDeliveriesHandler(pool)
	req := httptest.NewRequest(http.MethodGet, "/v1/webhooks/deliveries", nil)
	req = deliveriesAuthContext(req, customerID)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	// Decode as paging.Page[json.RawMessage] to verify the wire shape matches
	// the shared contract every other list endpoint uses.
	var page paging.Page[json.RawMessage]
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("response does not decode as paging.Page: %v", err)
	}
	if page.Items == nil {
		t.Error("paging.Page.Items must be a non-nil slice (never JSON null)")
	}
}
