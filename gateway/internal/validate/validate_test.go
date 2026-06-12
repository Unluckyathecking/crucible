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
}

// TestValidateBytesIntegerPrecision verifies that ValidateBytes uses UseNumber()
// so that fractional values above 2^53 are still rejected as non-integers.
func TestValidateBytesIntegerPrecision(t *testing.T) {
	schema := &openapi.Schema{
		Type: "object",
		Properties: map[string]*openapi.Schema{
			"id": {Type: "integer"},
		},
	}
	// 9007199254740992.5 — float64 rounds this to a whole number, losing the fraction.
	// UseNumber preserves the raw string so Int64() rejects it.
	err := validate.ValidateBytes(schema, []byte(`{"id":9007199254740992.5}`))
	if err == nil {
		t.Fatal("expected error for fractional value > 2^53, got nil")
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
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
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
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
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
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(validate.Middleware(routes))
		r.Post("/test", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/test", nil)
	req.Body = &errReader{err: errors.New("simulated read failure")}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

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

	// chi returns 405 for method-not-allowed; validation must not return 400.
	if w.Code == http.StatusBadRequest {
		t.Fatalf("non-POST request incorrectly returned 400 from validation middleware")
	}
	_ = reached // handler reachability depends on chi's method matching
}
