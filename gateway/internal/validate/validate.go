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
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
)

// patternCache stores compiled *regexp.Regexp values keyed by pattern string
// so patterns are compiled at most once per unique regex across all requests.
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
	// A second Decode must return io.EOF; anything else means junk after the value.
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		return &ValidationError{Message: "invalid JSON body"}
	}
	return Validate(schema, data)
}

// validateValue recurses into the schema tree, building a dot-separated field
// path for error messages.
func validateValue(s *openapi.Schema, value any, path string) error {
	if s == nil {
		return nil
	}
	// $ref schemas are not resolved by this validator — inline schemas only.
	// A schema that is purely a $ref has no local constraints, so we pass through.
	if s.Ref != "" {
		return nil
	}

	// Type check — performed before property/enum checks so the error message
	// names the correct constraint first.
	if s.Type != "" {
		if err := checkType(s.Type, value, path); err != nil {
			return err
		}
	}

	// Enum check.
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
		return validateObject(s, v, path)
	case string:
		return validateString(s, v, path)
	case float64:
		return validateNumber(s, v, path)
	case json.Number:
		// json.Number arrives when the caller decoded with UseNumber().
		// The type check above already accepted it; convert for constraint checks.
		f, err := v.Float64()
		if err != nil {
			return &ValidationError{Field: path, Message: "invalid number"}
		}
		return validateNumber(s, f, path)
	case []any:
		// Array values pass through; no array-specific constraints are implemented.
		return nil
	case nil:
		// JSON null: only valid when the schema type is absent or explicitly "null".
		if s.Type != "" && s.Type != "null" {
			return typeError(path, s.Type, value)
		}
		return nil
	}

	return nil
}

func validateObject(s *openapi.Schema, obj map[string]any, path string) error {
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
			// Invalid regex is a schema authoring bug; surface it clearly.
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("invalid schema pattern %q", s.Pattern),
			}
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

func validateNumber(s *openapi.Schema, v float64, path string) error {
	if s.Minimum != nil && v < *s.Minimum {
		return &ValidationError{
			Field:   path,
			Message: fmt.Sprintf("must be >= %v", *s.Minimum),
		}
	}
	if s.Maximum != nil && v > *s.Maximum {
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
			if v != float64(int64(v)) {
				return typeError(path, typeName, value)
			}
		case json.Number:
			// JSON Schema treats any number with zero fractional part as integer,
			// including notations like 1.0 or 1e3. Check via Float64.
			f, err := v.Float64()
			if err != nil || f != float64(int64(f)) {
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
// Numeric values are compared as float64 so that notations like 1, 1.0, and 1e0
// are treated as equal (JSON Schema numeric equality). Non-numeric values fall
// back to JSON-byte comparison (correct for strings, bools, null).
func inEnum(enum []any, value any) bool {
	if vf, ok := numericFloat(value); ok {
		for _, e := range enum {
			if ef, ok := numericFloat(e); ok && vf == ef {
				return true
			}
		}
		return false
	}
	vb, err := json.Marshal(value)
	if err != nil {
		return false
	}
	for _, e := range enum {
		eb, err := json.Marshal(e)
		if err != nil {
			continue
		}
		if string(vb) == string(eb) {
			return true
		}
	}
	return false
}

// numericFloat extracts a float64 from float64 or json.Number values.
func numericFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func formatEnum(enum []any) string {
	parts := make([]string, 0, len(enum))
	for _, e := range enum {
		b, _ := json.Marshal(e)
		parts = append(parts, string(b))
	}
	return strings.Join(parts, ", ")
}
