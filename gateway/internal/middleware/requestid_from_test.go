package middleware

import (
	"context"
	"testing"
)

func TestRequestIDFrom(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"present", context.WithValue(context.Background(), RequestIDKey, "abc"), "abc"},
		{"absent", context.Background(), ""},
		{"wrong type", context.WithValue(context.Background(), RequestIDKey, 42), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RequestIDFrom(tc.ctx); got != tc.want {
				t.Fatalf("RequestIDFrom() = %q, want %q", got, tc.want)
			}
		})
	}
}
