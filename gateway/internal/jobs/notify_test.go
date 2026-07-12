package jobs

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestNotify_NilEmitter_NoPanic proves notifySucceeded/notifyFailed are safe
// to call with a nil *webhookout.Emitter (the default when Deps.DB is unset) —
// mirroring webhookout.Emitter.Emit's own nil-receiver safety, with no
// separate nil check required at these call sites.
func TestNotify_NilEmitter_NoPanic(t *testing.T) {
	job := Job{ID: uuid.New(), CustomerID: uuid.New(), Operation: "echo"}

	notifySucceeded(context.Background(), nil, job)
	notifyFailed(context.Background(), nil, job, "WORKER_UNREACHABLE")
}
