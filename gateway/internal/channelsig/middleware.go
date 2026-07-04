package channelsig

import (
	"bytes"
	"io"
	"net/http"
	"time"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
)

// Middleware returns net/http middleware that verifies an inbound "t=,v1=" signature
// carried in headerName against the request body before calling next, using secret and
// window (see Verify). An empty secret disables verification entirely — every request
// passes through unmodified — the same opt-in, zero-config-safe default the outbound
// signers (webhookout.Sign, proxy.Client.WithSecret) already use. This exists for future
// signed inbound channels; neither webhookout nor proxy currently mount it, since the
// gateway is the caller (not the receiver) on both of today's signed channels.
func Middleware(secret []byte, headerName string, window time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if len(secret) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				apierror.Write(w, "", http.StatusBadRequest, apierror.BAD_REQUEST, "invalid request body", false)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			if err := Verify(secret, r.Header.Get(headerName), body, time.Now(), window); err != nil {
				apierror.Write(w, "", http.StatusUnauthorized, apierror.UNAUTHORIZED, "invalid request signature", false)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
