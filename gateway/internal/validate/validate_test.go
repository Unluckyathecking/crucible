package validate_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	mwpkg "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
	"github.com/Unluckyathecking/crucible/gateway/internal/validate"
)

// errReader is an io.ReadCloser that always returns an error from Read.
type errReader struct{ err error }

func (e *errReader) Read(_ []byte) (int, error) { return 0, e.err }
func (e *errReader) Close() error               { return nil }

// intPtr / float64Ptr are helpers for pointer literals in schema definitions.
func intPtr(v int) *int         { return &v }
func f64Ptr(v float64) *float64 { return &v }

// --- Validate() table-driven unit tests ---

func TestValidate(t *testing.T) {
	tests := []struct {
		name        string
		schema      *openapi.Schema
		input       string // raw JSON to unmarshal into any
		wantErr     bool
		errContains string // substring the error must contain
	}{
		// nil / empty schema — pass-through
		{
			name:   "nil schema is pass-through",
			schema: nil,
			input:  `{"any":"value"}`,
		},
		{
			name:   "empty schema is pass-through",
			schema: &openapi.Schema{},
			input:  `{"any":"value"}`,
		},
		// valid body
		{
			name: "valid body passes",
			schema: &openapi.Schema{
				Type:     "object",
				Required: []string{"name"},
				Properties: map[string]*openapi.Schema{
					"name": {Type: "string"},
					"age":  {Type: "integer"},
				},
			},
			input: `{"name":"Alice","age":30}`,
		},
		// required field
		{
			name: "missing required field returns error naming field",
			schema: &openapi.Schema{
				Type:     "object",
				Required: []string{"name"},
				Properties: map[string]*openapi.Schema{
					"name": {Type: "string"},
				},
			},
			input:       `{}`,
			wantErr:     true,
			errContains: "name",
		},
		{
			name: "present required field passes",
			schema: &openapi.Schema{
				Type:     "object",
				Required: []string{"id"},
				Properties: map[string]*openapi.Schema{
					"id": {Type: "string"},
				},
			},
			input: `{"id":"abc"}`,
		},
		// wrong type
		{
			name: "wrong type returns error naming field",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"age": {Type: "integer"},
				},
			},
			input:       `{"age":"not-a-number"}`,
			wantErr:     true,
			errContains: "age",
		},
		{
			name: "string where number expected",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"score": {Type: "number"},
				},
			},
			input:       `{"score":"high"}`,
			wantErr:     true,
			errContains: "score",
		},
		{
			name: "number where boolean expected",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"active": {Type: "boolean"},
				},
			},
			input:       `{"active":1}`,
			wantErr:     true,
			errContains: "active",
		},
		{
			name: "float rejected for integer type",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"count": {Type: "integer"},
				},
			},
			input:       `{"count":1.5}`,
			wantErr:     true,
			errContains: "count",
		},
		{
			name: "whole-number float accepted for integer type",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"count": {Type: "integer"},
				},
			},
			input: `{"count":42}`,
		},
		// additionalProperties: false
		{
			name: "unexpected field under additionalProperties false",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"name": {Type: "string"},
				},
				AdditionalProperties: &openapi.Schema{BoolFalse: true},
			},
			input:       `{"name":"Alice","extra":"field"}`,
			wantErr:     true,
			errContains: "extra",
		},
		{
			name: "no additional fields with additionalProperties false passes",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"name": {Type: "string"},
				},
				AdditionalProperties: &openapi.Schema{BoolFalse: true},
			},
			input: `{"name":"Bob"}`,
		},
		{
			name: "additional fields allowed when no additionalProperties constraint",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"name": {Type: "string"},
				},
			},
			input: `{"name":"Carol","extra":"ok"}`,
		},
		// enum
		{
			name: "enum violation returns error naming field",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"color": {Type: "string", Enum: []any{"red", "green", "blue"}},
				},
			},
			input:       `{"color":"purple"}`,
			wantErr:     true,
			errContains: "color",
		},
		{
			name: "enum value passes",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"color": {Type: "string", Enum: []any{"red", "green", "blue"}},
				},
			},
			input: `{"color":"green"}`,
		},
		{
			name: "enum with integer",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"level": {Type: "integer", Enum: []any{float64(1), float64(2), float64(3)}},
				},
			},
			input: `{"level":2}`,
		},
		{
			name: "enum integer violation",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"level": {Type: "integer", Enum: []any{float64(1), float64(2), float64(3)}},
				},
			},
			input:       `{"level":9}`,
			wantErr:     true,
			errContains: "level",
		},
		// string constraints
		{
			name: "minLength violation",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"code": {Type: "string", MinLength: intPtr(3)},
				},
			},
			input:       `{"code":"ab"}`,
			wantErr:     true,
			errContains: "code",
		},
		{
			name: "minLength satisfied",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"code": {Type: "string", MinLength: intPtr(3)},
				},
			},
			input: `{"code":"abc"}`,
		},
		{
			name: "maxLength violation",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"code": {Type: "string", MaxLength: intPtr(5)},
				},
			},
			input:       `{"code":"toolong"}`,
			wantErr:     true,
			errContains: "code",
		},
		{
			name: "maxLength satisfied",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"code": {Type: "string", MaxLength: intPtr(5)},
				},
			},
			input: `{"code":"hi"}`,
		},
		// pattern constraint
		{
			name: "pattern match passes",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"code": {Type: "string", Pattern: `^\d{4}$`},
				},
			},
			input: `{"code":"1234"}`,
		},
		{
			name: "pattern violation",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"code": {Type: "string", Pattern: `^\d{4}$`},
				},
			},
			input:       `{"code":"abc"}`,
			wantErr:     true,
			errContains: "code",
		},
		// null body
		{
			name: "null body fails object type check",
			schema: &openapi.Schema{
				Type: "object",
			},
			input:   `null`,
			wantErr: true,
		},
		// number constraints
		{
			name: "minimum violation",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"amount": {Type: "number", Minimum: f64Ptr(0)},
				},
			},
			input:       `{"amount":-1}`,
			wantErr:     true,
			errContains: "amount",
		},
		{
			name: "minimum satisfied at boundary",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"amount": {Type: "number", Minimum: f64Ptr(0)},
				},
			},
			input: `{"amount":0}`,
		},
		{
			name: "maximum violation",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"amount": {Type: "number", Maximum: f64Ptr(100)},
				},
			},
			input:       `{"amount":101}`,
			wantErr:     true,
			errContains: "amount",
		},
		{
			name: "maximum satisfied at boundary",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"amount": {Type: "number", Maximum: f64Ptr(100)},
				},
			},
			input: `{"amount":100}`,
		},
		// Unicode string length — minLength/maxLength count code points, not bytes.
		{
			name: "minLength counts runes not bytes",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					// "é" is 2 bytes but 1 rune; minLength:1 must pass.
					"s": {Type: "string", MinLength: intPtr(1)},
				},
			},
			input: `{"s":"é"}`,
		},
		{
			name: "maxLength counts runes not bytes - multibyte char fits",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					// "é" is 2 bytes but 1 rune; maxLength:1 must pass.
					"s": {Type: "string", MaxLength: intPtr(1)},
				},
			},
			input: `{"s":"é"}`,
		},
		// array values pass through without error
		{
			name: "array value with no constraints passes",
			schema: &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"tags": {Type: "array"},
				},
			},
			input: `{"tags":["a","b"]}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var data any
			if err := json.Unmarshal([]byte(tc.input), &data); err != nil {
				t.Fatalf("test setup: unmarshal %q: %v", tc.input, err)
			}
			err := validate.Validate(tc.schema, data)
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
			if tc.wantErr && tc.errContains != "" && err != nil {
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errContains)
				}
			}
		})
	}
}

// --- ValidateBytes edge cases ---

func TestValidateBytes(t *testing.T) {
	schema := &openapi.Schema{
		Type:     "object",
		Required: []string{"x"},
		Properties: map[string]*openapi.Schema{
			"x": {Type: "string"},
		},
	}

	t.Run("nil schema passes empty body", func(t *testing.T) {
		if err := validate.ValidateBytes(nil, []byte{}); err != nil {
			t.Errorf("nil schema: unexpected error: %v", err)
		}
	})

	t.Run("empty body returns error", func(t *testing.T) {
		err := validate.ValidateBytes(schema, []byte{})
		if err == nil {
			t.Fatal("expected error for empty body, got nil")
		}
	})

	t.Run("null JSON body fails object type check", func(t *testing.T) {
		err := validate.ValidateBytes(schema, []byte("null"))
		if err == nil {
			t.Fatal("expected error for null body against object schema, got nil")
		}
	})

	t.Run("valid body passes", func(t *testing.T) {
		err := validate.ValidateBytes(schema, []byte(`{"x":"hello"}`))
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("invalid JSON returns error", func(t *testing.T) {
		err := validate.ValidateBytes(schema, []byte(`not json`))
		if err == nil {
			t.Fatal("expected error for invalid JSON, got nil")
		}
	})

	t.Run("trailing valid JSON rejected with specific message", func(t *testing.T) {
		err := validate.ValidateBytes(schema, []byte(`{"x":"hello"}{}`))
		if err == nil {
			t.Fatal("expected error for trailing JSON tokens, got nil")
		}
		if !strings.Contains(err.Error(), "trailing") {
			t.Errorf("error should mention trailing data, got: %v", err)
		}
	})

	t.Run("trailing garbage rejected with trailing message", func(t *testing.T) {
		err := validate.ValidateBytes(schema, []byte(`{"x":"hello"}garbage`))
		if err == nil {
			t.Fatal("expected error for trailing garbage, got nil")
		}
		// Trailing garbage after a valid JSON value is still "trailing data",
		// not "invalid JSON body" — the primary value was already decoded.
		if !strings.Contains(err.Error(), "trailing") {
			t.Errorf("trailing garbage error should say 'trailing data', got: %v", err)
		}
	})
}

// TestValidateBytesIntegerNotations verifies that ValidateBytes accepts JSON
// integer notations with zero fractional parts (1.0, 1e3) per JSON Schema spec,
// while still rejecting genuine fractions (1.5).
func TestValidateBytesIntegerNotations(t *testing.T) {
	schema := &openapi.Schema{
		Type: "object",
		Properties: map[string]*openapi.Schema{
			"id": {Type: "integer"},
		},
	}
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"plain integer", `{"id":42}`, false},
		{"1.0 notation accepted", `{"id":1.0}`, false},
		{"1e3 notation accepted", `{"id":1e3}`, false},
		{"fractional 1.5 rejected", `{"id":1.5}`, true},
		{"fractional 0.5 rejected", `{"id":0.5}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validate.ValidateBytes(schema, []byte(tc.body))
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// --- Middleware integration tests ---

// makeTestRouter builds a chi router with validate.Middleware on the /v1 sub-route,
// using the supplied schema (nil = no RequestSchema on the route).
func makeTestRouter(schema *openapi.Schema, inner http.HandlerFunc) http.Handler {
	routes := []openapi.RouteDescriptor{
		{Path: "/test", Operation: "test", Summary: "Test", RequestSchema: schema},
	}
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(validate.Middleware(routes))
		r.Post("/test", inner)
	})
	return r
}

func TestMiddlewareBodyByteIdenticalAfterValidation(t *testing.T) {
	const body = `{"name":"Alice","age":30}`
	schema := &openapi.Schema{
		Type:     "object",
		Required: []string{"name"},
		Properties: map[string]*openapi.Schema{
			"name": {Type: "string"},
			"age":  {Type: "integer"},
		},
	}

	var got []byte
	router := makeTestRouter(schema, func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("inner handler read body: %v", err)
		}
		got = b
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if string(got) != body {
		t.Errorf("body not byte-identical: got %q, want %q", got, body)
	}
}

func TestMiddlewareNilSchemaPassThrough(t *testing.T) {
	// No RequestSchema on the route — middleware must pass all bodies through.
	reached := false
	router := makeTestRouter(nil, func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{"anything":"goes"}`))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if !reached {
		t.Error("inner handler was not reached for schema-less route")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestMiddlewareInvalidBodyReturns400(t *testing.T) {
	schema := &openapi.Schema{
		Type:     "object",
		Required: []string{"name"},
		Properties: map[string]*openapi.Schema{
			"name": {Type: "string"},
		},
	}

	workerCalled := false
	router := makeTestRouter(schema, func(w http.ResponseWriter, _ *http.Request) {
		workerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	// Missing required field "name".
	req := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if workerCalled {
		t.Error("inner handler must NOT be called when validation fails")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}

	// Response must be the apierror envelope.
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 400 body: %v", err)
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing top-level error object")
	}
	if errObj["code"] != "BAD_REQUEST" {
		t.Errorf("error.code = %q, want BAD_REQUEST", errObj["code"])
	}
	if errObj["retryable"] != false {
		t.Errorf("error.retryable = %v, want false", errObj["retryable"])
	}
	// Error message must name the failing field.
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "name") {
		t.Errorf("error.message %q does not name the failing field", msg)
	}
}

func TestMiddlewareErrorHasRequestID(t *testing.T) {
	const rid = "test-validate-rid-001"
	schema := &openapi.Schema{
		Type:     "object",
		Required: []string{"x"},
		Properties: map[string]*openapi.Schema{
			"x": {Type: "string"},
		},
	}

	routes := []openapi.RouteDescriptor{
		{Path: "/test", Operation: "test", Summary: "Test", RequestSchema: schema},
	}
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(validate.Middleware(routes))
		r.Post("/test", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{}`))
	req = req.WithContext(context.WithValue(req.Context(), mwpkg.RequestIDKey, rid))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj["request_id"] != rid {
		t.Errorf("error.request_id = %q, want %q", errObj["request_id"], rid)
	}
}

func TestMiddlewareAdditionalPropertiesFalse(t *testing.T) {
	schema := &openapi.Schema{
		Type: "object",
		Properties: map[string]*openapi.Schema{
			"name": {Type: "string"},
		},
		AdditionalProperties: &openapi.Schema{BoolFalse: true},
	}

	workerCalled := false
	router := makeTestRouter(schema, func(w http.ResponseWriter, _ *http.Request) {
		workerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{"name":"ok","extra":"bad"}`))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if workerCalled {
		t.Error("inner handler must NOT be called when additionalProperties:false is violated")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	errObj, _ := resp["error"].(map[string]any)
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "extra") {
		t.Errorf("error message %q does not name field 'extra'", msg)
	}
}

func TestMiddlewareMultipleRoutesSchemasIsolated(t *testing.T) {
	// Two routes: one with a strict schema, one with no schema.
	// Requests to the unconstrained route must pass even with "extra" fields.
	strictSchema := &openapi.Schema{
		Type: "object",
		Properties: map[string]*openapi.Schema{
			"x": {Type: "string"},
		},
		AdditionalProperties: &openapi.Schema{BoolFalse: true},
	}
	routes := []openapi.RouteDescriptor{
		{Path: "/strict", Operation: "strict", Summary: "Strict", RequestSchema: strictSchema},
		{Path: "/loose", Operation: "loose", Summary: "Loose"}, // no schema
	}

	var strictReached, looseReached bool
	mux := chi.NewRouter()
	mux.Route("/v1", func(r chi.Router) {
		r.Use(validate.Middleware(routes))
		r.Post("/strict", func(w http.ResponseWriter, _ *http.Request) {
			strictReached = true
			w.WriteHeader(http.StatusOK)
		})
		r.Post("/loose", func(w http.ResponseWriter, _ *http.Request) {
			looseReached = true
			w.WriteHeader(http.StatusOK)
		})
	})

	// Strict route with valid body passes.
	req := httptest.NewRequest(http.MethodPost, "/v1/strict", strings.NewReader(`{"x":"hi"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("strict/valid: expected 200, got %d", w.Code)
	}
	if !strictReached {
		t.Error("strict handler not reached on valid body")
	}

	// Strict route with extra field blocked.
	req = httptest.NewRequest(http.MethodPost, "/v1/strict", strings.NewReader(`{"x":"hi","y":"nope"}`))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("strict/extra: expected 400, got %d", w.Code)
	}

	// Loose route with arbitrary body passes.
	req = httptest.NewRequest(http.MethodPost, "/v1/loose", strings.NewReader(`{"anything":"goes"}`))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("loose: expected 200, got %d", w.Code)
	}
	if !looseReached {
		t.Error("loose handler not reached")
	}
}

// TestMiddlewareFastPathNoSchemas verifies that the middleware fast-path
// (all routes have no RequestSchema → schemas map is empty) passes through
// every request without touching the body.
func TestMiddlewareFastPathNoSchemas(t *testing.T) {
	reached := false
	routes := []openapi.RouteDescriptor{
		{Path: "/test", Operation: "test", Summary: "No schema"},
	}
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(validate.Middleware(routes))
		r.Post("/test", func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if !reached {
		t.Error("handler not reached via schemas-empty fast path")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// TestMiddlewareBodyReadError verifies that a body read failure returns 400
// and does not call the downstream handler.
func TestMiddlewareBodyReadError(t *testing.T) {
	schema := &openapi.Schema{Type: "object"}
	routes := []openapi.RouteDescriptor{
		{Path: "/test", Operation: "test", Summary: "Test", RequestSchema: schema},
	}
	handlerCalled := false
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(validate.Middleware(routes))
		r.Post("/test", func(w http.ResponseWriter, _ *http.Request) {
			handlerCalled = true
			w.WriteHeader(http.StatusOK)
		})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/test", nil)
	req.Body = &errReader{err: errors.New("simulated read failure")}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if handlerCalled {
		t.Error("downstream handler must NOT be called on body read error")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on body read error, got %d", w.Code)
	}
}

// TestMiddlewareNonPostPassThrough verifies that non-POST requests to
// schema-bearing paths are passed through without body validation.
func TestMiddlewareNonPostPassThrough(t *testing.T) {
	schema := &openapi.Schema{
		Type:     "object",
		Required: []string{"name"},
		Properties: map[string]*openapi.Schema{
			"name": {Type: "string"},
		},
	}
	reached := false
	router := makeTestRouter(schema, func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	// A GET to a schema-bearing route should not trigger body validation.
	req := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// chi returns 405 for method-not-allowed; the validation middleware must not
	// return a 400 before chi gets a chance to respond.
	if w.Code == http.StatusBadRequest {
		t.Fatalf("non-POST request incorrectly returned 400 from validation middleware")
	}
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 from chi, got %d", w.Code)
	}
	if reached {
		t.Error("handler must not be reached for non-POST request (chi returns 405 before routing)")
	}
}

// --- Additional edge-case tests ---

// TestValidateNullValues verifies that JSON null is handled correctly:
// typed schemas reject null, typeless schemas (or type:"null") accept it.
func TestValidateNullValues(t *testing.T) {
	var nilVal any // JSON null becomes nil in any

	// Typed schema: null must fail.
	if err := validate.Validate(&openapi.Schema{Type: "string"}, nilVal); err == nil {
		t.Error("null against type:string should fail")
	}
	if err := validate.Validate(&openapi.Schema{Type: "object"}, nilVal); err == nil {
		t.Error("null against type:object should fail")
	}
	if err := validate.Validate(&openapi.Schema{Type: "integer"}, nilVal); err == nil {
		t.Error("null against type:integer should fail")
	}

	// Explicit null type: must pass.
	if err := validate.Validate(&openapi.Schema{Type: "null"}, nilVal); err != nil {
		t.Errorf("null against type:null should pass, got: %v", err)
	}

	// No type constraint: null is permitted.
	if err := validate.Validate(&openapi.Schema{}, nilVal); err != nil {
		t.Errorf("null against typeless schema should pass, got: %v", err)
	}
}

// TestValidateRefSchemaPassThrough verifies that a schema that is purely a $ref
// (no local constraints) passes through any value without validation.
func TestValidateRefSchemaPassThrough(t *testing.T) {
	refSchema := &openapi.Schema{Ref: "#/components/schemas/SomeType"}
	for _, val := range []any{"string", float64(42), nil, map[string]any{}} {
		if err := validate.Validate(refSchema, val); err != nil {
			t.Errorf("$ref schema should pass through %T, got: %v", val, err)
		}
	}
}

// TestValidateNestedObject verifies that nested objects are recursively validated.
func TestValidateNestedObject(t *testing.T) {
	schema := &openapi.Schema{
		Type: "object",
		Properties: map[string]*openapi.Schema{
			"address": {
				Type:     "object",
				Required: []string{"city"},
				Properties: map[string]*openapi.Schema{
					"city":  {Type: "string"},
					"zip":   {Type: "string", Pattern: `^\d{5}$`},
				},
			},
		},
	}

	// Valid nested object.
	err := validate.ValidateBytes(schema, []byte(`{"address":{"city":"Boston","zip":"02101"}}`))
	if err != nil {
		t.Errorf("valid nested object: %v", err)
	}

	// Missing required nested field.
	err = validate.ValidateBytes(schema, []byte(`{"address":{}}`))
	if err == nil {
		t.Error("missing required nested field should fail")
	}

	// Invalid nested field pattern.
	err = validate.ValidateBytes(schema, []byte(`{"address":{"city":"Boston","zip":"bad"}}`))
	if err == nil {
		t.Error("invalid nested pattern should fail")
	}
	if err != nil && !strings.Contains(err.Error(), "zip") {
		t.Errorf("error should name failing field 'zip', got: %v", err)
	}
}

// TestValidateNumericEnumNotations verifies that numeric enum values are compared
// by value so different JSON notations of the same number match (1 == 1.0 == 1e0).
func TestValidateNumericEnumNotations(t *testing.T) {
	schema := &openapi.Schema{
		Type: "object",
		Properties: map[string]*openapi.Schema{
			"level": {Type: "integer", Enum: []any{float64(1), float64(2), float64(3)}},
		},
	}
	// Plain integer notation.
	if err := validate.ValidateBytes(schema, []byte(`{"level":2}`)); err != nil {
		t.Errorf("plain integer 2 in enum: %v", err)
	}
	// 1.0 notation should match float64(1) in enum.
	if err := validate.ValidateBytes(schema, []byte(`{"level":1.0}`)); err != nil {
		t.Errorf("1.0 notation should match enum value 1: %v", err)
	}
	// Value not in enum must fail.
	if err := validate.ValidateBytes(schema, []byte(`{"level":9}`)); err == nil {
		t.Error("value 9 not in enum should fail")
	}
}

// TestConcurrentPatternCache verifies that compiledPattern is safe under
// concurrent access (no data race on the sync.Map cache).
func TestConcurrentPatternCache(t *testing.T) {
	schema := &openapi.Schema{
		Type: "object",
		Properties: map[string]*openapi.Schema{
			"code": {Type: "string", Pattern: `^\d{4}$`},
		},
	}
	const goroutines = 50
	errs := make(chan error, goroutines)
	for range goroutines {
		go func() {
			errs <- validate.ValidateBytes(schema, []byte(`{"code":"1234"}`))
		}()
	}
	for range goroutines {
		if err := <-errs; err != nil {
			t.Errorf("concurrent validation error: %v", err)
		}
	}
}

// TestValidateBoolValue verifies that bool values are handled correctly:
// accepted for type:boolean, rejected for other types, matched in enum.
func TestValidateBoolValue(t *testing.T) {
	// type:boolean accepts bool.
	schema := &openapi.Schema{Type: "object", Properties: map[string]*openapi.Schema{
		"active": {Type: "boolean"},
	}}
	if err := validate.Validate(schema, map[string]any{"active": true}); err != nil {
		t.Errorf("bool with type:boolean should pass: %v", err)
	}

	// type:string rejects bool.
	schema2 := &openapi.Schema{Type: "object", Properties: map[string]*openapi.Schema{
		"active": {Type: "string"},
	}}
	if err := validate.Validate(schema2, map[string]any{"active": true}); err == nil {
		t.Error("bool against type:string should fail")
	}

	// bool in enum matches correctly.
	schema3 := &openapi.Schema{Type: "object", Properties: map[string]*openapi.Schema{
		"flag": {Enum: []any{true, false}},
	}}
	if err := validate.Validate(schema3, map[string]any{"flag": true}); err != nil {
		t.Errorf("bool true in enum [true,false] should pass: %v", err)
	}
	if err := validate.Validate(schema3, map[string]any{"flag": "yes"}); err == nil {
		t.Error("string 'yes' not in bool enum should fail")
	}
}

// TestValidateUnknownTypeRejected verifies that the default case in validateValue
// rejects non-JSON-standard types passed directly to Validate.
func TestValidateUnknownTypeRejected(t *testing.T) {
	// A typeless schema with an unknown Go type should be rejected by the default case.
	schema := &openapi.Schema{}
	type customType struct{ X int }
	err := validate.Validate(schema, customType{X: 1})
	if err == nil {
		t.Error("non-JSON Go type should be rejected by validateValue default case")
	}
}

// TestMiddlewareTrailingSlashPassThrough verifies that a trailing-slash request
// misses the schema map and is not intercepted by validation. chi returns 404
// because /v1/test/ does not match the registered /v1/test pattern.
func TestMiddlewareTrailingSlashPassThrough(t *testing.T) {
	schema := &openapi.Schema{
		Type:     "object",
		Required: []string{"name"},
		Properties: map[string]*openapi.Schema{"name": {Type: "string"}},
	}
	reached := false
	router := makeTestRouter(schema, func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/test/", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code == http.StatusBadRequest {
		t.Fatalf("trailing slash request returned 400 from validation — middleware must not intercept it")
	}
	// chi does not match /v1/test/ to /v1/test, so the handler is unreachable
	// and chi returns 404.
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 from chi for trailing slash, got %d", w.Code)
	}
	if reached {
		t.Error("inner handler must not be reached for trailing slash request")
	}
}

// TestBoolFalseSchema verifies that a schema with BoolFalse:true rejects every
// value when used as a property schema (not just via additionalProperties).
func TestBoolFalseSchema(t *testing.T) {
	schema := &openapi.Schema{
		Type: "object",
		Properties: map[string]*openapi.Schema{
			"allowed": {Type: "string"},
			"banned":  {BoolFalse: true},
		},
	}
	// "banned" field present: should fail because schema is false.
	if err := validate.Validate(schema, map[string]any{"allowed": "ok", "banned": "anything"}); err == nil {
		t.Error("BoolFalse property schema should reject any value")
	}
	// Without "banned" field: passes.
	if err := validate.Validate(schema, map[string]any{"allowed": "ok"}); err != nil {
		t.Errorf("object without banned field should pass: %v", err)
	}
}

// TestIntegerTypeLargeValues verifies that the integer type check accepts
// integers beyond int64 range without rejecting due to int64 overflow, and
// that fractions within float64 precision are still rejected.
func TestIntegerTypeLargeValues(t *testing.T) {
	schema := &openapi.Schema{
		Type: "object",
		Properties: map[string]*openapi.Schema{
			"id": {Type: "integer"},
		},
	}
	// 2^63+1 is beyond int64 range; the old int64-cast code overflows and
	// incorrectly rejects this. The string-based check must accept it.
	if err := validate.ValidateBytes(schema, []byte(`{"id":9223372036854775808}`)); err != nil {
		t.Errorf("large integer beyond int64 should be accepted as integer type: %v", err)
	}
	// Decimal notation of the same large integer should also be accepted.
	if err := validate.ValidateBytes(schema, []byte(`{"id":9223372036854775808.0}`)); err != nil {
		t.Errorf("large integer with trailing .0 should be accepted: %v", err)
	}
	// A fraction within float64 precision must still be rejected.
	if err := validate.ValidateBytes(schema, []byte(`{"id":1.5}`)); err == nil {
		t.Error("fractional value 1.5 should be rejected as non-integer")
	}
}

// TestCompositeEnumWithNumbers verifies that object/array enum values containing
// numbers compare correctly regardless of whether the schema uses float64 literals
// and the request is decoded with UseNumber (json.Number).
func TestCompositeEnumWithNumbers(t *testing.T) {
	schema := &openapi.Schema{
		Type: "object",
		Properties: map[string]*openapi.Schema{
			"config": {Enum: []any{
				map[string]any{"level": float64(1)},
				map[string]any{"level": float64(2)},
			}},
		},
	}
	// Request value decoded via UseNumber produces json.Number("1"), not float64(1).
	// normalizeNumbers must bridge this gap.
	if err := validate.ValidateBytes(schema, []byte(`{"config":{"level":1}}`)); err != nil {
		t.Errorf("composite enum with float64 literals should match json.Number from request: %v", err)
	}
	if err := validate.ValidateBytes(schema, []byte(`{"config":{"level":3}}`)); err == nil {
		t.Error("value not in composite enum should fail")
	}
}

// TestCompileSchemaPatterns verifies that CompileSchemaPatterns returns nil for
// valid RE2 patterns and an error for patterns that fail Go's regexp.Compile.
func TestCompileSchemaPatterns(t *testing.T) {
	t.Run("valid patterns return nil", func(t *testing.T) {
		routes := []openapi.RouteDescriptor{
			{
				Path: "/test", Operation: "test", Summary: "Test",
				RequestSchema: &openapi.Schema{
					Type: "object",
					Properties: map[string]*openapi.Schema{
						"code": {Type: "string", Pattern: `^\d{4}$`},
					},
				},
			},
		}
		if err := validate.CompileSchemaPatterns(routes); err != nil {
			t.Errorf("valid patterns should return nil, got: %v", err)
		}
	})

	t.Run("invalid RE2 pattern returns error", func(t *testing.T) {
		routes := []openapi.RouteDescriptor{
			{
				Path: "/test", Operation: "test", Summary: "Test",
				RequestSchema: &openapi.Schema{
					Type: "object",
					Properties: map[string]*openapi.Schema{
						// Go's RE2 does not support lookahead assertions.
						"s": {Type: "string", Pattern: `(?=.*\d)`},
					},
				},
			},
		}
		if err := validate.CompileSchemaPatterns(routes); err == nil {
			t.Error("invalid RE2 pattern should return error")
		}
	})

	t.Run("nil RequestSchema is skipped", func(t *testing.T) {
		routes := []openapi.RouteDescriptor{
			{Path: "/test", Operation: "test", Summary: "No schema"},
		}
		if err := validate.CompileSchemaPatterns(routes); err != nil {
			t.Errorf("nil schema should be skipped, got: %v", err)
		}
	})
}

// TestPatternInvalidRegexReturnsSchemaError verifies that an invalid RE2 pattern
// in a schema returns a descriptive schema-authoring error rather than silently
// skipping the constraint. CompileSchemaPatterns catches this at startup; if
// somehow reached at request time, the error clearly blames the schema, not the
// client's value.
func TestPatternInvalidRegexReturnsSchemaError(t *testing.T) {
	schema := &openapi.Schema{
		Type: "object",
		Properties: map[string]*openapi.Schema{
			// Lookahead — valid ECMAScript, invalid RE2.
			"s": {Type: "string", Pattern: `(?=.*\d)`},
		},
	}
	err := validate.Validate(schema, map[string]any{"s": "hello"})
	if err == nil {
		t.Fatal("invalid RE2 pattern should return a schema error")
	}
	if !strings.Contains(err.Error(), "invalid schema pattern") {
		t.Errorf("error should describe schema error, got: %v", err)
	}
}

// TestSciNotationEnumMatch verifies that scientific notation numbers with
// fractional mantissas that evaluate to integers (e.g. 1.5e2 = 150) compare
// equal to their plain integer form in enum checks.
// This exercises the sciNotationToInt path inside normalizeJSONNumber.
func TestSciNotationEnumMatch(t *testing.T) {
	tests := []struct {
		name    string
		enum    []any
		body    string
		wantErr bool
	}{
		// request body "150" against schema enum containing json.Number("1.5e2")
		{"request 150 matches enum 1.5e2", []any{json.Number("1.5e2")}, `{"v":150}`, false},
		// inverse: request body "1.5e2" against schema enum containing float64(150)
		{"request 1.5e2 matches enum float64(150)", []any{float64(150)}, `{"v":1.5e2}`, false},
		// 1.23e3 = 1230
		{"1.23e3 matches 1230", []any{float64(1230)}, `{"v":1.23e3}`, false},
		// 1.5e1 = 15
		{"1.5e1 matches 15", []any{float64(15)}, `{"v":1.5e1}`, false},
		// 1.5e0 = 1.5 (not integer): must NOT match float64(1)
		{"1.5 does not match integer 1", []any{float64(1)}, `{"v":1.5}`, true},
		// value not in enum always fails
		{"200 not in enum [150]", []any{float64(150)}, `{"v":200}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &openapi.Schema{
				Type: "object",
				Properties: map[string]*openapi.Schema{
					"v": {Enum: tc.enum},
				},
			}
			err := validate.ValidateBytes(s, []byte(tc.body))
			if tc.wantErr && err == nil {
				t.Fatal("expected enum mismatch error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestValidateRefSchemaWithConstraints documents the behaviour when a schema has
// both a $ref and local constraints: the $ref short-circuits validation and the
// local constraints are skipped (pass-through). This is intentional — this
// validator does not resolve $ref, and a schema that starts with $ref is treated
// as an unresolved reference that cannot be locally validated.
func TestValidateRefSchemaWithConstraints(t *testing.T) {
	// Ref + Type: the integer constraint would reject "hello", but $ref wins.
	refWithType := &openapi.Schema{
		Ref:  "#/components/schemas/SomeType",
		Type: "integer",
	}
	if err := validate.Validate(refWithType, "hello"); err != nil {
		t.Errorf("$ref with Type constraint: expected pass-through for string, got: %v", err)
	}

	// Ref + Required: would normally reject an empty object.
	refWithRequired := &openapi.Schema{
		Ref:      "#/components/schemas/SomeType",
		Required: []string{"name"},
	}
	if err := validate.Validate(refWithRequired, map[string]any{}); err != nil {
		t.Errorf("$ref with Required: expected pass-through for empty object, got: %v", err)
	}

	// Ref + Enum: would normally reject a value not in the enum.
	refWithEnum := &openapi.Schema{
		Ref:  "#/components/schemas/SomeType",
		Enum: []any{"a", "b"},
	}
	if err := validate.Validate(refWithEnum, "z"); err != nil {
		t.Errorf("$ref with Enum: expected pass-through for out-of-enum value, got: %v", err)
	}
}

// TestNumericEnumHighPrecision verifies that integer enum values beyond float64's
// 53-bit mantissa are compared without precision loss so that neighbouring
// high-precision integers do NOT match each other, and that decimal notation of
// the same value (9007199254740993.0) correctly matches the integer enum entry.
func TestNumericEnumHighPrecision(t *testing.T) {
	// 9007199254740993 == 2^53+1; this and 2^53 both collapse to the same float64.
	// The validator must distinguish them via integer comparison, not float64.
	schema := &openapi.Schema{
		Type: "object",
		Properties: map[string]*openapi.Schema{
			// enum contains only 9007199254740993
			"id": {Enum: []any{json.Number("9007199254740993")}},
		},
	}

	// Exact value should pass.
	if err := validate.ValidateBytes(schema, []byte(`{"id":9007199254740993}`)); err != nil {
		t.Errorf("exact high-precision enum value should pass: %v", err)
	}

	// Neighbouring value must fail (float64 precision loss would make them equal —
	// the int64 comparison path must be taken instead).
	if err := validate.ValidateBytes(schema, []byte(`{"id":9007199254740992}`)); err == nil {
		t.Error("neighbouring high-precision integer should fail enum check (float64 precision loss bug)")
	}

	// Decimal notation of the same integer (9007199254740993.0) should also pass
	// after normalizeJSONNumber strips the trailing ".0".
	schemaWithDecimalEnum := &openapi.Schema{
		Type: "object",
		Properties: map[string]*openapi.Schema{
			"id": {Enum: []any{json.Number("9007199254740993.0")}},
		},
	}
	if err := validate.ValidateBytes(schemaWithDecimalEnum, []byte(`{"id":9007199254740993}`)); err != nil {
		t.Errorf("decimal enum notation 9007199254740993.0 should match integer input: %v", err)
	}

	// Values in uint64 range [2^63, 2^64-1] must be compared via uint64, not
	// float64 (which rounds both to the same imprecise value).
	schemaUint64 := &openapi.Schema{
		Type: "object",
		Properties: map[string]*openapi.Schema{
			"id": {Enum: []any{json.Number("9223372036854775809")}}, // 2^63+1
		},
	}
	if err := validate.ValidateBytes(schemaUint64, []byte(`{"id":9223372036854775809}`)); err != nil {
		t.Errorf("exact uint64 enum value should pass: %v", err)
	}
	if err := validate.ValidateBytes(schemaUint64, []byte(`{"id":9223372036854775808}`)); err == nil {
		t.Error("neighbouring uint64 integer should fail enum check")
	}
}
