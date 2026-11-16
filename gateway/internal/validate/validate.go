// Package validate provides a stdlib-only JSON Schema subset validator for the
// Crucible gateway's request-body validation middleware.
//
// Supported constraints: type, required, properties, additionalProperties,
// enum, minLength, maxLength, pattern, minimum, maximum.
package validate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
)

// patternCache stores compiled *regexp.Regexp values keyed by pattern string
// so patterns are compiled at most once per unique regex across all requests.
// Cache size is bounded by the number of unique patterns in statically-defined
// schemas; no eviction is needed under normal operation.
var patternCache sync.Map

func compiledPattern(pattern string) (*regexp.Regexp, error) {
	if v, ok := patternCache.Load(pattern); ok {
		if re, ok := v.(*regexp.Regexp); ok {
			return re, nil
		}
		// Corrupt cache entry (should never happen); fall through to recompile.
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	// Store may lose a race with another goroutine; that's fine — compiled results
	// are identical, and we return the freshly compiled one either way.
	patternCache.Store(pattern, re)
	return re, nil
}

// CompileSchemaPatterns pre-compiles every regex pattern reachable from the
// given routes and returns an error describing the first one that fails.
// Call this at gateway startup so RE2-incompatible patterns (e.g. ECMAScript
// lookaheads) are caught during initialisation rather than silently skipped
// at request time. Returns nil when all patterns compile successfully.
func CompileSchemaPatterns(routes []openapi.RouteDescriptor) error {
	for _, rt := range routes {
		if rt.RequestSchema != nil {
			if err := compileSchemaPatterns(rt.RequestSchema); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateSampleRequests validates every route's non-nil RouteDescriptor.SampleRequest
// against that route's RequestSchema and returns an error describing the first
// violation. Call this at gateway startup (see server.NewRouter, next to
// CompileSchemaPatterns) so a SampleRequest that has drifted from its own
// RequestSchema — the same class of authoring mistake CompileSchemaPatterns
// catches for regex patterns — is caught during initialization instead of
// silently shipping a broken /openapi.json example and a no-op acceptance-test
// payload. Returns nil when every non-nil SampleRequest validates (or no route
// declares one).
func ValidateSampleRequests(routes []openapi.RouteDescriptor) error {
	for _, rt := range routes {
		if rt.SampleRequest == nil {
			continue
		}
		if err := ValidateBytes(rt.RequestSchema, rt.SampleRequest); err != nil {
			return fmt.Errorf("route %s: SampleRequest fails validation against RequestSchema: %w", rt.Path, err)
		}
	}
	return nil
}

func compileSchemaPatterns(s *openapi.Schema) error {
	if s == nil {
		return nil
	}
	if s.Pattern != "" {
		if _, err := compiledPattern(s.Pattern); err != nil {
			return fmt.Errorf("schema pattern %q is not valid RE2 syntax: %w", s.Pattern, err)
		}
	}
	for _, ps := range s.Properties {
		if err := compileSchemaPatterns(ps); err != nil {
			return err
		}
	}
	if err := compileSchemaPatterns(s.AdditionalProperties); err != nil {
		return err
	}
	return nil
}

// ValidationError names the failing field and describes the constraint violation.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

// Validate validates a pre-decoded JSON value (as returned by json.Unmarshal
// into an any) against schema. Returns nil when valid or when schema is nil.
func Validate(schema *openapi.Schema, data any) error {
	if schema == nil {
		return nil
	}
	return validateValue(schema, data, "")
}

// ValidateBytes parses raw JSON and validates it against schema.
// Returns nil when schema is nil (pass-through) or when the body is valid.
//
// Uses json.Decoder with UseNumber so that large integer values are not
// silently rounded to float64 before the integer type-check.
func ValidateBytes(schema *openapi.Schema, body []byte) error {
	if schema == nil {
		return nil
	}
	if len(body) == 0 {
		return &ValidationError{Message: "request body is empty"}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var data any
	if err := dec.Decode(&data); err != nil {
		return &ValidationError{Message: "invalid JSON body"}
	}
	// Reject trailing tokens — a valid request body contains exactly one JSON value.
	// Any non-EOF result (valid JSON, garbage, syntax error) means there is extra
	// content after the primary value; the body is the client's fault either way.
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		return &ValidationError{Message: "trailing data after JSON value"}
	}
	return Validate(schema, data)
}

// validateValue recurses into the schema tree, building a dot-separated field
// path for error messages.
func validateValue(s *openapi.Schema, value any, path string) error {
	if s == nil {
		return nil
	}
	// BoolFalse is the JSON Schema boolean false — the schema rejects every value.
	if s.BoolFalse {
		return &ValidationError{Field: path, Message: "value is not allowed by schema (false)"}
	}
	// $ref schemas are not resolved by this validator — inline schemas only.
	// A schema that is purely a $ref has no local constraints, so we pass through.
	if s.Ref != "" {
		return nil
	}

	// Type check — before enum so the error names the correct constraint first.
	if s.Type != "" {
		if err := checkType(s.Type, value, path); err != nil {
			return err
		}
	}

	// Enum check — runs before structural (object) validation so that schemas
	// combining enum with Properties/Required validate enum values of any Go type
	// without incorrect rejection from the object-dispatch branch.
	if len(s.Enum) > 0 {
		if !inEnum(s.Enum, value) {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("must be one of: %s", formatEnum(s.Enum)),
			}
		}
	}

	switch v := value.(type) {
	case map[string]any:
		// Reaching this branch is the implicit object-type guard: only map[string]any
		// values are routed to validateObject, which checks Properties and Required.
		return validateObject(s, v, path)
	case string:
		return validateString(s, v, path)
	case float64:
		// Convert to json.Number so validateNumber can use precision-safe integer
		// comparison for Minimum/Maximum checks.
		return validateNumber(s, json.Number(strconv.FormatFloat(v, 'f', -1, 64)), path)
	case json.Number:
		// Pass json.Number directly — no float64 conversion here, preserving
		// large-integer precision for Minimum/Maximum comparisons.
		return validateNumber(s, v, path)
	case []any:
		// Array values pass through; no array-specific constraints are implemented.
		return nil
	case nil:
		// JSON null (any(nil)): reject when schema declares a non-null type.
		// This case MUST be explicit — a missing case nil would allow null to
		// bypass type constraints by falling to the default branch.
		if s.Type != "" && s.Type != "null" {
			return typeError(path, s.Type, value)
		}
		return nil
	case bool:
		// bool has no constraints beyond type and enum, both handled above.
		return nil
	default:
		// Any type that does not originate from standard JSON decoding is rejected.
		return &ValidationError{
			Field:   path,
			Message: fmt.Sprintf("unsupported value type %T", value),
		}
	}
}

func validateObject(s *openapi.Schema, obj map[string]any, path string) error {
	if s.Type != "" && s.Type != "object" {
		return typeError(path, s.Type, obj)
	}
	// Required fields must be present.
	for _, req := range s.Required {
		if _, present := obj[req]; !present {
			return &ValidationError{Field: joinPath(path, req), Message: "required field missing"}
		}
	}

	// Validate each present field against its property schema (if defined),
	// and enforce additionalProperties constraints for undefined fields.
	for k, v := range obj {
		propSchema, defined := s.Properties[k]
		if !defined {
			if s.AdditionalProperties != nil {
				if s.AdditionalProperties.BoolFalse {
					return &ValidationError{Field: joinPath(path, k), Message: "additional property not allowed"}
				}
				// additionalProperties is a schema — validate the value against it.
				if err := validateValue(s.AdditionalProperties, v, joinPath(path, k)); err != nil {
					return err
				}
			}
			continue
		}
		if err := validateValue(propSchema, v, joinPath(path, k)); err != nil {
			return err
		}
	}
	return nil
}

func validateString(s *openapi.Schema, v string, path string) error {
	// JSON Schema defines string length in Unicode code points, not bytes.
	runes := utf8.RuneCountInString(v)
	if s.MinLength != nil && runes < *s.MinLength {
		return &ValidationError{
			Field:   path,
			Message: fmt.Sprintf("must be at least %d characters long", *s.MinLength),
		}
	}
	if s.MaxLength != nil && runes > *s.MaxLength {
		return &ValidationError{
			Field:   path,
			Message: fmt.Sprintf("must be at most %d characters long", *s.MaxLength),
		}
	}
	if s.Pattern != "" {
		re, err := compiledPattern(s.Pattern)
		if err != nil {
			// The pattern is a schema authoring bug (e.g. ECMAScript-only syntax
			// unsupported by Go's RE2). CompileSchemaPatterns is called at startup
			// to catch this; reaching here at request time is a server-side error.
			// Return a generic message to avoid leaking schema internals to the client.
			return &ValidationError{Field: path, Message: "internal schema validation error"}
		}
		if !re.MatchString(v) {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("must match pattern %q", s.Pattern),
			}
		}
	}
	return nil
}

// cmpNumBound compares a json.Number value against a float64 bound.
// Both are normalized to json.Number first, then compared via string fast path,
// int64, uint64, and float64 fallback — the same strategy as numericEqual —
// so large integers and negative bounds are handled precisely.
// Returns -1 if v < bound, 0 if equal, +1 if v > bound.
func cmpNumBound(v json.Number, bound float64) int {
	nb := normalizeJSONNumber(json.Number(strconv.FormatFloat(bound, 'f', -1, 64)))
	nv := normalizeJSONNumber(v)
	if nv == nb {
		return 0
	}
	ia, errA := strconv.ParseInt(string(nv), 10, 64)
	ib, errB := strconv.ParseInt(string(nb), 10, 64)
	if errA == nil && errB == nil {
		if ia < ib {
			return -1
		}
		if ia > ib {
			return 1
		}
		return 0
	}
	ua, errA := strconv.ParseUint(string(nv), 10, 64)
	ub, errB := strconv.ParseUint(string(nb), 10, 64)
	if errA == nil && errB == nil {
		if ua < ub {
			return -1
		}
		if ua > ub {
			return 1
		}
		return 0
	}
	fv, errA := nv.Float64()
	fb, errB := nb.Float64()
	if errA != nil || errB != nil {
		return 0
	}
	if fv < fb {
		return -1
	}
	if fv > fb {
		return 1
	}
	return 0
}

func validateNumber(s *openapi.Schema, v json.Number, path string) error {
	norm := normalizeJSONNumber(v)
	if s.Minimum != nil && cmpNumBound(norm, *s.Minimum) < 0 {
		return &ValidationError{
			Field:   path,
			Message: fmt.Sprintf("must be >= %v", *s.Minimum),
		}
	}
	if s.Maximum != nil && cmpNumBound(norm, *s.Maximum) > 0 {
		return &ValidationError{
			Field:   path,
			Message: fmt.Sprintf("must be <= %v", *s.Maximum),
		}
	}
	return nil
}

func checkType(typeName string, value any, path string) error {
	switch typeName {
	case "string":
		if _, ok := value.(string); !ok {
			return typeError(path, typeName, value)
		}
	case "number":
		switch value.(type) {
		case float64, json.Number:
			// both are valid JSON number representations
		default:
			return typeError(path, typeName, value)
		}
	case "integer":
		switch v := value.(type) {
		case float64:
			// Use math.Trunc to avoid int64 overflow for values > math.MaxInt64.
			if v != math.Trunc(v) {
				return typeError(path, typeName, value)
			}
		case json.Number:
			if !isIntegerNumber(v) {
				return typeError(path, typeName, value)
			}
		default:
			return typeError(path, typeName, value)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return typeError(path, typeName, value)
		}
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return typeError(path, typeName, value)
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return typeError(path, typeName, value)
		}
	case "null":
		if value != nil {
			return typeError(path, typeName, value)
		}
	}
	return nil
}

func typeError(path, expected string, got any) *ValidationError {
	return &ValidationError{
		Field:   path,
		Message: fmt.Sprintf("must be %s, got %T", expected, got),
	}
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

// inEnum checks whether value is present in enum.
// Uses deepEqualJSON so numeric comparisons (json.Number vs float64) are handled
// precisely via numericEqual without float64 precision loss for large integers.
func inEnum(enum []any, value any) bool {
	for _, e := range enum {
		if deepEqualJSON(value, e) {
			return true
		}
	}
	return false
}

// deepEqualJSON compares two JSON-decoded values for equality.
// Numbers (json.Number or float64) are compared via numericEqual, which uses
// int64/uint64 paths for precision-safe integer comparison before falling back
// to float64. Composite values (objects, arrays) recurse. All other types fall
// back to reflect.DeepEqual.
func deepEqualJSON(a, b any) bool {
	if isNumeric(a) {
		if isNumeric(b) {
			return numericEqual(a, b)
		}
		return false
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			if !deepEqualJSON(v, bv[k]) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !deepEqualJSON(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}

func formatEnum(enum []any) string {
	parts := make([]string, 0, len(enum))
	for _, e := range enum {
		b, _ := json.Marshal(e)
		parts = append(parts, string(b))
	}
	return strings.Join(parts, ", ")
}
