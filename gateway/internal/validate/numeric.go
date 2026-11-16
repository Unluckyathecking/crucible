package validate

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

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
	if len(intPart)+len(fracPart)+zeros > 20 {
		return "", false // result too large for uint64 range; float64 fallback handles it
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
