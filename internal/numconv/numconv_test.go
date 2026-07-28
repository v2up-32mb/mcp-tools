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
