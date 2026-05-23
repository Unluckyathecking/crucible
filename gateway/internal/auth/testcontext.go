package auth

import "context"

// WithTestKey injects a Key into context so downstream middleware (quota, ratelimit)
// can retrieve it via FromContext. Only used by tests; never compiled into production.
func WithTestKey(ctx context.Context, k *Key) context.Context {
	return context.WithValue(ctx, keyCtxKey, k)
}
