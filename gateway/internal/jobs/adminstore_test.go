package jobs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestStore_AdminGet_CrossCustomer(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-admin-get-"+uuid.New().String()+"@example.com")

	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{"in":1}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// AdminGet must find the row with no customer_id supplied at all —
	// the cross-customer counterpart to Get's IDOR-safe, customer-scoped lookup.
	job, ok, err := s.AdminGet(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("AdminGet: ok=%v err=%v", ok, err)
	}
	if job.ID != id || job.CustomerID != custA || job.Operation != "echo" {
		t.Errorf("AdminGet returned unexpected job: %+v", job)
	}
	if job.ClaimedBy != nil || job.ClaimedAt != nil {
		t.Errorf("AdminGet on a queued job: claimed_by=%v claimed_at=%v, want both nil", job.ClaimedBy, job.ClaimedAt)
	}
}

func TestStore_AdminGet_ClaimedFieldsPopulated(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-admin-get-claimed-"+uuid.New().String()+"@example.com")

	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	instanceID := uuid.New()
	if _, err := s.Claim(context.Background(), 10, instanceID, 0); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	job, ok, err := s.AdminGet(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("AdminGet: ok=%v err=%v", ok, err)
	}
	if job.Status != StatusRunning {
		t.Fatalf("status = %q, want %q", job.Status, StatusRunning)
	}
	if job.ClaimedBy == nil || *job.ClaimedBy != instanceID {
		t.Errorf("claimed_by = %v, want %s", job.ClaimedBy, instanceID)
	}
	if job.ClaimedAt == nil {
		t.Error("claimed_at = nil, want non-nil for a running job")
	}
}

func TestStore_AdminGet_NotFound(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)

	_, ok, err := s.AdminGet(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("AdminGet(unknown id): err = %v, want nil", err)
	}
	if ok {
		t.Error("AdminGet(unknown id): ok = true, want false")
	}
}

func TestStore_AdminGet_NilReceiver(t *testing.T) {
	var s *Store
	job, ok, err := s.AdminGet(context.Background(), uuid.New())
	if job.ID != uuid.Nil || ok || err != nil {
		t.Errorf("nil Store.AdminGet: got (%+v, %v, %v), want (zero, false, nil)", job, ok, err)
	}
}

func TestStore_AdminList_CrossCustomer(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-admin-list-a-"+uuid.New().String()+"@example.com")
	custB, keyB := seedCustomer(t, pool, "jobs-admin-list-b-"+uuid.New().String()+"@example.com")

	idA, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-a", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue (custA): %v", err)
	}
	idB, err := s.Enqueue(context.Background(), custB, keyB, "echo", "req-b", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue (custB): %v", err)
	}

	// Unlike List (customer-scoped), AdminList must surface rows from both
	// customers in one call — that's the entire point of the cross-customer view.
	adminJobs, total, err := s.AdminList(context.Background(), nil, 50, 0)
	if err != nil {
		t.Fatalf("AdminList: %v", err)
	}
	if total < 2 {
		t.Fatalf("total = %d, want >= 2", total)
	}
	seen := map[uuid.UUID]bool{}
	for _, j := range adminJobs {
		seen[j.ID] = true
	}
	if !seen[idA] || !seen[idB] {
		t.Errorf("AdminList missing jobs from one or both customers: seen=%v want %s and %s", seen, idA, idB)
	}
}

func TestStore_AdminList_StatusFilter(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-admin-list-status-"+uuid.New().String()+"@example.com")

	queuedID, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-queued", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue (queued): %v", err)
	}
	failedID, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-failed", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue (failed): %v", err)
	}
	if err := s.Fail(context.Background(), failedID, "WORKER_BAD_RESPONSE", "worker contract violation"); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	failedStatus := StatusFailed
	adminJobs, total, err := s.AdminList(context.Background(), &failedStatus, 50, 0)
	if err != nil {
		t.Fatalf("AdminList(status=failed): %v", err)
	}
	for _, j := range adminJobs {
		if j.ID == queuedID {
			t.Error("AdminList(status=failed) returned the still-queued job")
		}
	}
	found := false
	for _, j := range adminJobs {
		if j.ID == failedID {
			found = true
		}
	}
	if !found {
		t.Error("AdminList(status=failed) did not return the failed job")
	}
	if total < 1 {
		t.Errorf("total = %d, want >= 1", total)
	}
}

func TestStore_AdminList_Pagination(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-admin-list-page-"+uuid.New().String()+"@example.com")

	const numJobs = 5
	for i := 0; i < numJobs; i++ {
		if _, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, ""); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	page1, _, err := s.AdminList(context.Background(), nil, 2, 0)
	if err != nil {
		t.Fatalf("AdminList(page1): %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("len(page1) = %d, want 2", len(page1))
	}

	page2, _, err := s.AdminList(context.Background(), nil, 2, 2)
	if err != nil {
		t.Fatalf("AdminList(page2): %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("len(page2) = %d, want 2", len(page2))
	}
	if page1[0].ID == page2[0].ID || page1[1].ID == page2[0].ID {
		t.Error("AdminList pages overlap: same job returned on page1 and page2")
	}
}

func TestStore_AdminList_NilReceiver(t *testing.T) {
	var s *Store
	adminJobs, total, err := s.AdminList(context.Background(), nil, 10, 0)
	if adminJobs != nil || total != 0 || err != nil {
		t.Errorf("nil Store.AdminList: got (%v, %d, %v), want (nil, 0, nil)", adminJobs, total, err)
	}
}
