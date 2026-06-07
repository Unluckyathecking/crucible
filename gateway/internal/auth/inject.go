package auth

import "context"

// WithKey returns a derived context carrying key, mirroring the placement
// auth.Middleware performs after verifying a Bearer token.
// Use in service-to-service paths and tests that need to seed a trusted key
// without a full DB/Redis lookup. The auth middleware is always the right choice
// on the external request path.
func WithKey(ctx context.Context, key *Key) context.Context {
	return context.WithValue(ctx, keyCtxKey, key)
}
