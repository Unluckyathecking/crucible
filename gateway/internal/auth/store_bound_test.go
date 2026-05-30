package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestEnqueueUpdate_Bounded proves the last_used_at background work is bounded:
// during a cold-cache storm (e.g. Redis outage, every request is a cold path) the
// per-lookup enqueue is non-blocking and sheds excess work once the buffer is full,
// instead of spawning an unbounded goroutine per request. Pre-fix, Lookup spawned a
// detached `go func()` per cold lookup, so a burst of N lookups produced up to N
// concurrent DB writes with no upper bound — this test would not hold there.
func TestEnqueueUpdate_Bounded(t *testing.T) {
	const buf = 8
	// Construct the Store directly (white-box) so no real worker drains the buffer;
	// this lets us observe the cap deterministically without Postgres/Redis.
	s := &Store{updates: make(chan uuid.UUID, buf)}

	const burst = 1000
	queued := 0
	for i := 0; i < burst; i++ {
		if s.enqueueUpdate(uuid.New()) {
			queued++
		}
	}

	// At most `buf` updates are ever in flight; everything else is dropped. The number
	// of pending writes is capped by the buffer, NOT by the inbound request rate.
	if queued != buf {
		t.Fatalf("queued=%d under burst of %d, want exactly buffer size %d (excess must be dropped)", queued, burst, buf)
	}
	if len(s.updates) != buf {
		t.Fatalf("channel depth=%d, want %d — bound must equal buffer capacity", len(s.updates), buf)
	}
}

// TestEnqueueUpdate_NeverBlocks proves the hot path never blocks even when every
// caller in a concurrent burst tries to enqueue against a full, undrained buffer.
// A blocking (unbounded-backpressure) implementation would deadlock this test.
func TestEnqueueUpdate_NeverBlocks(t *testing.T) {
	s := &Store{updates: make(chan uuid.UUID, 4)}

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i := 0; i < 200; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.enqueueUpdate(uuid.New()) // must return promptly whether queued or dropped
			}()
		}
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueueUpdate blocked under concurrent burst against a full buffer; hot path must never block")
	}
}

// TestClose_CancelsRootContext proves background writes derive from a long-lived store
// context that Close cancels — not a detached context.Background(). On shutdown this
// aborts any in-flight last_used_at write instead of leaking a write bound only by its
// own 2s timeout.
func TestClose_CancelsRootContext(t *testing.T) {
	rootCtx, cancel := context.WithCancel(context.Background())
	s := &Store{
		updates: make(chan uuid.UUID, 1),
		rootCtx: rootCtx,
		cancel:  cancel,
	}
	s.wg.Add(1)
	go s.processUpdates()

	if err := s.rootCtx.Err(); err != nil {
		t.Fatalf("root context cancelled before Close: %v", err)
	}

	s.Close()

	if err := s.rootCtx.Err(); err == nil {
		t.Fatal("root context not cancelled after Close; background writes would outlive the Store")
	}
}
