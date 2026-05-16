package quota

import (
	"context"
	"sync/atomic"
)

// The recordSignal is the bridge between the quota middleware (which reserves a slot
// up-front) and the usage recorder (which knows whether the request actually produced
// billable usage). The middleware seeds an empty signal into context; the recorder
// flips it true on a successful Record(); the middleware checks it after the handler
// returns and refunds the reserve when no usage was recorded.
//
// This handles the case Codex flagged: a worker can return HTTP 200 with a structured
// error envelope (resp.Error != nil), the route writes a success response, but the
// recorder declines to insert a usage row. Status-only refunding would miss this.

type recordedKey struct{}

type recordSignal struct {
	recorded atomic.Bool
}

// withRecordSignal returns a derived context carrying a fresh signal and the signal itself.
// Called by the middleware before invoking the inner handler.
func withRecordSignal(ctx context.Context) (context.Context, *recordSignal) {
	s := &recordSignal{}
	return context.WithValue(ctx, recordedKey{}, s), s
}

// MarkRecorded flips the in-context signal to "usage was written" — called by the recorder
// after a successful DB insert. No-op if the quota middleware isn't in the chain (the
// signal isn't seeded), which makes the recorder safe to call from contexts where quota
// isn't enforced.
func MarkRecorded(ctx context.Context) {
	if s, ok := ctx.Value(recordedKey{}).(*recordSignal); ok {
		s.recorded.Store(true)
	}
}
