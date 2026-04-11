package fs

import (
	"context"
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
