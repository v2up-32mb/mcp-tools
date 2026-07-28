package numconv

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"
)

func TestIntFromFloat64RejectsMaxExclusiveBoundary(t *testing.T) {
	if _, err := IntFromFloat64(math.Ldexp(1, 63), "value"); err == nil {
		t.Fatal("expected 2^63 to be rejected as out of range")
	}
}

func TestIntFromFloat64RejectsFractionalValues(t *testing.T) {
	if _, err := IntFromFloat64(1.25, "value"); err == nil {
		t.Fatal("expected fractional value to be rejected")
	}
}

func TestIntFromFloat64OKOmitsInvalidValues(t *testing.T) {
	if _, ok := IntFromFloat64OK(math.Inf(1)); ok {
		t.Fatal("expected Inf to be rejected")
	}
	if got, ok := IntFromFloat64OK(42); !ok || got != 42 {
		t.Fatalf("expected 42 to parse, got %d ok=%t", got, ok)
	}
}

func TestIntFromJSONNumberRejectsFractionWithoutFloatRounding(t *testing.T) {
	if _, err := IntFromJSONNumber(json.Number("9007199254740992.5"), "value"); err == nil {
		t.Fatal("expected fractional JSON number to be rejected")
	}
}

func TestIntFromJSONNumberRejectsFractionalExponent(t *testing.T) {
	if _, err := IntFromJSONNumber(json.Number("1e-3"), "value"); err == nil {
		t.Fatal("expected fractional exponent to be rejected")
	}
}

func TestIntFromJSONNumberRejectsExponentOutOfRange(t *testing.T) {
	if _, err := IntFromJSONNumber(json.Number("1e1000"), "value"); err == nil {
		t.Fatal("expected huge exponent to be rejected as out of range")
	}
}

func TestIntFromJSONNumberAcceptsExactIntegerNotation(t *testing.T) {
	for raw, want := range map[string]int{
		"42.0": 42,
		"1e3":  1000,
	} {
		t.Run(raw, func(t *testing.T) {
			got, err := IntFromJSONNumber(json.Number(raw), "value")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != want {
				t.Fatalf("unexpected parsed value: got %d want %d", got, want)
			}
		})
	}
}

func TestIntFromJSONNumberParsesLargeExactInteger(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("large integer exactness check requires 64-bit int")
	}
	got, err := IntFromJSONNumber(json.Number("9007199254740993"), "value")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if int64(got) != 9007199254740993 {
		t.Fatalf("unexpected parsed value: %d", got)
	}
}

func TestIntFromFloat64AcceptsMinInclusiveBoundary(t *testing.T) {
	// The lower bound is -2^63 (inclusive), so it should be accepted.
	got, err := IntFromFloat64(-math.Ldexp(1, 63), "value")
	if err != nil {
		t.Fatalf("expected -2^63 to be accepted as the inclusive lower bound, got error: %v", err)
	}
	if got >= 0 {
		t.Fatalf("expected a negative value, got %d", got)
	}
}

func TestIntFromFloat64RejectsNaN(t *testing.T) {
	if _, err := IntFromFloat64(math.NaN(), "value"); err == nil {
		t.Fatal("expected NaN to be rejected")
	}
}

func TestIntFromFloat64RejectsNegativeInfinity(t *testing.T) {
	if _, err := IntFromFloat64(math.Inf(-1), "value"); err == nil {
		t.Fatal("expected -Inf to be rejected")
	}
}

func TestIntFromFloat64AcceptsZero(t *testing.T) {
	got, err := IntFromFloat64(0, "value")
	if err != nil {
		t.Fatalf("unexpected error for zero: %v", err)
	}
	if got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}
}

func TestIntFromFloat64AcceptsNegativeInteger(t *testing.T) {
	got, err := IntFromFloat64(-42, "value")
	if err != nil {
		t.Fatalf("unexpected error for -42: %v", err)
	}
	if got != -42 {
		t.Fatalf("expected -42, got %d", got)
	}
}

func TestIntFromFloat64OKRejectsNaN(t *testing.T) {
	if _, ok := IntFromFloat64OK(math.NaN()); ok {
		t.Fatal("expected NaN to be rejected")
	}
}

func TestIntFromFloat64OKRejectsNegativeInf(t *testing.T) {
	if _, ok := IntFromFloat64OK(math.Inf(-1)); ok {
		t.Fatal("expected -Inf to be rejected")
	}
}

func TestIntFromJSONNumberAcceptsNegativeInteger(t *testing.T) {
	got, err := IntFromJSONNumber(json.Number("-42"), "value")
	if err != nil {
		t.Fatalf("unexpected error for -42: %v", err)
	}
	if got != -42 {
		t.Fatalf("expected -42, got %d", got)
	}
}

func TestIntFromJSONNumberAcceptsZero(t *testing.T) {
	got, err := IntFromJSONNumber(json.Number("0"), "value")
	if err != nil {
		t.Fatalf("unexpected error for 0: %v", err)
	}
	if got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}
}

func TestIntFromJSONNumberRejectsLeadingZeros(t *testing.T) {
	// The implementation rejects leading zeros to prevent ambiguity.
	if _, err := IntFromJSONNumber(json.Number("007"), "value"); err == nil {
		t.Fatal("expected leading-zero number to be rejected")
	}
}

func TestIntFromJSONNumberRejectsEmptyString(t *testing.T) {
	if _, err := IntFromJSONNumber(json.Number(""), "value"); err == nil {
		t.Fatal("expected empty string to be rejected")
	}
}

func TestIntFromJSONNumberRejectsNonNumeric(t *testing.T) {
	if _, err := IntFromJSONNumber(json.Number("abc"), "value"); err == nil {
		t.Fatal("expected non-numeric string to be rejected")
	}
}

func TestIntFromJSONNumberOKRejectsInvalid(t *testing.T) {
	if _, ok := IntFromJSONNumberOK(json.Number("abc")); ok {
		t.Fatal("expected non-numeric to be rejected")
	}
	if _, ok := IntFromJSONNumberOK(json.Number("")); ok {
		t.Fatal("expected empty string to be rejected")
	}
}

func TestIntFromJSONNumberOKAcceptsValidInteger(t *testing.T) {
	got, ok := IntFromJSONNumberOK(json.Number("42"))
	if !ok || got != 42 {
		t.Fatalf("expected 42 to parse, got %d ok=%t", got, ok)
	}
}
