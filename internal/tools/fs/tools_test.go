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

func TestReadFileAllowsPathOutsideAllowedRootsWhenUnsafeAllowAllEnabled(t *testing.T) {
	root := t.TempDir()
	outsideDir := t.TempDir()
	target := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(target, []byte("hello yolo"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		AuditLogPath:     filepath.Join(root, "audit.jsonl"),
		OutputMaxBytes:   1 << 16,
		CommandTimeout:   5 * time.Second,
		UnsafeAllowAll:   true,
	}
	tool := findTool(t, cfg, "fs.read_file")
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path": target,
	})
	if err != nil {
		t.Fatalf("fs.read_file failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
	if len(res.Content) == 0 || res.Content[0].Text != "hello yolo" {
		t.Fatalf("unexpected read result: %#v", res)
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

func TestSearchTextRegexMode(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.search_text")
	target := filepath.Join(cfg.StartupDirectory, "regex-test.txt")
	content := "func main() {\n  var x = 1\n  func inner() {}\n}\n"
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":  "regex-test.txt",
		"query": `func\s+\w+\s*\(`,
		"regex": true,
	})
	if err != nil {
		t.Fatalf("search_text regex failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %#v", result)
	}
	matches, ok := result.StructuredContent["matches"].([]map[string]any)
	if !ok || len(matches) != 2 {
		t.Fatalf("expected 2 matches for regex, got %d", len(matches))
	}
	if matches[0]["line"] != 1 {
		t.Errorf("expected first match on line 1, got %v", matches[0]["line"])
	}
	if matches[1]["line"] != 3 {
		t.Errorf("expected second match on line 3, got %v", matches[1]["line"])
	}
}

func TestSearchTextInvalidRegex(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.search_text")
	target := filepath.Join(cfg.StartupDirectory, "invalid-regex.txt")
	if err := os.WriteFile(target, []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":  "invalid-regex.txt",
		"query": "[invalid",
		"regex": true,
	})
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
	if !strings.Contains(err.Error(), "invalid regex") {
		t.Fatalf("expected invalid regex error, got: %v", err)
	}
}

func TestSearchTextGitignoreFiltering(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.search_text")

	// Create .gitignore
	gitignoreContent := []byte("*.log\nbuild/\n")
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, ".gitignore"), gitignoreContent, 0o644); err != nil {
		t.Fatal(err)
	}

	// Create files
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "main.go"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "app.log"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.StartupDirectory, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "build", "generated.go"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Search with gitignore enabled (default)
	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":          ".",
		"query":         "needle",
		"use_gitignore": true,
	})
	if err != nil {
		t.Fatalf("search_text failed: %v", err)
	}
	matches, _ := result.StructuredContent["matches"].([]map[string]any)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match with gitignore (should skip .log and build/), got %d", len(matches))
	}

	// Search without gitignore
	result2, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":          ".",
		"query":         "needle",
		"use_gitignore": false,
	})
	if err != nil {
		t.Fatalf("search_text failed: %v", err)
	}
	matches2, _ := result2.StructuredContent["matches"].([]map[string]any)
	if len(matches2) != 3 {
		t.Fatalf("expected 3 matches without gitignore, got %d", len(matches2))
	}
}

func TestFindFilesBasic(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.find_files")

	// Create test structure
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "main_test.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "README.md"), []byte("# test"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.StartupDirectory, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "pkg", "util.go"), []byte("package pkg"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":    ".",
		"pattern": "*.go",
	})
	if err != nil {
		t.Fatalf("find_files failed: %v", err)
	}
	files, _ := result.StructuredContent["files"].([]map[string]any)
	if len(files) != 3 {
		t.Fatalf("expected 3 .go files (root + pkg/), got %d", len(files))
	}
}

func TestFindFilesWithGlobPattern(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.find_files")

	// Create test structure
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.StartupDirectory, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "pkg", "util.go"), []byte("package pkg"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create a nested file
	if err := os.MkdirAll(filepath.Join(cfg.StartupDirectory, "pkg", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "pkg", "nested", "deep.go"), []byte("package nested"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Note: filepath.Match doesn't support **, so we use simple patterns
	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":    ".",
		"pattern": "*.go",
	})
	if err != nil {
		t.Fatalf("find_files failed: %v", err)
	}
	files, _ := result.StructuredContent["files"].([]map[string]any)
	// walkGlob 递归遍历，所有层级的 .go 文件均匹配
	if len(files) != 3 {
		t.Fatalf("expected 3 .go files (recursive), got %d", len(files))
	}
}

func TestFindFilesGitignoreFiltering(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.find_files")

	// Create .gitignore
	gitignoreContent := []byte("*.log\n")
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, ".gitignore"), gitignoreContent, 0o644); err != nil {
		t.Fatal(err)
	}

	// Create files
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "app.log"), []byte("log data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "debug.log"), []byte("log data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Search with gitignore enabled (default)
	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":          ".",
		"pattern":       "*.go",
		"use_gitignore": true,
	})
	if err != nil {
		t.Fatalf("find_files failed: %v", err)
	}
	files, _ := result.StructuredContent["files"].([]map[string]any)
	if len(files) != 1 {
		t.Fatalf("expected 1 .go file with gitignore, got %d", len(files))
	}

	// Search without gitignore - try to find .log files
	result2, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":          ".",
		"pattern":       "*.log",
		"use_gitignore": false,
	})
	if err != nil {
		t.Fatalf("find_files failed: %v", err)
	}
	files2, _ := result2.StructuredContent["files"].([]map[string]any)
	if len(files2) != 2 {
		t.Fatalf("expected 2 .log files without gitignore, got %d", len(files2))
	}
}

func TestFindFilesSingleFile(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.find_files")

	target := filepath.Join(cfg.StartupDirectory, "main.go")
	if err := os.WriteFile(target, []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":    "main.go",
		"pattern": "*.go",
	})
	if err != nil {
		t.Fatalf("find_files failed: %v", err)
	}
	files, _ := result.StructuredContent["files"].([]map[string]any)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
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

// TestMatchGitignorePattern 锁定 gitignore 匹配的关键行为（T3-04/MED）：
// **/ 前缀按段匹配、**/build 不再误杀 xbuild、无斜杠目录名子树忽略、basename best-effort。
func TestMatchGitignorePattern(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		// **/ 前缀：目录名在任意深度忽略（含子树）
		{name: "doublestar vendor root subtree", pattern: "**/vendor/", path: "vendor/foo.go", want: true},
		{name: "doublestar vendor nested subtree", pattern: "**/vendor/", path: "pkg/vendor/foo.go", want: true},
		{name: "doublestar vendor dir itself", pattern: "**/vendor/", path: "vendor", want: true},
		{name: "doublestar vendor no slash", pattern: "**/vendor", path: "vendor/foo.go", want: true},
		{name: "doublestar vendorx not matched", pattern: "**/vendor/", path: "vendorx/foo.go", want: false},
		// **/build vs xbuild 边界（HasSuffix 误杀回归）
		{name: "doublestar build vs xbuild dir", pattern: "**/build", path: "xbuild", want: false},
		{name: "doublestar build vs xbuild subtree", pattern: "**/build/", path: "xbuild/out.go", want: false},
		{name: "doublestar build subtree", pattern: "**/build", path: "build/out.go", want: true},
		{name: "doublestar build nested subtree", pattern: "**/build", path: "pkg/build/out.go", want: true},
		// **/ 多段相对模式
		{name: "doublestar multi segment root", pattern: "**/src/build", path: "src/build/out.go", want: true},
		{name: "doublestar multi segment nested", pattern: "**/src/build", path: "pkg/src/build/out.go", want: true},
		{name: "doublestar multi segment prefix not fooled", pattern: "**/src/build", path: "pkg/src/buildx/out.go", want: false},
		// **/ glob 后缀
		{name: "doublestar glob root", pattern: "**/*.go", path: "util.go", want: true},
		{name: "doublestar glob nested", pattern: "**/*.go", path: "pkg/util.go", want: true},
		{name: "doublestar glob deep", pattern: "**/*.go", path: "pkg/nested/deep.go", want: true},
		{name: "doublestar glob prefix", pattern: "**/test_*.py", path: "pkg/test_a.py", want: true},
		{name: "doublestar glob mismatch", pattern: "**/*.go", path: "pkg/util.py", want: false},
		// 无斜杠目录名：目录整体忽略（真实 gitignore 语义）
		{name: "plain dir subtree", pattern: "build", path: "build/out.go", want: true},
		{name: "plain dir itself", pattern: "build", path: "build", want: true},
		{name: "plain dir not fooled by prefix", pattern: "build", path: "xbuild", want: false},
		{name: "docs subtree", pattern: "docs", path: "docs/readme.md", want: true},
		{name: "docs not fooled", pattern: "docs", path: "mydocs/x.md", want: false},
		// basename best-effort（既有行为保留）
		{name: "basename file", pattern: "README.md", path: "README.md", want: true},
		{name: "basename nested", pattern: "README.md", path: "sub/README.md", want: true},
		{name: "basename glob", pattern: "*.log", path: "app.log", want: true},
		{name: "basename glob nested", pattern: "*.log", path: "sub/app.log", want: true},
		{name: "basename glob no match", pattern: "*.log", path: "app.txt", want: false},
		// 精确路径
		{name: "exact path", pattern: "src/main.go", path: "src/main.go", want: true},
		{name: "exact path different", pattern: "src/main.go", path: "src/other.go", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchGitignorePattern(tt.pattern, tt.path); got != tt.want {
				t.Fatalf("matchGitignorePattern(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}

// TestParseGitignoreSkipsNegationAndComments 锁定 T3-05：
// ! 取反模式显式跳过（当前实现不支持取反，与 README 声明一致），
// 注释与空行跳过，其余模式保留。
func TestParseGitignoreSkipsNegationAndComments(t *testing.T) {
	content := strings.Join([]string{
		"# comment",
		"",
		"*.log",
		"!keep.log",
		"build/",
		"   ",
		"vendor",
	}, "\n")
	got := parseGitignore(content)
	want := []string{"*.log", "build/", "vendor"}
	if len(got) != len(want) {
		t.Fatalf("parseGitignore = %#v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseGitignore[%d] = %q, want %q (full: %#v)", i, got[i], want[i], got)
		}
	}

	// !keep.log 被显式跳过，不作为忽略模式；但 *.log 仍按 basename 匹配 keep.log
	//（不支持取反意味着 keep.log 被 *.log 忽略——与 README 声明的行为一致）
	m := &gitignoreMatcher{patterns: parseGitignore(content)}
	if !m.ShouldIgnore("app.log") {
		t.Fatal("expected app.log to be ignored by *.log")
	}
	parsed := parseGitignore(content)
	for _, p := range parsed {
		if strings.HasPrefix(p, "!") {
			t.Fatalf("negation pattern %q must be skipped by parseGitignore, got %#v", p, parsed)
		}
	}
}

// TestSearchTextGitignoreDoublestarPrefix 端到端验证 **/vendor/ 前缀分支
// 在 search_text 路径下生效（旧实现匹配不到，T3-04 回归锁定）。
func TestSearchTextGitignoreDoublestarPrefix(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.search_text")

	gitignoreContent := []byte("**/vendor/\n")
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, ".gitignore"), gitignoreContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.StartupDirectory, "pkg", "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "pkg", "vendor", "vendored.go"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "main.go"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":          ".",
		"query":         "needle",
		"use_gitignore": true,
	})
	if err != nil {
		t.Fatalf("search_text failed: %v", err)
	}
	matches, _ := result.StructuredContent["matches"].([]map[string]any)
	if len(matches) != 1 {
		t.Fatalf("expected only main.go to match (**/vendor/ should skip pkg/vendor/vendored.go), got %d matches: %#v", len(matches), matches)
	}
	if got, _ := matches[0]["path"].(string); filepath.Base(got) != "main.go" {
		t.Fatalf("expected match on main.go, got %q", got)
	}
}

// TestFindFilesGitignoreDoublestarGlob 端到端验证 **/*.go glob 形态
// 在 find_files 路径下生效（gitignore 过滤维度）。
func TestFindFilesGitignoreDoublestarGlob(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.find_files")

	gitignoreContent := []byte("**/generated/\n")
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, ".gitignore"), gitignoreContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.StartupDirectory, "internal", "generated"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "internal", "generated", "gen.go"), []byte("package gen"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":          ".",
		"pattern":       "*.go",
		"use_gitignore": true,
	})
	if err != nil {
		t.Fatalf("find_files failed: %v", err)
	}
	files, _ := result.StructuredContent["files"].([]map[string]any)
	if len(files) != 1 {
		t.Fatalf("expected only main.go (**/generated/ should skip internal/generated/gen.go), got %d: %#v", len(files), files)
	}
}

// --- find_files ** 跨层级 glob（CORR-5）---

// doublestarFixture 创建跨层级测试结构：
//
//	main.go / test_main.py（根层级）
//	pkg/util.go / pkg/test_util.py
//	pkg/nested/deep.go / pkg/nested/test_deep.py
//	ignored/hidden.go（用于 gitignore 组合用例）
func doublestarFixture(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"main.go":                                      "package main",
		"test_main.py":                                 "# root",
		filepath.Join("pkg", "util.go"):                "package pkg",
		filepath.Join("pkg", "test_util.py"):           "# pkg",
		filepath.Join("pkg", "nested", "deep.go"):      "package nested",
		filepath.Join("pkg", "nested", "test_deep.py"): "# nested",
		filepath.Join("ignored", "hidden.go"):          "package ignored",
	}
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// doublestarPaths 从结果中提取相对路径集合（ToSlash 归一化，Windows 兼容断言）。
func doublestarPaths(t *testing.T, result mcp.Result) map[string]bool {
	t.Helper()
	files, _ := result.StructuredContent["files"].([]map[string]any)
	out := make(map[string]bool, len(files))
	for _, f := range files {
		p, _ := f["path"].(string)
		rel, err := filepath.Rel(t.TempDir(), p)
		if err != nil {
			// path 可能不在 TempDir 下（单文件根等），退回 ToSlash 全路径
			out[filepath.ToSlash(p)] = true
			continue
		}
		_ = rel
		out[filepath.ToSlash(p)] = true
	}
	return out
}

// doublestarBasenames 提取结果的 basename 集合（路径断言更稳）。
func doublestarBasenames(t *testing.T, result mcp.Result) map[string]bool {
	t.Helper()
	files, _ := result.StructuredContent["files"].([]map[string]any)
	out := make(map[string]bool, len(files))
	for _, f := range files {
		p, _ := f["path"].(string)
		out[filepath.Base(p)] = true
	}
	return out
}

// TestFindFilesDoublestarBasic 锁定 ** 基础语义：**/*.go 匹配根目录与所有子目录的 .go 文件。
func TestFindFilesDoublestarBasic(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.find_files")
	doublestarFixture(t, cfg.StartupDirectory)

	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":    ".",
		"pattern": "**/*.go",
	})
	if err != nil {
		t.Fatalf("find_files failed: %v", err)
	}
	got := doublestarBasenames(t, result)
	want := map[string]bool{"main.go": true, "util.go": true, "deep.go": true, "hidden.go": true}
	for name := range want {
		if !got[name] {
			t.Fatalf("expected %s matched by **/*.go, got %#v", name, got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("expected exactly %d .go files, got %#v", len(want), got)
	}
}

// TestFindFilesDoublestarNestedPattern 锁定 **/test_*.py：** 跨段 + 单 * 段组合。
func TestFindFilesDoublestarNestedPattern(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.find_files")
	doublestarFixture(t, cfg.StartupDirectory)

	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":    ".",
		"pattern": "**/test_*.py",
	})
	if err != nil {
		t.Fatalf("find_files failed: %v", err)
	}
	got := doublestarBasenames(t, result)
	want := map[string]bool{"test_main.py": true, "test_util.py": true, "test_deep.py": true}
	for name := range want {
		if !got[name] {
			t.Fatalf("expected %s matched by **/test_*.py, got %#v", name, got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("expected exactly %d test files, got %#v", len(want), got)
	}
}

// TestFindFilesDoublestarZeroDepthAndPrefix 锁定 ** 的零段语义与显式前缀：
// **/pkg/util.go 匹配 pkg/util.go；** 单独作为前缀时也匹配根层级文件。
func TestFindFilesDoublestarZeroDepthAndPrefix(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.find_files")
	doublestarFixture(t, cfg.StartupDirectory)

	// **/pkg/util.go：** 消费零段，pkg/util.go 显式匹配
	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":    ".",
		"pattern": "**/pkg/util.go",
	})
	if err != nil {
		t.Fatalf("find_files failed: %v", err)
	}
	got := doublestarBasenames(t, result)
	if len(got) != 1 || !got["util.go"] {
		t.Fatalf("expected exactly util.go matched by **/pkg/util.go, got %#v", got)
	}

	// **/deep.go：** 消费 pkg/nested 两段
	result2, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":    ".",
		"pattern": "**/deep.go",
	})
	if err != nil {
		t.Fatalf("find_files failed: %v", err)
	}
	got2 := doublestarBasenames(t, result2)
	if len(got2) != 1 || !got2["deep.go"] {
		t.Fatalf("expected exactly deep.go matched by **/deep.go, got %#v", got2)
	}
}

// TestFindFilesDoublestarWithGitignore 锁定 ** 与 use_gitignore 组合正确：
// **/*.go 在 gitignore 过滤启用时跳过 ignored/，关闭时包含。
func TestFindFilesDoublestarWithGitignore(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.find_files")
	doublestarFixture(t, cfg.StartupDirectory)
	gitignoreContent := []byte("ignored/\n")
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, ".gitignore"), gitignoreContent, 0o644); err != nil {
		t.Fatal(err)
	}

	// 默认启用 gitignore：ignored/hidden.go 被跳过
	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":    ".",
		"pattern": "**/*.go",
	})
	if err != nil {
		t.Fatalf("find_files failed: %v", err)
	}
	got := doublestarBasenames(t, result)
	if got["hidden.go"] {
		t.Fatalf("expected ignored/hidden.go filtered by gitignore, got %#v", got)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 .go files with gitignore, got %#v", got)
	}

	// use_gitignore=false：hidden.go 回来
	result2, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":          ".",
		"pattern":       "**/*.go",
		"use_gitignore": false,
	})
	if err != nil {
		t.Fatalf("find_files failed: %v", err)
	}
	got2 := doublestarBasenames(t, result2)
	if !got2["hidden.go"] || len(got2) != 4 {
		t.Fatalf("expected 4 .go files without gitignore, got %#v", got2)
	}
}

// TestFindFilesDoublestarLimitTruncation 锁定 ** 匹配结果受 limit 约束。
func TestFindFilesDoublestarLimitTruncation(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.find_files")
	doublestarFixture(t, cfg.StartupDirectory)

	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":    ".",
		"pattern": "**/*.go",
		"limit":   float64(2),
	})
	if err != nil {
		t.Fatalf("find_files failed: %v", err)
	}
	files, _ := result.StructuredContent["files"].([]map[string]any)
	if len(files) != 2 {
		t.Fatalf("expected limit=2 to truncate **/*.go results to 2, got %d: %#v", len(files), files)
	}
}

// TestFindFilesSingleStarDoesNotCrossSeparator 锁定单 * 语义不变：
// 路径段通配 pkg/*\/deep.go 不等价 **/deep.go 的既有行为保持——
// pkg/ 子层级由递归遍历 + basename 匹配覆盖，显式路径段模式 **/pkg/*.go 只匹配 pkg 直下。
func TestFindFilesSingleStarDoesNotCrossSeparator(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.find_files")
	doublestarFixture(t, cfg.StartupDirectory)

	// **/pkg/*.go：* 段不跨 /，只匹配 pkg 直下的 util.go，不含 pkg/nested/deep.go
	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":    ".",
		"pattern": "**/pkg/*.go",
	})
	if err != nil {
		t.Fatalf("find_files failed: %v", err)
	}
	got := doublestarBasenames(t, result)
	if len(got) != 1 || !got["util.go"] {
		t.Fatalf("expected single * not to cross / (only pkg/util.go), got %#v", got)
	}

	// **/*.go 与 *.go：单 * 模式（无 ** 段）保持既有递归+basename 语义
	result2, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":    ".",
		"pattern": "test_*.py",
	})
	if err != nil {
		t.Fatalf("find_files failed: %v", err)
	}
	got2 := doublestarBasenames(t, result2)
	// 既有语义：递归遍历所有层级 + basename 匹配
	want2 := map[string]bool{"test_main.py": true, "test_util.py": true, "test_deep.py": true}
	for name := range want2 {
		if !got2[name] {
			t.Fatalf("expected %s matched by existing single-* semantics, got %#v", name, got2)
		}
	}
	if len(got2) != len(want2) {
		t.Fatalf("expected exactly %d files under single-* semantics, got %#v", len(want2), got2)
	}
}

// TestMatchDoublestarGlobSegments 单元锁定段匹配核心：** 多段消费、零段、单 * 不跨 /、非法段。
func TestMatchDoublestarGlobSegments(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/*.go", "main.go", true},
		{"**/*.go", "pkg/util.go", true},
		{"**/*.go", "pkg/nested/deep.go", true},
		{"**/*.go", "main.py", false},
		{"**/pkg/util.go", "pkg/util.go", true},
		{"**/pkg/util.go", "other/util.go", false},
		{"**/deep.go", "pkg/nested/deep.go", true},
		{"**", "anything/here.txt", true},
		{"**/pkg/*.go", "pkg/util.go", true},
		{"**/pkg/*.go", "pkg/nested/deep.go", false}, // 单 * 不跨 /
		{"pkg/**/*.go", "pkg/nested/deep.go", true},
		{"pkg/**/*.go", "other/nested/deep.go", false},
		{"?.go", "a.go", true},
		{"?.go", "ab.go", false},
	}
	for _, tc := range cases {
		if got := matchDoublestarGlob(tc.pattern, tc.path); got != tc.want {
			t.Errorf("matchDoublestarGlob(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

// TestHasDoublestarSegment 锁定完整段判定：仅独立 "**" 段生效，"**.go" 不算。
func TestHasDoublestarSegment(t *testing.T) {
	if !hasDoublestarSegment("**/*.go") {
		t.Fatal("expected **/*.go to have doublestar segment")
	}
	if !hasDoublestarSegment("pkg/**") {
		t.Fatal("expected pkg/** to have doublestar segment")
	}
	if hasDoublestarSegment("**.go") {
		t.Fatal("expected **.go NOT to count as doublestar segment")
	}
	if hasDoublestarSegment("*.go") {
		t.Fatal("expected *.go NOT to have doublestar segment")
	}
}
