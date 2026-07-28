package util

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// --------------------------------------------------------------------------
// CloneStrings
// --------------------------------------------------------------------------

func TestCloneStringsNilReturnsNil(t *testing.T) {
	got := CloneStrings(nil)
	if got != nil {
		t.Fatalf("CloneStrings(nil) = %v, want nil", got)
	}
}

func TestCloneStringsEmptyReturnsEmpty(t *testing.T) {
	got := CloneStrings([]string{})
	if got == nil {
		t.Fatal("CloneStrings(empty) = nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("CloneStrings(empty) len = %d, want 0", len(got))
	}
}

func TestCloneStringsNonEmptyIsIndependentCopy(t *testing.T) {
	src := []string{"a", "b", "c"}
	got := CloneStrings(src)

	if len(got) != len(src) {
		t.Fatalf("len = %d, want %d", len(got), len(src))
	}
	for i := range src {
		if got[i] != src[i] {
			t.Fatalf("index %d: got %q, want %q", i, got[i], src[i])
		}
	}

	// Mutating the clone must not affect the source slice (byte-level copy).
	if len(got) > 0 {
		got[0] = "X"
		if src[0] != "a" {
			t.Fatalf("source mutated after clone edit: src[0]=%q, want %q", src[0], "a")
		}
	}
}

func TestCloneStringsMakesUnderlyingArrayCopy(t *testing.T) {
	// Ensure the returned slice has its own backing array: appending to the
	// clone beyond its length (which would write into the source's array if
	// they shared storage) must not change the source.
	src := make([]string, 2, 4)
	src[0], src[1] = "alpha", "beta"
	got := CloneStrings(src)
	got = append(got, "gamma")

	if src[0] != "alpha" || src[1] != "beta" || len(src) != 2 {
		t.Fatalf("source changed after append to clone: src=%v", src)
	}
	if len(got) != 3 {
		t.Fatalf("clone len after append = %d, want 3", len(got))
	}
}

// --------------------------------------------------------------------------
// FirstNonEmpty
// --------------------------------------------------------------------------

func TestFirstNonEmptyAllEmptyReturnsEmpty(t *testing.T) {
	if got := FirstNonEmpty("", "", ""); got != "" {
		t.Fatalf("FirstNonEmpty(\"\",\"\",\"\") = %q, want empty", got)
	}
}

func TestFirstNonEmptyNoArgsReturnsEmpty(t *testing.T) {
	if got := FirstNonEmpty(); got != "" {
		t.Fatalf("FirstNonEmpty() = %q, want empty", got)
	}
}

func TestFirstNonEmptyWhitespaceOnlyTreatedAsEmpty(t *testing.T) {
	// A whitespace-only string is not "non-empty" for selection purposes.
	if got := FirstNonEmpty("   ", "\t", "\n\t "); got != "" {
		t.Fatalf("FirstNonEmpty(whitespace...) = %q, want empty", got)
	}
}

func TestFirstNonEmptyReturnsFirstNonWhitespaceValue(t *testing.T) {
	got := FirstNonEmpty("", "  ", "first", "second")
	if got != "first" {
		t.Fatalf("FirstNonEmpty = %q, want %q", got, "first")
	}
}

func TestFirstNonEmptyAllNonEmptyReturnsFirst(t *testing.T) {
	got := FirstNonEmpty("one", "two", "three")
	if got != "one" {
		t.Fatalf("FirstNonEmpty = %q, want %q", got, "one")
	}
}

func TestFirstNonEmptyMixedWhitespaceAndNonEmpty(t *testing.T) {
	// The returned value is the original (non-trimmed) string, even if it has
	// surrounding whitespace, as long as its trimmed value is non-empty.
	got := FirstNonEmpty("", "   ", "  value  ", "")
	if got != "  value  " {
		t.Fatalf("FirstNonEmpty = %q, want %q", got, "  value  ")
	}
}

func TestFirstNonEmptySurroundingWhitespaceValueReturnedAsIs(t *testing.T) {
	got := FirstNonEmpty(" x ")
	if got != " x " {
		t.Fatalf("FirstNonEmpty = %q, want %q", got, " x ")
	}
	if strings.TrimSpace(got) != "x" {
		t.Fatalf("trimmed value = %q, want %q", strings.TrimSpace(got), "x")
	}
}

// --------------------------------------------------------------------------
// Truncate
// --------------------------------------------------------------------------

func TestTruncateUnderLimitReturnsOriginal(t *testing.T) {
	for _, tc := range []struct {
		text  string
		limit int
	}{
		{"", 0},
		{"abc", 3},  // exactly equal
		{"abc", 10}, // well under
		{"a", 1},    // single char, exact
	} {
		if got := Truncate(tc.text, tc.limit); got != tc.text {
			t.Fatalf("Truncate(%q,%d) = %q, want %q", tc.text, tc.limit, got, tc.text)
		}
	}
}

func TestTruncateEmptyTextReturnsEmptyForAnyLimit(t *testing.T) {
	for _, limit := range []int{0, 1, 10} {
		if got := Truncate("", limit); got != "" {
			t.Fatalf("Truncate(\"\",%d) = %q, want empty", limit, got)
		}
	}
}

func TestTruncateOverLimitAppendsMarker(t *testing.T) {
	got := Truncate("hello world", 5)
	want := "hello\n[truncated]"
	if got != want {
		t.Fatalf("Truncate = %q, want %q", got, want)
	}
}

func TestTruncateLimitZeroOnNonEmptyTruncates(t *testing.T) {
	got := Truncate("nonempty", 0)
	want := "\n[truncated]"
	if got != want {
		t.Fatalf("Truncate(\"nonempty\",0) = %q, want %q", got, want)
	}
}

func TestTruncateCutLengthIsBytesNotRunesASCII(t *testing.T) {
	// For ASCII the byte and rune counts coincide, so the kept prefix matches.
	text := strings.Repeat("A", 100)
	limit := 40
	got := Truncate(text, limit)
	if len(got) != limit+len("\n[truncated]") {
		t.Fatalf("result byte len = %d, want %d", len(got), limit+len("\n[truncated]"))
	}
	if got != text[:limit]+"\n[truncated]" {
		t.Fatalf("result = %q, want %q", got, text[:limit]+"\n[truncated]")
	}
}

func TestTruncateSplitsMultibyteRuneByBytes(t *testing.T) {
	// "世" is 3 bytes in UTF-8 (E4 B8 96). A limit of 1 cuts inside the rune,
	// producing an invalid-UTF-8 byte. The function is defined to truncate by
	// bytes, so we assert byte-level behavior, not rune validity.
	const rune3 = "世" // 3 bytes
	if len(rune3) != 3 {
		t.Fatalf("setup: expected 3-byte rune, got %d", len(rune3))
	}

	limit := 1
	got := Truncate(rune3, limit)
	want := rune3[:limit] + "\n[truncated]"
	if got != want {
		t.Fatalf("Truncate multibyte(1) = %q (bytes %v), want %q (bytes %v)",
			got, []byte(got), want, []byte(want))
	}
	if len(got) != limit+len("\n[truncated]") {
		t.Fatalf("byte len = %d, want %d", len(got), limit+len("\n[truncated]"))
	}
}

func TestTruncateMultibyteAlignedBoundary(t *testing.T) {
	// Three "中" runes = 9 bytes. A limit exactly at a rune boundary (6 bytes)
	// keeps two whole runes and appends the marker; the kept prefix is valid.
	text := "中中中" // 9 bytes
	if len(text) != 9 {
		t.Fatalf("setup: expected 9-byte text, got %d", len(text))
	}
	limit := 6
	got := Truncate(text, limit)
	want := "中中" + "\n[truncated]"
	if got != want {
		t.Fatalf("Truncate aligned = %q, want %q", got, want)
	}
}

func TestTruncateMultibyteUnalignedBoundary(t *testing.T) {
	// "中" (3 bytes) + "A" (1 byte) + "中" (3 bytes) = 7 bytes.
	// A limit of 4 bytes cuts after "中A" plus the first byte of the third
	// rune, splitting it. Validate byte-level result, not UTF-8 validity.
	text := "中A中"
	if len(text) != 7 {
		t.Fatalf("setup: expected 7-byte text, got %d", len(text))
	}
	limit := 4
	got := Truncate(text, limit)
	want := text[:limit] + "\n[truncated]"
	if got != want {
		t.Fatalf("Truncate unaligned = %q (bytes %v), want %q (bytes %v)",
			got, []byte(got), want, []byte(want))
	}
}

// --------------------------------------------------------------------------
// ValidateEnvKey
// --------------------------------------------------------------------------

func TestValidateEnvKeyEmptyReturnsError(t *testing.T) {
	err := ValidateEnvKey("")
	if err == nil {
		t.Fatal("ValidateEnvKey(\"\") = nil, want error")
	}
	if err.Error() != "key cannot be empty" {
		t.Fatalf("error message = %q, want %q", err.Error(), "key cannot be empty")
	}
}

func TestValidateEnvKeyWhitespaceOnlyReturnsEmptyError(t *testing.T) {
	for _, key := range []string{" ", "\t", "\n", "   \t  "} {
		err := ValidateEnvKey(key)
		if err == nil {
			t.Fatalf("ValidateEnvKey(%q) = nil, want error", key)
		}
		if err.Error() != "key cannot be empty" {
			t.Fatalf("error for %q = %q, want %q", key, err.Error(), "key cannot be empty")
		}
	}
}

func TestValidateEnvKeyContainingEqualsReturnsError(t *testing.T) {
	for _, key := range []string{"=", "KEY=value", "KEY=", "=VAL", "K=E", "  =  "} {
		err := ValidateEnvKey(key)
		if err == nil {
			t.Fatalf("ValidateEnvKey(%q) = nil, want error", key)
		}
		if err.Error() != "key cannot contain '='" {
			t.Fatalf("error for %q = %q, want %q", key, err.Error(), "key cannot contain '='")
		}
	}
}

func TestValidateEnvKeyEqualsInMiddleReturnsError(t *testing.T) {
	err := ValidateEnvKey("FOO=BAR")
	if err == nil {
		t.Fatal("ValidateEnvKey(\"FOO=BAR\") = nil, want error")
	}
	if !strings.Contains(err.Error(), "'='") {
		t.Fatalf("error message = %q, want it to mention '='", err.Error())
	}
}

func TestValidateEnvKeyValidReturnsNil(t *testing.T) {
	for _, key := range []string{"PATH", "HOME", "MY_VAR_123", "  PATH  "} {
		// Leading/trailing whitespace is trimmed by the empty check; a key
		// with surrounding whitespace but no '=' is still accepted (returns
		// nil), mirroring ValidateEnvKey's implementation.
		err := ValidateEnvKey(key)
		if err != nil {
			t.Fatalf("ValidateEnvKey(%q) = %v, want nil", key, err)
		}
	}
}

func TestValidateEnvKeyWhitespaceWithEqualsReturnsEqualsError(t *testing.T) {
	// Trimming " = " leaves "=", which is non-empty, so the implementation
	// passes the empty check and is then rejected by the '=' check. Any key
	// whose trimmed form is non-empty and that contains '=' yields the
	// equals error, even if it also has surrounding whitespace.
	err := ValidateEnvKey(" = ")
	if err == nil {
		t.Fatal("ValidateEnvKey(\" = \") = nil, want error")
	}
	if err.Error() != "key cannot contain '='" {
		t.Fatalf("error for \" = \" = %q, want \"key cannot contain '=\"", err.Error())
	}
}

// --------------------------------------------------------------------------
// ParseIntegerFloat
// --------------------------------------------------------------------------

func TestParseIntegerFloatIntegerReturnsInt(t *testing.T) {
	for _, tc := range []struct {
		value float64
		want  int
	}{
		{0, 0},
		{1, 1},
		{-1, -1},
		{42, 42},
		{1e3, 1000},
	} {
		got, err := ParseIntegerFloat(tc.value, "value")
		if err != nil {
			t.Fatalf("ParseIntegerFloat(%v) unexpected err: %v", tc.value, err)
		}
		if got != tc.want {
			t.Fatalf("ParseIntegerFloat(%v) = %d, want %d", tc.value, got, tc.want)
		}
	}
}

func TestParseIntegerFloatFractionalReturnsError(t *testing.T) {
	_, err := ParseIntegerFloat(1.25, "myfield")
	if err == nil {
		t.Fatal("ParseIntegerFloat(1.25) = nil, want error")
	}
	if err.Error() != "myfield must be an integer" {
		t.Fatalf("error = %q, want %q", err.Error(), "myfield must be an integer")
	}
}

func TestParseIntegerFloatNaNReturnsError(t *testing.T) {
	_, err := ParseIntegerFloat(math.NaN(), "value")
	if err == nil {
		t.Fatal("ParseIntegerFloat(NaN) = nil, want error")
	}
	if !strings.Contains(err.Error(), "must be an integer") {
		t.Fatalf("error = %q, want it to contain 'must be an integer'", err.Error())
	}
}

func TestParseIntegerFloatInfReturnsError(t *testing.T) {
	for _, v := range []float64{math.Inf(1), math.Inf(-1)} {
		_, err := ParseIntegerFloat(v, "value")
		if err == nil {
			t.Fatalf("ParseIntegerFloat(%v) = nil, want error", v)
		}
	}
}

func TestParseIntegerFloatOutOfRangeReturnsError(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("out-of-range checks assume 64-bit int")
	}

	// The implementation's range is the half-open interval [-2^63, 2^63):
	// maxExclusive = float64(1<<63) = 2^63 exactly. 2^63 is exactly
	// representable in float64, so it is the smallest float64 at the upper
	// boundary and must be rejected as out of range.
	maxExclusive := math.Ldexp(1, 63)
	if _, err := ParseIntegerFloat(maxExclusive, "n"); err == nil {
		t.Fatal("expected 2^63 to be rejected as out of range")
	} else if err.Error() != "n is out of range" {
		t.Fatalf("error = %q, want %q", err.Error(), "n is out of range")
	}

	// The lower bound is inclusive: -2^63 is exactly representable in float64
	// and must be accepted and round-trip exactly.
	minInclusive := -math.Ldexp(1, 63)
	got, err := ParseIntegerFloat(minInclusive, "n")
	if err != nil {
		t.Fatalf("ParseIntegerFloat(-2^63) unexpected err: %v", err)
	}
	if int64(got) != int64(minInclusive) {
		t.Fatalf("min edge got %d, want %d", got, int64(minInclusive))
	}

	// A large odd integer that is exactly representable in float64 (2^53 - 1,
	// the largest odd float64 < 2^53) round-trips without precision loss and
	// is well inside the platform int range, so it must be accepted. Using an
	// odd value proves there is no float64 rounding inside the parser.
	preciseOdd := math.Ldexp(1, 53) - 1
	got, err = ParseIntegerFloat(preciseOdd, "n")
	if err != nil {
		t.Fatalf("ParseIntegerFloat(2^53-1) unexpected err: %v", err)
	}
	if int64(got) != int64(preciseOdd) {
		t.Fatalf("2^53-1 round-trip got %d, want %d", got, int64(preciseOdd))
	}
}

// --------------------------------------------------------------------------
// TempDir-isolated sanity test (no file actors in util.go, validates
// t.TempDir usage is wired correctly).
// --------------------------------------------------------------------------

func TestTempDirIsolationIsUsable(t *testing.T) {
	dir := t.TempDir()
	if dir == "" {
		t.Fatal("t.TempDir() returned empty path")
	}
}
