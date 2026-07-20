package auth

import "context"

// WithTestKey injects a Key into context so downstream middleware (quota,
// ratelimit) can retrieve it via FromContext, mirroring the placement
// auth.Middleware performs after verifying a Bearer token. Used only by tests
// to seed a trusted key without a full DB/Redis lookup — the auth middleware is
// always the right choice on the external request path. This file has no _test
// suffix, so it does compile into the production binary; it is exported for
// cross-package test use (quota, ratelimit, server, and others test against it).
func WithTestKey(ctx context.Context, k *Key) context.Context {
	return context.WithValue(ctx, keyCtxKey, k)
}
