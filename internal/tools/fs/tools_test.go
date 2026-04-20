package fs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
)

func newTestConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	return config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		AuditLogPath:     filepath.Join(root, "audit.jsonl"),
		OutputMaxBytes:   1 << 16,
		CommandTimeout:   5 * time.Second,
	}
}

func findTool(t *testing.T, cfg config.Config, name string) mcp.Tool {
	t.Helper()
	for _, tool := range NewTools(cfg) {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("tool %s not found", name)
	return nil
}

func TestEditLinesExpandsWithoutShiftingFollowingContent(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.edit_lines")
	target := filepath.Join(cfg.StartupDirectory, "expand.txt")
	if err := os.WriteFile(target, []byte("a\nb\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":       "expand.txt",
		"start_line": 2,
		"end_line":   2,
		"new_text":   "B1\nB2\n",
	})
	if err != nil {
		t.Fatalf("edit_lines failed: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	want := "a\nB1\nB2\nc\nd\n"
	if string(got) != want {
		t.Fatalf("unexpected file contents\nwant: %q\ngot:  %q", want, string(got))
	}
}

func TestEditLinesShrinksWithoutShiftingFollowingContent(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.edit_lines")
	target := filepath.Join(cfg.StartupDirectory, "shrink.txt")
	if err := os.WriteFile(target, []byte("a\nb\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":       "shrink.txt",
		"start_line": 2,
		"end_line":   3,
		"new_text":   "BC\n",
	})
	if err != nil {
		t.Fatalf("edit_lines failed: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	want := "a\nBC\nd\n"
	if string(got) != want {
		t.Fatalf("unexpected file contents\nwant: %q\ngot:  %q", want, string(got))
	}
}

func TestEditLinesSupportsReplacementWithoutTrailingNewline(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.edit_lines")
	target := filepath.Join(cfg.StartupDirectory, "no-trailing-newline.txt")
	if err := os.WriteFile(target, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":       "no-trailing-newline.txt",
		"start_line": 2,
		"end_line":   2,
		"new_text":   "TWO",
	})
	if err != nil {
		t.Fatalf("edit_lines failed: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	want := "one\nTWO\nthree\n"
	if string(got) != want {
		t.Fatalf("unexpected file contents\nwant: %q\ngot:  %q", want, string(got))
	}
}

func TestEditLinesExpectedOldTextWithDifferentLineCountsStillMatchesExactRange(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.edit_lines")
	target := filepath.Join(cfg.StartupDirectory, "expected.txt")
	if err := os.WriteFile(target, []byte("top\nold1\nold2\nbottom\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":              "expected.txt",
		"start_line":        2,
		"end_line":          3,
		"expected_old_text": "old1\nold2\n",
		"new_text":          "new-only\n",
	})
	if err != nil {
		t.Fatalf("edit_lines failed: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	want := "top\nnew-only\nbottom\n"
	if string(got) != want {
		t.Fatalf("unexpected file contents\nwant: %q\ngot:  %q", want, string(got))
	}
}

func TestEditLinesReturnsMismatchWhenExpectedOldTextWrong(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.edit_lines")
	target := filepath.Join(cfg.StartupDirectory, "mismatch.txt")
	if err := os.WriteFile(target, []byte("x\ny\nz\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":              "mismatch.txt",
		"start_line":        2,
		"end_line":          2,
		"expected_old_text": "wrong\n",
		"new_text":          "Y\nQ\n",
	})
	if err == nil {
		t.Fatal("expected mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "expected_old_text mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}

	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "x\ny\nz\n" {
		t.Fatalf("file should remain unchanged, got %q", string(got))
	}
}

func TestEditLinesNormalizesMultiLineReplacementWithoutTrailingNewline(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.edit_lines")
	target := filepath.Join(cfg.StartupDirectory, "normalize-multi.txt")
	if err := os.WriteFile(target, []byte("a\nb\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":       "normalize-multi.txt",
		"start_line": 2,
		"end_line":   3,
		"new_text":   "X\nY",
	})
	if err != nil {
		t.Fatalf("edit_lines failed: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	want := "a\nX\nY\nd\n"
	if string(got) != want {
		t.Fatalf("unexpected file contents\nwant: %q\ngot:  %q", want, string(got))
	}
}

func TestSearchTextSupportsLongLines(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.search_text")
	target := filepath.Join(cfg.StartupDirectory, "long-line.txt")
	payload := bytes.Repeat([]byte("a"), 70*1024)
	payload = append(payload, []byte("needle\nshort line\n")...)
	if err := os.WriteFile(target, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":  "long-line.txt",
		"query": "needle",
	})
	if err != nil {
		t.Fatalf("search_text failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %#v", result)
	}
	if !strings.Contains(result.Content[0].Text, "needle") {
		t.Fatalf("expected result text to contain needle, got %q", result.Content[0].Text)
	}
	matches, ok := result.StructuredContent["matches"].([]map[string]any)
	if ok && len(matches) == 0 {
		t.Fatal("expected at least one match")
	}
}

func TestReplaceTextReplacesFirstOccurrenceByDefault(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.replace_text")
	target := filepath.Join(cfg.StartupDirectory, "replace-first.txt")
	if err := os.WriteFile(target, []byte("hello foo world foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":     "replace-first.txt",
		"old_text": "foo",
		"new_text": "bar",
	})
	if err != nil {
		t.Fatalf("replace_text failed: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	want := "hello bar world foo\n"
	if string(got) != want {
		t.Fatalf("unexpected file contents\nwant: %q\ngot:  %q", want, string(got))
	}
}

func TestReplaceTextReplacesAllOccurrences(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.replace_text")
	target := filepath.Join(cfg.StartupDirectory, "replace-all.txt")
	if err := os.WriteFile(target, []byte("a foo b foo c\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":        "replace-all.txt",
		"old_text":    "foo",
		"new_text":    "bar",
		"replace_all": true,
	})
	if err != nil {
		t.Fatalf("replace_text failed: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	want := "a bar b bar c\n"
	if string(got) != want {
		t.Fatalf("unexpected file contents\nwant: %q\ngot:  %q", want, string(got))
	}
}

func TestReplaceTextRejectsExpectedReplacementMismatch(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.replace_text")
	target := filepath.Join(cfg.StartupDirectory, "replace-mismatch.txt")
	if err := os.WriteFile(target, []byte("foo foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":                  "replace-mismatch.txt",
		"old_text":              "foo",
		"new_text":              "bar",
		"expected_replacements": 1,
	})
	if err == nil {
		t.Fatal("expected replacement mismatch error")
	}
	if !strings.Contains(err.Error(), "expected_replacements mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyUnifiedDiffSupportsMultipleHunks(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.apply_unified_diff")
	target := filepath.Join(cfg.StartupDirectory, "multi.diff.txt")
	if err := os.WriteFile(target, []byte("alpha\nbeta\ngamma\ndelta\nepsilon\nzeta\neta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := strings.Join([]string{
		"--- a/multi.diff.txt",
		"+++ b/multi.diff.txt",
		"@@ -1,3 +1,3 @@",
		" alpha",
		"-beta",
		"+beta2",
		" gamma",
		"@@ -5,3 +5,4 @@",
		" epsilon",
		"-zeta",
		"+zeta2",
		" eta",
		"+theta",
		"",
	}, "\n")

	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path": "multi.diff.txt",
		"diff": diff,
	})
	if err != nil {
		t.Fatalf("apply_unified_diff failed: %v", err)
	}
	if got := res.StructuredContent["hunks_applied"]; got != 2 {
		t.Fatalf("expected 2 hunks_applied, got %#v", got)
	}
	if got := res.StructuredContent["dry_run"]; got != false {
		t.Fatalf("expected dry_run=false, got %#v", got)
	}

	payload, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	want := "alpha\nbeta2\ngamma\ndelta\nepsilon\nzeta2\neta\ntheta\n"
	if string(payload) != want {
		t.Fatalf("unexpected file contents\nwant: %q\ngot:  %q", want, string(payload))
	}
}

func TestApplyUnifiedDiffDryRunLeavesFileUnchanged(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.apply_unified_diff")
	target := filepath.Join(cfg.StartupDirectory, "dry-run.txt")
	original := "one\ntwo\nthree\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := strings.Join([]string{
		"--- a/dry-run.txt",
		"+++ b/dry-run.txt",
		"@@ -1,3 +1,3 @@",
		" one",
		"-two",
		"+TWO",
		" three",
		"",
	}, "\n")

	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":    "dry-run.txt",
		"diff":    diff,
		"dry_run": true,
	})
	if err != nil {
		t.Fatalf("apply_unified_diff dry_run failed: %v", err)
	}
	if got := res.StructuredContent["dry_run"]; got != true {
		t.Fatalf("expected dry_run=true, got %#v", got)
	}
	if got := res.StructuredContent["bytes_written"]; got != 0 {
		t.Fatalf("expected bytes_written=0 for dry_run, got %#v", got)
	}

	payload, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != original {
		t.Fatalf("dry_run should not modify file\nwant: %q\ngot:  %q", original, string(payload))
	}
}

func TestApplyUnifiedDiffReturnsStructuredConflict(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.apply_unified_diff")
	target := filepath.Join(cfg.StartupDirectory, "conflict.txt")
	original := "one\ntwo\nthree\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := strings.Join([]string{
		"--- a/conflict.txt",
		"+++ b/conflict.txt",
		"@@ -1,3 +1,3 @@",
		" one",
		"-TWO",
		"+two2",
		" three",
		"",
	}, "\n")

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path": "conflict.txt",
		"diff": diff,
	})
	if err == nil {
		t.Fatal("expected conflict error")
	}
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.StructuredContent["reason"] != "delete_mismatch" {
		t.Fatalf("unexpected conflict reason: %#v", toolErr.StructuredContent)
	}
	if toolErr.StructuredContent["hunk_index"] != 1 {
		t.Fatalf("expected hunk_index=1, got %#v", toolErr.StructuredContent)
	}
	if _, ok := toolErr.StructuredContent["expected_lines"]; !ok {
		t.Fatalf("expected structured expected_lines, got %#v", toolErr.StructuredContent)
	}
	payload, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(payload) != original {
		t.Fatalf("file should remain unchanged\nwant: %q\ngot:  %q", original, string(payload))
	}
}

func TestApplyUnifiedDiffSupportsNoNewlineMarker(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.apply_unified_diff")
	target := filepath.Join(cfg.StartupDirectory, "no-eol.txt")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := strings.Join([]string{
		"--- a/no-eol.txt",
		"+++ b/no-eol.txt",
		"@@ -1 +1 @@",
		"-old",
		"\\ No newline at end of file",
		"+new",
		"\\ No newline at end of file",
		"",
	}, "\n")

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path": "no-eol.txt",
		"diff": diff,
	})
	if err != nil {
		t.Fatalf("apply_unified_diff failed: %v", err)
	}
	payload, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "new" {
		t.Fatalf("unexpected file contents: %q", string(payload))
	}
}
