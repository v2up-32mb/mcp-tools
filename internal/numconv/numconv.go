package numconv

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// IntFromFloat64 converts JSON-style float64 numbers to int only when the value
// is integral and safely representable by the platform int type.
func IntFromFloat64(value float64, name string) (int, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	minInclusive, maxExclusive := intFloatBounds()
	if value < minInclusive || value >= maxExclusive {
		return 0, fmt.Errorf("%s is out of range", name)
	}
	return int(value), nil
}

// IntFromFloat64OK is the predicate form of IntFromFloat64 for best-effort
// logging paths that should omit invalid fields instead of returning errors.
func IntFromFloat64OK(value float64) (int, bool) {
	parsed, err := IntFromFloat64(value, "value")
	return parsed, err == nil
}

// IntFromJSONNumber converts a JSON number token to int without going through
// float64, so fractional or out-of-range values cannot be accepted after
// precision loss.
func IntFromJSONNumber(value json.Number, name string) (int, error) {
	normalized, outOfRange, ok := normalizeJSONInteger(value.String())
	if outOfRange {
		return 0, fmt.Errorf("%s is out of range", name)
	}
	if !ok {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	parsed, err := strconv.ParseInt(normalized, 10, strconv.IntSize)
	if err == nil {
		return int(parsed), nil
	}
	if numErr, ok := err.(*strconv.NumError); ok && numErr.Err == strconv.ErrRange {
		return 0, fmt.Errorf("%s is out of range", name)
	}
	return 0, fmt.Errorf("%s must be an integer", name)
}

// IntFromJSONNumberOK is the predicate form of IntFromJSONNumber for
// best-effort logging paths that should omit invalid fields.
func IntFromJSONNumberOK(value json.Number) (int, bool) {
	parsed, err := IntFromJSONNumber(value, "value")
	return parsed, err == nil
}

func intFloatBounds() (float64, float64) {
	maxExclusive := float64(uint64(1) << (strconv.IntSize - 1))
	return -maxExclusive, maxExclusive
}

func normalizeJSONInteger(token string) (normalized string, outOfRange bool, ok bool) {
	if token == "" {
		return "", false, false
	}
	i := 0
	negative := false
	if token[i] == '-' {
		negative = true
		i++
		if i == len(token) {
			return "", false, false
		}
	}

	intStart := i
	switch {
	case token[i] == '0':
		i++
		if i < len(token) && isDigit(token[i]) {
			return "", false, false
		}
	case token[i] >= '1' && token[i] <= '9':
		for i < len(token) && isDigit(token[i]) {
			i++
		}
	default:
		return "", false, false
	}
	intDigits := token[intStart:i]

	fracDigits := ""
	if i < len(token) && token[i] == '.' {
		i++
		fracStart := i
		for i < len(token) && isDigit(token[i]) {
			i++
		}
		if i == fracStart {
			return "", false, false
		}
		fracDigits = token[fracStart:i]
	}

	exp := 0
	if i < len(token) && (token[i] == 'e' || token[i] == 'E') {
		i++
		expSign := 1
		if i < len(token) && (token[i] == '+' || token[i] == '-') {
			if token[i] == '-' {
				expSign = -1
			}
			i++
		}
		if i == len(token) || !isDigit(token[i]) {
			return "", false, false
		}
		// expCap is an upper bound chosen so that any exponent beyond it
		// would push the result past maxPlatformIntDigits regardless of
		// digit count. We cap exp at expCap+1 so subsequent digits are
		// ignored, and the final range check on len(digits)+scale catches
		// any remaining edge cases.
		expCap := len(intDigits) + len(fracDigits) + maxPlatformIntDigits() + 1
		for i < len(token) && isDigit(token[i]) {
			if exp <= expCap {
				exp = exp*10 + int(token[i]-'0')
				if exp > expCap {
					exp = expCap + 1
				}
			}
			i++
		}
		exp *= expSign
	}
	if i != len(token) {
		return "", false, false
	}

	digits := strings.TrimLeft(intDigits+fracDigits, "0")
	if digits == "" {
		return "0", false, true
	}

	scale := exp - len(fracDigits)
	if scale >= 0 {
		if len(digits)+scale > maxPlatformIntDigits() {
			return "", true, true
		}
		normalized = digits + strings.Repeat("0", scale)
	} else {
		cut := -scale
		if cut > len(digits) || !allZeros(digits[len(digits)-cut:]) {
			return "", false, false
		}
		normalized = strings.TrimLeft(digits[:len(digits)-cut], "0")
		if normalized == "" {
			return "0", false, true
		}
	}
	if len(normalized) > maxPlatformIntDigits() {
		return "", true, true
	}
	if negative {
		normalized = "-" + normalized
	}
	return normalized, false, true
}

func maxPlatformIntDigits() int {
	return len(strconv.FormatInt(int64(int(^uint(0)>>1)), 10))
}

func isDigit(ch byte) bool {
	return ch >= '0' && ch <= '9'
}

func allZeros(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}
