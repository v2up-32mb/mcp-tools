package util

import (
	"errors"
	"strings"

	"github.com/example/mcp-tools/internal/numconv"
)

// CloneStrings returns a copy of values, or nil if values is nil.
func CloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

// FirstNonEmpty returns the first string whose trimmed value is non-empty.
func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// Truncate cuts text to limit bytes and appends "[truncated]" if needed.
func Truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "\n[truncated]"
}

// ValidateEnvKey checks an environment key is non-empty and doesn't contain '='.
func ValidateEnvKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("key cannot be empty")
	}
	if strings.Contains(key, "=") {
		return errors.New("key cannot contain '='")
	}
	return nil
}

// ParseIntegerFloat converts a JSON-style float64 to int safely.
func ParseIntegerFloat(value float64, name string) (int, error) {
	return numconv.IntFromFloat64(value, name)
}
