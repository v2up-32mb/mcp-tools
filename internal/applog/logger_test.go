package applog

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestLoggerFiltersBelowMinLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, "INFO")
	logger.now = func() time.Time { return time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC) }

	logger.Debug("http.debug", "request received", Field{Key: "path", Value: "/mcp"})
	if buf.Len() != 0 {
		t.Fatalf("expected debug log to be filtered, got %q", buf.String())
	}
}

func TestLoggerFormatsBlockWithOrderedFields(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, "INFO")
	logger.now = func() time.Time { return time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC) }

	logger.Info("mcp.tool", "tool call completed",
		Field{Key: "tool", Value: "fs.apply_unified_diff"},
		Field{Key: "target_path", Value: "/tmp/demo.go"},
		Field{Key: "diff_hunks", Value: 3},
		Field{Key: "duration_ms", Value: 12},
	)

	got := buf.String()
	if !strings.Contains(got, "[2026-04-30 12:00:00] INFO  mcp.tool") {
		t.Fatalf("missing header: %q", got)
	}
	if !strings.Contains(got, "tool call completed") {
		t.Fatalf("missing message: %q", got)
	}
	order := []string{
		"\n  tool: fs.apply_unified_diff",
		"\n  target_path: /tmp/demo.go",
		"\n  diff_hunks: 3",
		"\n  duration_ms: 12",
	}
	last := -1
	for _, needle := range order {
		idx := strings.Index(got, needle)
		if idx < 0 {
			t.Fatalf("missing field %q in %q", needle, got)
		}
		if idx <= last {
			t.Fatalf("field order incorrect in %q", got)
		}
		last = idx
	}
}

func TestLoggerFormatsErrorBlock(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, "ERROR")
	logger.now = func() time.Time { return time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC) }

	logger.Error("mcp.tool", "tool call failed",
		Field{Key: "tool", Value: "exec.run"},
		Field{Key: "request_id", Value: "req-1"},
		Field{Key: "session_id", Value: "sess-1"},
		Field{Key: "error", Value: "exec go_test failed"},
	)

	got := buf.String()
	for _, needle := range []string{
		"[2026-04-30 12:00:00] ERROR  mcp.tool",
		"tool call failed",
		"request_id: req-1",
		"session_id: sess-1",
		"error: exec go_test failed",
	} {
		if !strings.Contains(got, needle) {
			t.Fatalf("missing %q in %q", needle, got)
		}
	}
}
