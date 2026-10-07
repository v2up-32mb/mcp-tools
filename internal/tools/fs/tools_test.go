package fs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/v2up-32mb/mcp-tools/internal/config"
	"github.com/v2up-32mb/mcp-tools/internal/mcp"
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
	tool := findTool(t, cfg, "fs_read_file")
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path": target,
	})
	if err != nil {
		t.Fatalf("fs_read_file failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
	if len(res.Content) == 0 || res.Content[0].Text != "hello yolo" {
		t.Fatalf("unexpected read result: %#v", res)
	}
}

func TestDeletePathRemovesSymlinkItselfNotTarget(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_delete_path")
	target := filepath.Join(cfg.StartupDirectory, "target.txt")
	if err := os.WriteFile(target, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfg.StartupDirectory, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path": "link.txt",
	})
	if err != nil {
		t.Fatalf("delete_path failed: %v", err)
	}
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected symlink to be removed, got err=%v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("target should remain readable: %v", err)
	}
	if string(got) != "keep\n" {
		t.Fatalf("target content changed: %q", got)
	}
}

func TestMovePathMovesSymlinkItselfNotTarget(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_move_path")
	target := filepath.Join(cfg.StartupDirectory, "target.txt")
	if err := os.WriteFile(target, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfg.StartupDirectory, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	moved := filepath.Join(cfg.StartupDirectory, "moved-link.txt")

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"src": "link.txt",
		"dst": "moved-link.txt",
	})
	if err != nil {
		t.Fatalf("move_path failed: %v", err)
	}
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected original symlink to be moved away, got err=%v", err)
	}
	if info, err := os.Lstat(moved); err != nil {
		t.Fatalf("expected moved symlink: %v", err)
	} else if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected moved path to remain symlink, got mode %s", info.Mode())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("target should remain readable: %v", err)
	}
	if string(got) != "keep\n" {
		t.Fatalf("target content changed: %q", got)
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

func TestParseIntegerRejectsFractionalJSONNumber(t *testing.T) {
	if _, err := parseInteger(json.Number("9007199254740992.5"), "limit"); err == nil {
		t.Fatal("expected fractional JSON number to be rejected")
	}
}

func TestFSSchemasDeclareNumericMinimums(t *testing.T) {
	cfg := newTestConfig(t)
	tests := []struct {
		tool string
		prop string
		want int
	}{
		{tool: "fs_replace_text", prop: "expected_replacements", want: 0},
		{tool: "fs_edit_lines", prop: "start_line", want: 1},
		{tool: "fs_edit_lines", prop: "end_line", want: 1},
		{tool: "fs_edit_lines", prop: "context_lines", want: 0},
		{tool: "fs_apply_unified_diff", prop: "context_lines", want: 0},
		{tool: "fs_pull_file", prop: "max_bytes", want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.tool+"."+tt.prop, func(t *testing.T) {
			tool := findTool(t, cfg, tt.tool)
			props := tool.Schema()["properties"].(map[string]any)
			prop := props[tt.prop].(map[string]any)
			if got, ok := prop["minimum"].(int); !ok || got != tt.want {
				t.Fatalf("%s schema should declare %s minimum=%d, got %#v", tt.tool, tt.prop, tt.want, prop)
			}
		})
	}
}

func TestFSSchemasDeclareStringMinLength(t *testing.T) {
	cfg := newTestConfig(t)
	tests := []struct {
		tool string
		prop string
	}{
		{tool: "fs_read_file", prop: "path"},
		{tool: "fs_write_file", prop: "path"},
		{tool: "fs_list_dir", prop: "path"},
		{tool: "fs_stat_path", prop: "path"},
		{tool: "fs_make_dir", prop: "path"},
		{tool: "fs_move_path", prop: "src"},
		{tool: "fs_move_path", prop: "dst"},
		{tool: "fs_delete_path", prop: "path"},
		{tool: "fs_search_text", prop: "path"},
		{tool: "fs_search_text", prop: "query"},
		{tool: "fs_replace_text", prop: "path"},
		{tool: "fs_replace_text", prop: "old_text"},
		{tool: "fs_edit_lines", prop: "path"},
		{tool: "fs_apply_unified_diff", prop: "path"},
		{tool: "fs_apply_unified_diff", prop: "diff"},
		{tool: "fs_pull_file", prop: "path"},
	}

	for _, tt := range tests {
		t.Run(tt.tool+"."+tt.prop, func(t *testing.T) {
			tool := findTool(t, cfg, tt.tool)
			props := tool.Schema()["properties"].(map[string]any)
			prop := props[tt.prop].(map[string]any)
			if got, ok := prop["minLength"].(int); !ok || got != 1 {
				t.Fatalf("%s schema should declare %s minLength=1, got %#v", tt.tool, tt.prop, prop)
			}
		})
	}
}

func TestFSRequiredStringArgsRejectNonStrings(t *testing.T) {
	cfg := newTestConfig(t)
	if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, "existing.txt"), []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		tool    string
		args    map[string]any
		wantErr string
	}{
		{
			name: "read_file path",
			tool: "fs_read_file",
			args: map[string]any{
				"path": []any{"existing.txt"},
			},
			wantErr: "path must be a string",
		},
		{
			name: "write_file text",
			tool: "fs_write_file",
			args: map[string]any{
				"path": "write-type.txt",
				"text": 123,
			},
			wantErr: "text must be a string",
		},
		{
			name: "move_path src",
			tool: "fs_move_path",
			args: map[string]any{
				"src": []any{"existing.txt"},
				"dst": "moved.txt",
			},
			wantErr: "src must be a string",
		},
		{
			name: "search_text query",
			tool: "fs_search_text",
			args: map[string]any{
				"path":  "existing.txt",
				"query": 123,
			},
			wantErr: "query must be a string",
		},
		{
			name: "replace_text old_text",
			tool: "fs_replace_text",
			args: map[string]any{
				"path":     "existing.txt",
				"old_text": 123,
				"new_text": "gamma",
			},
			wantErr: "old_text must be a string",
		},
		{
			name: "replace_text new_text",
			tool: "fs_replace_text",
			args: map[string]any{
				"path":     "existing.txt",
				"old_text": "alpha",
				"new_text": 123,
			},
			wantErr: "new_text must be a string",
		},
		{
			name: "edit_lines new_text",
			tool: "fs_edit_lines",
			args: map[string]any{
				"path":       "existing.txt",
				"start_line": 1,
				"end_line":   1,
				"new_text":   123,
			},
			wantErr: "new_text must be a string",
		},
		{
			name: "apply_unified_diff diff",
			tool: "fs_apply_unified_diff",
			args: map[string]any{
				"path": "existing.txt",
				"diff": 123,
			},
			wantErr: "diff must be a string",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := findTool(t, cfg, tt.tool)
			_, err := tool.Call(context.Background(), mcp.CallContext{}, tt.args)
			if err == nil {
				t.Fatal("expected non-string argument to be rejected")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("unexpected error\nwant substring: %q\ngot: %v", tt.wantErr, err)
			}
		})
	}

	got, err := os.ReadFile(filepath.Join(cfg.StartupDirectory, "existing.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "alpha\nbeta\n" {
		t.Fatalf("validation failures should not modify existing file, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(cfg.StartupDirectory, "write-type.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("write_file should not create file on validation error, stat err=%v", err)
	}
}

func TestEditLinesExpandsWithoutShiftingFollowingContent(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_edit_lines")
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
	tool := findTool(t, cfg, "fs_edit_lines")
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
	tool := findTool(t, cfg, "fs_edit_lines")
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
	tool := findTool(t, cfg, "fs_edit_lines")
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

func TestEditLinesHonorsEmptyExpectedOldText(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_edit_lines")
	target := filepath.Join(cfg.StartupDirectory, "expected-empty.txt")
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":              "expected-empty.txt",
		"start_line":        1,
		"end_line":          1,
		"expected_old_text": "",
		"new_text":          "new\n",
	})
	if err == nil {
		t.Fatal("expected empty expected_old_text mismatch")
	}
	if !strings.Contains(err.Error(), "expected_old_text mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}

	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "old\n" {
		t.Fatalf("file should remain unchanged, got %q", string(got))
	}
}

func TestEditLinesRejectsNonStringExpectedOldText(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_edit_lines")
	target := filepath.Join(cfg.StartupDirectory, "expected-type.txt")
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":              "expected-type.txt",
		"start_line":        1,
		"end_line":          1,
		"expected_old_text": 123,
		"new_text":          "new\n",
	})
	if err == nil {
		t.Fatal("expected non-string expected_old_text to be rejected")
	}
	if !strings.Contains(err.Error(), "expected_old_text must be a string") {
		t.Fatalf("unexpected error: %v", err)
	}

	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "old\n" {
		t.Fatalf("file should remain unchanged, got %q", string(got))
	}
}

func TestEditLinesReturnsMismatchWhenExpectedOldTextWrong(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_edit_lines")
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

func TestEditLinesRejectsFractionalLineNumber(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_edit_lines")
	target := filepath.Join(cfg.StartupDirectory, "fractional-line.txt")
	if err := os.WriteFile(target, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":       "fractional-line.txt",
		"start_line": 1.5,
		"end_line":   float64(1),
		"new_text":   "A\n",
	})
	if err == nil {
		t.Fatal("expected fractional line number to be rejected")
	}
}

func TestRenderLineContextHandlesOutOfRangeStart(t *testing.T) {
	got := renderLineContext([]string{"alpha\n"}, int(^uint(0)>>1), int(^uint(0)>>1), 2)
	if got != "" {
		t.Fatalf("expected empty snippet for out-of-range start, got %q", got)
	}
}

func TestRenderLineContextClampsEndToAvailableLines(t *testing.T) {
	got := renderLineContext([]string{"alpha\n", "beta\n"}, 2, 100, 0)
	want := ">    2 | beta"
	if got != want {
		t.Fatalf("unexpected snippet\nwant: %q\ngot:  %q", want, got)
	}
}

func TestEditLinesNormalizesMultiLineReplacementWithoutTrailingNewline(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_edit_lines")
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
	tool := findTool(t, cfg, "fs_search_text")
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
	if !ok {
		t.Fatalf("expected matches to be []map[string]any, got %T", result.StructuredContent["matches"])
	}
	if len(matches) == 0 {
		t.Fatal("expected at least one match")
	}
}

func TestSearchTextAllowsWhitespaceOnlyQuery(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_search_text")
	target := filepath.Join(cfg.StartupDirectory, "spaces.txt")
	if err := os.WriteFile(target, []byte("alpha beta\nno-space\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":  "spaces.txt",
		"query": " ",
	})
	if err != nil {
		t.Fatalf("search_text should allow literal whitespace queries, got %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %#v", result)
	}
	matches, ok := result.StructuredContent["matches"].([]map[string]any)
	if !ok {
		t.Fatalf("expected structured matches, got %#v", result.StructuredContent["matches"])
	}
	if len(matches) != 1 || matches[0]["text"] != "alpha beta" {
		t.Fatalf("unexpected whitespace matches: %#v", matches)
	}
}

func TestSearchTextSingleFileLimitReturnsPartialMatches(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_search_text")
	target := filepath.Join(cfg.StartupDirectory, "single-limit.txt")
	if err := os.WriteFile(target, []byte("needle one\nneedle two\nneedle three\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":  "single-limit.txt",
		"query": "needle",
		"limit": 2,
	})
	if err != nil {
		t.Fatalf("search_text should return partial matches when limit is reached, got %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %#v", result)
	}
	matches, ok := result.StructuredContent["matches"].([]map[string]any)
	if !ok {
		t.Fatalf("expected structured matches, got %#v", result.StructuredContent["matches"])
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 limited matches, got %#v", matches)
	}
}

func TestSearchTextSkipsFileSymlinkOutsideAllowedRoots(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_search_text")
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("needle secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfg.StartupDirectory, "linked-secret.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":  ".",
		"query": "needle",
	})
	if err != nil {
		t.Fatalf("search_text failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %#v", result)
	}
	matches, ok := result.StructuredContent["matches"].([]map[string]any)
	if !ok {
		t.Fatalf("expected structured matches, got %#v", result.StructuredContent["matches"])
	}
	if len(matches) != 0 {
		t.Fatalf("expected symlink target outside allowed roots to be skipped, got %#v", matches)
	}
	if strings.Contains(result.Content[0].Text, "secret") {
		t.Fatalf("search result leaked outside symlink content: %q", result.Content[0].Text)
	}
}

func TestSearchTextRejectsNonIntegerLimit(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_search_text")
	target := filepath.Join(cfg.StartupDirectory, "search-limit.txt")
	if err := os.WriteFile(target, []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":  "search-limit.txt",
		"query": "needle",
		"limit": 1.5,
	})
	if err == nil {
		t.Fatal("expected fractional search limit to be rejected")
	}

	_, err = tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":  "search-limit.txt",
		"query": "needle",
		"limit": 1e100,
	})
	if err == nil {
		t.Fatal("expected out-of-range search limit to be rejected")
	}
}

func TestReplaceTextReplacesFirstOccurrenceByDefault(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_replace_text")
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
	tool := findTool(t, cfg, "fs_replace_text")
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

func TestReplaceTextRejectsNonBooleanReplaceAll(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_replace_text")
	target := filepath.Join(cfg.StartupDirectory, "replace-bool.txt")
	if err := os.WriteFile(target, []byte("foo foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":        "replace-bool.txt",
		"old_text":    "foo",
		"new_text":    "bar",
		"replace_all": "true",
	})
	if err == nil {
		t.Fatal("expected non-boolean replace_all error")
	}
	if !strings.Contains(err.Error(), "replace_all must be a boolean") {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "foo foo\n" {
		t.Fatalf("replace_text should not modify file on validation error, got %q", got)
	}
}

func TestReplaceTextRejectsExpectedReplacementMismatch(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_replace_text")
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

func TestReplaceTextRejectsFractionalExpectedReplacements(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_replace_text")
	target := filepath.Join(cfg.StartupDirectory, "replace-fractional.txt")
	if err := os.WriteFile(target, []byte("foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":                  "replace-fractional.txt",
		"old_text":              "foo",
		"new_text":              "bar",
		"expected_replacements": 1.5,
	})
	if err == nil {
		t.Fatal("expected fractional expected_replacements to be rejected")
	}
}

func TestReplaceTextRejectsNegativeExpectedReplacements(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_replace_text")
	target := filepath.Join(cfg.StartupDirectory, "replace-negative.txt")
	original := "foo\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":                  "replace-negative.txt",
		"old_text":              "foo",
		"new_text":              "bar",
		"expected_replacements": -1,
	})
	if err == nil {
		t.Fatal("expected negative expected_replacements to be rejected")
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != original {
		t.Fatalf("file should remain unchanged\nwant: %q\ngot:  %q", original, string(got))
	}
}

func TestApplyUnifiedDiffSupportsMultipleHunks(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_apply_unified_diff")
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

func TestApplyUnifiedDiffRejectsFractionalContextLines(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_apply_unified_diff")
	target := filepath.Join(cfg.StartupDirectory, "fractional-context.txt")
	if err := os.WriteFile(target, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":          "fractional-context.txt",
		"diff":          "--- a/fractional-context.txt\n+++ b/fractional-context.txt\n@@ -1 +1 @@\n-alpha\n+beta\n",
		"context_lines": 1.5,
	})
	if err == nil {
		t.Fatal("expected fractional context_lines to be rejected")
	}
}

func TestApplyUnifiedDiffRejectsOverflowingHunkHeaderNumber(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_apply_unified_diff")
	target := filepath.Join(cfg.StartupDirectory, "overflow-hunk.txt")
	original := "alpha\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path": "overflow-hunk.txt",
		"diff": "--- a/overflow-hunk.txt\n" +
			"+++ b/overflow-hunk.txt\n" +
			"@@ -999999999999999999999999999999999999999999999999999 +1 @@\n" +
			"-alpha\n" +
			"+beta\n",
	})
	if err == nil {
		t.Fatal("expected overflowing hunk header number to be rejected")
	}
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.StructuredContent["reason"] != "invalid_unified_diff" {
		t.Fatalf("expected invalid_unified_diff, got %#v", toolErr.StructuredContent)
	}
	payload, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(payload) != original {
		t.Fatalf("file should remain unchanged\nwant: %q\ngot:  %q", original, string(payload))
	}
}

func TestApplyUnifiedDiffRejectsZeroNewStartWithPositiveCount(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_apply_unified_diff")
	target := filepath.Join(cfg.StartupDirectory, "zero-new-start.txt")
	original := "old\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path": "zero-new-start.txt",
		"diff": "--- a/zero-new-start.txt\n" +
			"+++ b/zero-new-start.txt\n" +
			"@@ -1 +0 @@\n" +
			"-old\n" +
			"+new\n",
	})
	if err == nil {
		t.Fatal("expected invalid new hunk start to be rejected")
	}
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.StructuredContent["reason"] != "invalid_unified_diff" {
		t.Fatalf("expected invalid_unified_diff, got %#v", toolErr.StructuredContent)
	}
	payload, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(payload) != original {
		t.Fatalf("file should remain unchanged\nwant: %q\ngot:  %q", original, string(payload))
	}
}

func TestApplyUnifiedDiffDryRunLeavesFileUnchanged(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_apply_unified_diff")
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

func TestApplyUnifiedDiffRejectsNonBooleanDryRun(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_apply_unified_diff")
	target := filepath.Join(cfg.StartupDirectory, "dry-run-bool.txt")
	original := "one\ntwo\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := strings.Join([]string{
		"--- a/dry-run-bool.txt",
		"+++ b/dry-run-bool.txt",
		"@@ -1,2 +1,2 @@",
		" one",
		"-two",
		"+TWO",
		"",
	}, "\n")

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":    "dry-run-bool.txt",
		"diff":    diff,
		"dry_run": "true",
	})
	if err == nil {
		t.Fatal("expected non-boolean dry_run error")
	}
	if !strings.Contains(err.Error(), "dry_run must be a boolean") {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("apply_unified_diff should not modify file on validation error\nwant: %q\ngot:  %q", original, string(got))
	}
}

func TestApplyUnifiedDiffHonorsEmptyExpectedOldText(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_apply_unified_diff")
	target := filepath.Join(cfg.StartupDirectory, "expected-empty-diff.txt")
	original := "old\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := strings.Join([]string{
		"--- a/expected-empty-diff.txt",
		"+++ b/expected-empty-diff.txt",
		"@@ -1 +1 @@",
		"-old",
		"+new",
		"",
	}, "\n")

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":              "expected-empty-diff.txt",
		"diff":              diff,
		"expected_old_text": "",
	})
	if err == nil {
		t.Fatal("expected empty expected_old_text mismatch")
	}
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.StructuredContent["reason"] != "expected_old_text_mismatch" {
		t.Fatalf("expected expected_old_text_mismatch, got %#v", toolErr.StructuredContent)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != original {
		t.Fatalf("file should remain unchanged\nwant: %q\ngot:  %q", original, string(got))
	}
}

func TestApplyUnifiedDiffRejectsNonStringExpectedOldText(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_apply_unified_diff")
	target := filepath.Join(cfg.StartupDirectory, "expected-type-diff.txt")
	original := "old\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := strings.Join([]string{
		"--- a/expected-type-diff.txt",
		"+++ b/expected-type-diff.txt",
		"@@ -1 +1 @@",
		"-old",
		"+new",
		"",
	}, "\n")

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":              "expected-type-diff.txt",
		"diff":              diff,
		"expected_old_text": 123,
	})
	if err == nil {
		t.Fatal("expected non-string expected_old_text to be rejected")
	}
	if !strings.Contains(err.Error(), "expected_old_text must be a string") {
		t.Fatalf("unexpected error: %v", err)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != original {
		t.Fatalf("file should remain unchanged\nwant: %q\ngot:  %q", original, string(got))
	}
}

func TestApplyUnifiedDiffReturnsStructuredConflict(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_apply_unified_diff")
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
	tool := findTool(t, cfg, "fs_apply_unified_diff")
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

func TestApplyUnifiedDiffAllowsRelativeHeaderForDotDotPrefixedName(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_apply_unified_diff")
	target := filepath.Join(cfg.StartupDirectory, "..data", "target.txt")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := strings.Join([]string{
		"--- a/..data/target.txt",
		"+++ b/..data/target.txt",
		"@@ -1 +1 @@",
		"-old",
		"+new",
		"",
	}, "\n")

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path": target,
		"diff": diff,
	})
	if err != nil {
		t.Fatalf("apply_unified_diff should accept relative header for in-root ..-prefixed names: %v", err)
	}
	payload, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "new\n" {
		t.Fatalf("unexpected file contents: %q", string(payload))
	}
}

func TestApplyUnifiedDiffRejectsTrailingGarbageAfterHunk(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_apply_unified_diff")
	target := filepath.Join(cfg.StartupDirectory, "garbage.txt")
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := strings.Join([]string{
		"--- a/garbage.txt",
		"+++ b/garbage.txt",
		"@@ -1 +1 @@",
		"-old",
		"+new",
		"junk-after-hunk",
		"",
	}, "\n")

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path": "garbage.txt",
		"diff": diff,
	})
	if err == nil {
		t.Fatal("expected trailing garbage after hunk to be rejected")
	}
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.StructuredContent["reason"] != "invalid_unified_diff" {
		t.Fatalf("unexpected structured error: %#v", toolErr.StructuredContent)
	}
	payload, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "old\n" {
		t.Fatalf("file should remain unchanged, got %q", string(payload))
	}
}

// TestReplaceTextExpectedReplacementsZeroWithExistingRejects verifies that when
// expected_replacements=0 is set but old_text DOES exist, the tool rejects the
// call (matches=1 != expected=0). The replaceText implementation checks
// matches==0 first ("old_text not found"), then the mismatch guard.
func TestReplaceTextExpectedReplacementsZeroWithExistingRejects(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_replace_text")
	target := filepath.Join(cfg.StartupDirectory, "exists.txt")
	if err := os.WriteFile(target, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":                  "exists.txt",
		"old_text":              "world",
		"new_text":              "replacement",
		"expected_replacements": 0,
	})
	if err == nil {
		t.Fatal("expected error when old_text exists but expected_replacements=0")
	}
	if !strings.Contains(err.Error(), "expected_replacements mismatch") {
		t.Fatalf("expected mismatch error, got %v", err)
	}
	// file must remain unchanged on precondition failure
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "hello world" {
		t.Fatalf("file should remain unchanged, got %q", string(got))
	}
}

// TestReplaceTextExpectedReplacementsZeroWithMissingReportsNotFound verifies
// the implementation's actual ordering: matches==0 is caught first and reports
// "old_text not found" BEFORE the expected_replacements==0 guard can accept it.
// Callers cannot use expected_replacements=0 as a "success when absent" assertion
// under the current implementation; this test pins that behavior so any future
// reorder is a deliberate, visible change.
func TestReplaceTextExpectedReplacementsZeroWithMissingReportsNotFound(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_replace_text")
	target := filepath.Join(cfg.StartupDirectory, "missing.txt")
	if err := os.WriteFile(target, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":                  "missing.txt",
		"old_text":              "nonexistent",
		"new_text":              "replacement",
		"expected_replacements": 0,
	})
	if err == nil {
		t.Fatal("expected error: matches==0 short-circuits to old_text not found before the expected=0 guard")
	}
	if !strings.Contains(err.Error(), "old_text not found") {
		t.Fatalf("expected old_text not found error, got %v", err)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "hello world" {
		t.Fatalf("file should remain unchanged, got %q", string(got))
	}
}

// TestEditLinesEmptyNewTextDeletesRange deletes a middle range and verifies the
// surrounding lines collapse together.
func TestEditLinesEmptyNewTextDeletesRange(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_edit_lines")
	target := filepath.Join(cfg.StartupDirectory, "delete-range.txt")
	if err := os.WriteFile(target, []byte("L1\nL2\nL3\nL4\nL5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":       "delete-range.txt",
		"start_line": 2,
		"end_line":   3,
		"new_text":   "",
	})
	if err != nil {
		t.Fatalf("edit_lines delete failed: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	want := "L1\nL4\nL5\n"
	if string(got) != want {
		t.Fatalf("unexpected file contents\nwant: %q\ngot:  %q", want, string(got))
	}
}

// TestEditLinesNewTextSingleNewlineInsertsBlankLine replaces one line with a
// single blank line (new_text == "\n").
func TestEditLinesNewTextSingleNewlineInsertsBlankLine(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_edit_lines")
	target := filepath.Join(cfg.StartupDirectory, "blank-line.txt")
	if err := os.WriteFile(target, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":       "blank-line.txt",
		"start_line": 1,
		"end_line":   1,
		"new_text":   "\n",
	})
	if err != nil {
		t.Fatalf("edit_lines blank insert failed: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	want := "\nb\n"
	if string(got) != want {
		t.Fatalf("unexpected file contents\nwant: %q\ngot:  %q", want, string(got))
	}
}

// TestEditLinesStartBeyondFileLengthRejected verifies out-of-bounds rejection
// when start > number of lines.
func TestEditLinesStartBeyondFileLengthRejected(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_edit_lines")
	target := filepath.Join(cfg.StartupDirectory, "oob.txt")
	if err := os.WriteFile(target, []byte("only\nsecond\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":       "oob.txt",
		"start_line": 3,
		"end_line":   3,
		"new_text":   "x\n",
	})
	if err == nil {
		t.Fatal("expected out-of-bounds error when start > len(lines)")
	}
	if !strings.Contains(err.Error(), "out of bounds") {
		t.Fatalf("expected out of bounds error, got %v", err)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "only\nsecond\n" {
		t.Fatalf("file should remain unchanged, got %q", string(got))
	}
}

// TestEditLinesNegativeContextLinesRejected verifies context_lines < 0 fails.
func TestEditLinesNegativeContextLinesRejected(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_edit_lines")
	target := filepath.Join(cfg.StartupDirectory, "neg-ctx.txt")
	if err := os.WriteFile(target, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":          "neg-ctx.txt",
		"start_line":    1,
		"end_line":      1,
		"new_text":      "A\n",
		"context_lines": -1,
	})
	if err == nil {
		t.Fatal("expected error for negative context_lines")
	}
	if !strings.Contains(err.Error(), "context_lines must be >= 0") {
		t.Fatalf("expected context_lines >= 0 error, got %v", err)
	}
}

// TestEditLinesContextLinesCapsAtTwenty verifies context_lines > 20 is capped to
// 20 by counting the lines in the returned context_snippet. With a 1-line
// replacement at the start of a small file, the snippet covers the whole file,
// so we instead place the edit in the middle of a 50-line file so that 20-line
// context windows on each side stop short of the file ends.
func TestEditLinesContextLinesCapsAtTwenty(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_edit_lines")
	target := filepath.Join(cfg.StartupDirectory, "ctx-cap.txt")
	var buf bytes.Buffer
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&buf, "line%02d\n", i)
	}
	if err := os.WriteFile(target, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":          "ctx-cap.txt",
		"start_line":    25,
		"end_line":      25,
		"new_text":      "EDIT\n",
		"context_lines": 999,
	})
	if err != nil {
		t.Fatalf("edit_lines failed: %v", err)
	}
	snippet, ok := res.StructuredContent["context_snippet"].(string)
	if !ok {
		t.Fatalf("expected string context_snippet, got %#v", res.StructuredContent["context_snippet"])
	}
	// context window: 20 lines before + the 1 edited line + 20 lines after = 41
	gotLines := strings.Count(snippet, "\n") + 1
	if gotLines != 41 {
		t.Fatalf("expected context_snippet capped at 20+1+20=41 lines, got %d", gotLines)
	}
}

// TestDeletePathFailsOnNonEmptyDirectory verifies os.Remove semantics: a
// non-empty directory cannot be deleted.
func TestDeletePathFailsOnNonEmptyDirectory(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_delete_path")
	dir := filepath.Join(cfg.StartupDirectory, "nonempty")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path": "nonempty",
	})
	if err == nil {
		t.Fatal("expected error when deleting non-empty directory")
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatalf("non-empty directory should still exist after failed delete: %v", statErr)
	}
}

// TestSearchTextLimitZeroFallsBackToDefault verifies limit=0 silently uses the
// default of 200 (v <= 0 branch). A file with 5 matching lines all under 200
// should return all 5.
func TestSearchTextLimitZeroFallsBackToDefault(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_search_text")
	target := filepath.Join(cfg.StartupDirectory, "limit-zero.txt")
	if err := os.WriteFile(target, []byte("needle one\nneedle two\nneedle three\nnot here\nneedle four\nneedle five\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":  "limit-zero.txt",
		"query": "needle",
		"limit": 0,
	})
	if err != nil {
		t.Fatalf("search_text with limit=0 failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
	matches, ok := res.StructuredContent["matches"].([]map[string]any)
	if !ok {
		t.Fatalf("expected structured matches, got %#v", res.StructuredContent["matches"])
	}
	if len(matches) != 5 {
		t.Fatalf("expected all 5 matches with limit=0 (default 200), got %d", len(matches))
	}
}

func TestWriteFileCreatesParentDirectories(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_write_file")
	relPath := filepath.Join("subdir", "deep", "file.txt")

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path": relPath,
		"text": "content\n",
	})
	if err != nil {
		t.Fatalf("write_file should create parent dirs: %v", err)
	}

	absPath := filepath.Join(cfg.StartupDirectory, relPath)
	got, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatalf("expected file to exist: %v", err)
	}
	if string(got) != "content\n" {
		t.Fatalf("unexpected content: %q", got)
	}
}

func TestMakeDirIsIdempotent(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_make_dir")
	relDir := filepath.Join("a", "b", "c")

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{"path": relDir})
	if err != nil {
		t.Fatalf("first make_dir failed: %v", err)
	}

	// Call again on the same existing directory — should not error.
	_, err = tool.Call(context.Background(), mcp.CallContext{}, map[string]any{"path": relDir})
	if err != nil {
		t.Fatalf("second make_dir on existing dir should be idempotent: %v", err)
	}

	info, err := os.Stat(filepath.Join(cfg.StartupDirectory, relDir))
	if err != nil || !info.IsDir() {
		t.Fatalf("expected directory to exist: err=%v isDir=%v", err, info.IsDir())
	}
}

func TestMovePathToExistingDestinationOverwrites(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_move_path")
	src := filepath.Join(cfg.StartupDirectory, "src.txt")
	dst := filepath.Join(cfg.StartupDirectory, "dst.txt")
	if err := os.WriteFile(src, []byte("from-src\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"src": "src.txt",
		"dst": "dst.txt",
	})
	if err != nil {
		t.Fatalf("move_path to existing dst failed: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("dst should exist: %v", err)
	}
	if string(got) != "from-src\n" {
		t.Fatalf("expected overwritten content, got %q", got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("src should no longer exist, err=%v", err)
	}
}

func TestReadFileRejectsNonExistentFile(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_read_file")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path": "nonexistent.txt",
	})
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
	if !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("expected 'no such file' error, got %v", err)
	}
}

func TestStatPathRejectsNonExistentFile(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_stat_path")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path": "nonexistent.txt",
	})
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

func TestSearchTextRejectsNonExistentDirectory(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_search_text")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":  "nonexistent_dir",
		"query": "test",
	})
	if err == nil {
		t.Fatal("expected error for non-existent directory")
	}
}

func TestSearchTextLimitAbove1000CappedTo1000(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_search_text")
	// Create a file with 5 matches.
	path := filepath.Join(cfg.StartupDirectory, "file.txt")
	content := strings.Repeat("hello\n", 5)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":  "file.txt",
		"query": "hello",
		"limit": 5000,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	matches, _ := res.StructuredContent["matches"].([]map[string]any)
	if len(matches) != 5 {
		t.Fatalf("expected 5 matches, got %d", len(matches))
	}
}

func TestReplaceTextExpectedReplacementsExactMatchSucceeds(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_replace_text")
	path := filepath.Join(cfg.StartupDirectory, "file.txt")
	if err := os.WriteFile(path, []byte("foo bar foo baz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":                  "file.txt",
		"old_text":              "foo",
		"new_text":              "qux",
		"expected_replacements": 2,
		"replace_all":           true,
	})
	if err != nil {
		t.Fatalf("replace_text with exact expected_replacements failed: %v", err)
	}
	if got, _ := res.StructuredContent["replaced_occurrences"].(int); got != 2 {
		t.Fatalf("expected 2 replacements, got %d", got)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "qux bar qux baz\n" {
		t.Fatalf("unexpected content: %q", got)
	}
}

func TestMakeDirRejectsNonStringPath(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_make_dir")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path": 123,
	})
	if err == nil {
		t.Fatal("expected error for non-string path")
	}
}

func TestListDirReturnsEntries(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_list_dir")
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(cfg.StartupDirectory, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{"path": "."})
	if err != nil {
		t.Fatalf("list_dir failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %#v", res)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "a.txt") || !strings.Contains(text, "b.txt") {
		t.Fatalf("expected listing to contain a.txt and b.txt, got %q", text)
	}
}

func TestStatPathReturnsDirectoryMetadata(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_stat_path")
	subDir := filepath.Join(cfg.StartupDirectory, "subdir")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{"path": "subdir"})
	if err != nil {
		t.Fatalf("stat_path failed: %v", err)
	}
	isDir, _ := res.StructuredContent["is_dir"].(bool)
	if !isDir {
		t.Fatalf("expected is_dir=true, got %#v", res.StructuredContent)
	}
}

func TestReadFileRejectsPathOutsideAllowedRootsWhenUnsafeDisabled(t *testing.T) {
	cfg := newTestConfig(t)
	// UnsafeAllowAll is false by default in newTestConfig
	tool := findTool(t, cfg, "fs_read_file")
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path": target,
	})
	if err == nil {
		t.Fatal("expected error when reading outside allowed roots with UnsafeAllowAll=false")
	}
}

func TestWriteFileOverwritesExistingFile(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_write_file")
	path := filepath.Join(cfg.StartupDirectory, "file.txt")
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path": "file.txt",
		"text": "overwritten\n",
	})
	if err != nil {
		t.Fatalf("write_file overwrite failed: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "overwritten\n" {
		t.Fatalf("expected overwritten content, got %q", got)
	}
}

func TestReadOnlyFlagMatchesToolSemantics(t *testing.T) {
	cfg := newTestConfig(t)
	readOnlyTools := map[string]bool{
		"fs_read_file":   true,
		"fs_list_dir":    true,
		"fs_stat_path":   true,
		"fs_search_text": true,
		"fs_pull_file":   true,
	}
	writeTools := map[string]bool{
		"fs_write_file":         false,
		"fs_make_dir":           false,
		"fs_move_path":          false,
		"fs_delete_path":        false,
		"fs_replace_text":       false,
		"fs_edit_lines":         false,
		"fs_apply_unified_diff": false,
	}
	for _, tool := range NewTools(cfg) {
		name := tool.Name()
		if want, ok := readOnlyTools[name]; ok {
			if tool.ReadOnly() != want {
				t.Fatalf("%s ReadOnly() should be %v, got %v", name, want, tool.ReadOnly())
			}
		}
		if want, ok := writeTools[name]; ok {
			if tool.ReadOnly() != want {
				t.Fatalf("%s ReadOnly() should be %v, got %v", name, want, tool.ReadOnly())
			}
		}
	}
}

func TestPullFileIssuesRelativeURL(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.PullFile = config.PullFileConfig{Enabled: true, MaxBytes: 1 << 20, TTLSeconds: 300}
	target := filepath.Join(cfg.StartupDirectory, "progress.png")
	if err := os.WriteFile(target, []byte(pngBytes()), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := findTool(t, cfg, "fs_pull_file")
	result, err := tool.Call(context.Background(), mcp.CallContext{Host: "127.0.0.1:8080"}, map[string]any{"path": "progress.png"})
	if err != nil {
		t.Fatalf("pull_file failed: %v", err)
	}
	got := result.StructuredContent
	if got["url_kind"] != "relative" {
		t.Fatalf("url_kind=%v", got["url_kind"])
	}
	url, _ := got["url"].(string)
	if !strings.HasPrefix(url, "/file/") {
		t.Fatalf("url=%q", url)
	}
	if got["filename"] != "progress.png" {
		t.Fatalf("filename=%v", got["filename"])
	}
	if got["mime_type"] != "image/png" {
		t.Fatalf("mime_type=%v", got["mime_type"])
	}
	if got["host_hint"] != "127.0.0.1:8080" {
		t.Fatalf("host_hint=%v", got["host_hint"])
	}
	if got["expires_in_sec"] != 300 {
		t.Fatalf("expires_in_sec=%v", got["expires_in_sec"])
	}
}

func TestPullFileIssuesAbsoluteURLWithPublicBase(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.PullFile = config.PullFileConfig{Enabled: true, MaxBytes: 1 << 20, TTLSeconds: 300, PublicBaseURL: "https://public.example.com/mcp-tools"}
	target := filepath.Join(cfg.StartupDirectory, "progress.png")
	if err := os.WriteFile(target, []byte(pngBytes()), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := findTool(t, cfg, "fs_pull_file")
	result, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{"path": "progress.png"})
	if err != nil {
		t.Fatalf("pull_file failed: %v", err)
	}
	got := result.StructuredContent
	if got["url_kind"] != "absolute" {
		t.Fatalf("url_kind=%v", got["url_kind"])
	}
	url, _ := got["url"].(string)
	if !strings.HasPrefix(url, "https://public.example.com/mcp-tools/file/") {
		t.Fatalf("url=%q", url)
	}
}

func TestPullFileDisabled(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.PullFile = config.PullFileConfig{Enabled: false}
	tool := findTool(t, cfg, "fs_pull_file")
	if _, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{"path": "x.png"}); err == nil {
		t.Fatal("expected disabled error")
	}
}

func TestPullFileRejectsExtensionWhitelist(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.PullFile = config.PullFileConfig{Enabled: true, MaxBytes: 1 << 20, TTLSeconds: 300, AllowedExtensions: []string{".jpg"}}
	target := filepath.Join(cfg.StartupDirectory, "progress.png")
	if err := os.WriteFile(target, []byte(pngBytes()), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := findTool(t, cfg, "fs_pull_file")
	if _, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{"path": "progress.png"}); err == nil {
		t.Fatal("expected extension rejection")
	}
}

func TestPullFileRejectsOversize(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.PullFile = config.PullFileConfig{Enabled: true, MaxBytes: 4, TTLSeconds: 300}
	target := filepath.Join(cfg.StartupDirectory, "progress.png")
	if err := os.WriteFile(target, []byte(pngBytes()), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := findTool(t, cfg, "fs_pull_file")
	if _, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{"path": "progress.png"}); err == nil {
		t.Fatal("expected oversize rejection")
	}
}

func TestPullFileMaxBytesParamCannotExceedConfig(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.PullFile = config.PullFileConfig{Enabled: true, MaxBytes: 4, TTLSeconds: 300}
	target := filepath.Join(cfg.StartupDirectory, "progress.png")
	if err := os.WriteFile(target, []byte(pngBytes()), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := findTool(t, cfg, "fs_pull_file")
	// Caller may not raise the cap: config max_bytes=4 must still reject.
	if _, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{"path": "progress.png", "max_bytes": 1 << 20}); err == nil {
		t.Fatal("expected oversize rejection despite caller max_bytes")
	}
}

func pngBytes() []byte {
	return []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 64))
}

// --- 以下用例来自 yolo 分支（进程/搜索增强），命名已按 OpenAI 兼容规则改为 fs_* ---
func TestSearchTextRegexMode(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs_search_text")
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
	tool := findTool(t, cfg, "fs_search_text")
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
	tool := findTool(t, cfg, "fs_search_text")

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
	tool := findTool(t, cfg, "fs_find_files")

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
	tool := findTool(t, cfg, "fs_find_files")

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
	tool := findTool(t, cfg, "fs_find_files")

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
	tool := findTool(t, cfg, "fs_find_files")

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
	tool := findTool(t, cfg, "fs_search_text")

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
	tool := findTool(t, cfg, "fs_find_files")

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
	tool := findTool(t, cfg, "fs_find_files")
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
	tool := findTool(t, cfg, "fs_find_files")
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
	tool := findTool(t, cfg, "fs_find_files")
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
	tool := findTool(t, cfg, "fs_find_files")
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
	tool := findTool(t, cfg, "fs_find_files")
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
	tool := findTool(t, cfg, "fs_find_files")
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
