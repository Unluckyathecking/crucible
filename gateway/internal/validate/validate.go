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
			// to catch this before traffic; reaching here at request time is a
			// programming error in the clone, not a client error.
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
// Numeric values use numericEqual so that notations like 1, 1.0, and 1e0
// are treated as equal per JSON Schema numeric equality while preserving
// precision for integers beyond float64's 53-bit mantissa (e.g. 2^53+1).
// Non-numeric values use reflect.DeepEqual which handles map key-ordering
// non-determinism correctly.
func inEnum(enum []any, value any) bool {
	if isNumeric(value) {
		for _, e := range enum {
			if isNumeric(e) && numericEqual(value, e) {
				return true
			}
		}
		return false
	}
	// For composite values (objects, arrays), json.Number in the decoded request
	// and float64 in schema literals must compare equal. Normalize both sides so
	// reflect.DeepEqual sees the same numeric type throughout the structure.
	normValue := normalizeNumbers(value)
	for _, e := range enum {
		if reflect.DeepEqual(normValue, normalizeNumbers(e)) {
			return true
		}
	}
	return false
}

// normalizeNumbers walks a decoded JSON value and converts all json.Number
// values to float64 so that reflect.DeepEqual can compare request values
// (decoded with UseNumber) against schema enum literals (Go float64).
func normalizeNumbers(v any) any {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return v
		}
		return f
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, val := range n {
			out[k] = normalizeNumbers(val)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, val := range n {
			out[i] = normalizeNumbers(val)
		}
		return out
	}
	return v
}

func isNumeric(v any) bool {
	switch v.(type) {
	case float64, json.Number:
		return true
	}
	return false
}

// numericEqual compares two numeric values (float64 or json.Number) for
// JSON Schema numeric equality. Strategy:
//  1. Normalize: strip trailing fractional zeros ("1.0" → "1", "1.50" → "1.5")
//     so that integer-valued decimals enter the integer path, not float64.
//  2. String equality fast path: normalized identical strings are equal.
//  3. Integer equality via int64: precision-safe for values up to ±2^63-1.
//  4. Float64 fallback: handles true decimals like 1.5 vs 1.50.
func numericEqual(a, b any) bool {
	na, aIsNum := toJSONNumber(a)
	nb, bIsNum := toJSONNumber(b)
	if !aIsNum || !bIsNum {
		return false
	}
	na = normalizeJSONNumber(na)
	nb = normalizeJSONNumber(nb)
	if na == nb {
		return true
	}
	// Integer path: parse both as int64 for precision-safe comparison.
	ia, errA := strconv.ParseInt(string(na), 10, 64)
	ib, errB := strconv.ParseInt(string(nb), 10, 64)
	if errA == nil && errB == nil {
		return ia == ib
	}
	// Uint64 path: handles integers in [2^63, 2^64-1] without float64 rounding.
	ua, errA := strconv.ParseUint(string(na), 10, 64)
	ub, errB := strconv.ParseUint(string(nb), 10, 64)
	if errA == nil && errB == nil {
		return ua == ub
	}
	// Decimal fallback: compare as float64 (precision loss accepted for
	// integers beyond uint64 range and for decimal values like 1.5).
	fa, errA := na.Float64()
	fb, errB := nb.Float64()
	if errA != nil || errB != nil {
		return false
	}
	return fa == fb
}

// normalizeJSONNumber strips trailing fractional zeros so that integer-valued
// decimal notations ("1.0", "1.00") compare equal to their integer form ("1")
// in string and ParseInt checks, without precision loss from float64.
// Scientific notation with a fractional mantissa that evaluates to a whole
// number is further reduced to its integer string form via sciNotationToInt
// so the string-equality fast path in numericEqual applies directly:
// "1.5e2" → "150", "1.23e3" → "1230", "2.0e3" → "2e3", "1.5e0" → "1.5".
// Examples: "1.0" → "1", "1.50" → "1.5", "2.0e3" → "2e3", "1e3" → "1e3".
func normalizeJSONNumber(n json.Number) json.Number {
	s := string(n)
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return n // no decimal point: plain integer or exponent-only scientific notation
	}
	// Locate exponent ('e' or 'E') if present; only strip zeros before it.
	expOff := strings.IndexAny(s[dot:], "eE")
	var exp int
	if expOff >= 0 {
		exp = dot + expOff
	} else {
		exp = len(s)
	}
	end := exp
	for end > dot+1 && s[end-1] == '0' {
		end--
	}
	if end == dot+1 {
		// All zeros after '.': drop the decimal point; keep exponent if present.
		return json.Number(s[:dot] + s[exp:])
	}
	// Non-zero fractional digits remain after stripping trailing zeros.
	// When an exponent is present, the value may still be a whole number
	// (e.g. "1.5e2" = 150). Promote to integer string form so that the
	// string-equality fast path in numericEqual applies without requiring
	// the float64 fallback for this common case.
	if expOff >= 0 {
		if intStr, ok := sciNotationToInt(s[:end] + s[exp:]); ok {
			return json.Number(intStr)
		}
	}
	return json.Number(s[:end] + s[exp:])
}

// sciNotationToInt converts a normalized scientific notation number with a
// non-zero fractional mantissa (e.g. "1.5e2") to its integer string form
// ("150") when the exponent shifts the decimal point past all fractional
// digits. The conversion is done by string arithmetic — no float64 involved —
// so it is exact for any magnitude.
//
// Returns ("", false) when:
//   - the value is not a whole number (exponent < fractional digit count), or
//   - the integer string would exceed 20 digits (beyond uint64 range; the
//     float64 fallback in numericEqual handles those cases).
func sciNotationToInt(s string) (string, bool) {
	eDot := strings.IndexByte(s, '.')
	if eDot < 0 {
		return "", false
	}
	eExp := strings.IndexAny(s, "eE")
	if eExp < 0 {
		return "", false
	}
	expVal, err := strconv.Atoi(s[eExp+1:])
	if err != nil || expVal < 0 {
		return "", false // negative or non-numeric exponent: not an integer via this path
	}
	sign := ""
	mantissa := s[:eExp]
	if strings.HasPrefix(mantissa, "-") {
		sign = "-"
		mantissa = mantissa[1:]
		eDot-- // dot index shifts left when sign is removed
	}
	intPart := mantissa[:eDot]
	fracPart := mantissa[eDot+1:]
	// fracPart has already had trailing zeros stripped by normalizeJSONNumber.
	if expVal < len(fracPart) {
		return "", false // exponent does not cover all fractional digits: not an integer
	}
	zeros := expVal - len(fracPart)
	if len(sign)+len(intPart)+len(fracPart)+zeros > 20 {
		return "", false // result too large for int64/uint64; float64 fallback handles it
	}
	result := strings.TrimLeft(intPart+fracPart+strings.Repeat("0", zeros), "0")
	if result == "" {
		result = "0"
	}
	return sign + result, true
}

// isIntegerNumber reports whether n represents a whole-number (integer) value.
// After stripping trailing fractional zeros, if no decimal point remains the
// value is an integer regardless of magnitude — this accepts integers beyond
// int64 range (e.g. 2^63+1) that the int64 cast path would overflow.
// Scientific notation without a remaining decimal (e.g. "1e3") is also accepted;
// notation that retains a decimal after normalization (e.g. "1.5e2" = 150) is
// checked via math.Trunc.
func isIntegerNumber(n json.Number) bool {
	norm := normalizeJSONNumber(n)
	if !strings.ContainsRune(string(norm), '.') {
		return true // no fractional part: plain integer or integer scientific notation
	}
	// Fractional part remains after normalization; verify via float64.
	f, err := norm.Float64()
	return err == nil && f == math.Trunc(f)
}

// toJSONNumber normalises float64 and json.Number to json.Number.
func toJSONNumber(v any) (json.Number, bool) {
	switch n := v.(type) {
	case json.Number:
		return n, true
	case float64:
		return json.Number(strconv.FormatFloat(n, 'f', -1, 64)), true
	}
	return "", false
}

func formatEnum(enum []any) string {
	parts := make([]string, 0, len(enum))
	for _, e := range enum {
		b, _ := json.Marshal(e)
		parts = append(parts, string(b))
	}
	return strings.Join(parts, ", ")
}
