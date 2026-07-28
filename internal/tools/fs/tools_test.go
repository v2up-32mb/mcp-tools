package fs

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestDeletePathRemovesSymlinkItselfNotTarget(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.delete_path")
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
	tool := findTool(t, cfg, "fs.move_path")
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
		{tool: "fs.replace_text", prop: "expected_replacements", want: 0},
		{tool: "fs.edit_lines", prop: "start_line", want: 1},
		{tool: "fs.edit_lines", prop: "end_line", want: 1},
		{tool: "fs.edit_lines", prop: "context_lines", want: 0},
		{tool: "fs.apply_unified_diff", prop: "context_lines", want: 0},
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
		{tool: "fs.read_file", prop: "path"},
		{tool: "fs.write_file", prop: "path"},
		{tool: "fs.list_dir", prop: "path"},
		{tool: "fs.stat_path", prop: "path"},
		{tool: "fs.make_dir", prop: "path"},
		{tool: "fs.move_path", prop: "src"},
		{tool: "fs.move_path", prop: "dst"},
		{tool: "fs.delete_path", prop: "path"},
		{tool: "fs.search_text", prop: "path"},
		{tool: "fs.search_text", prop: "query"},
		{tool: "fs.replace_text", prop: "path"},
		{tool: "fs.replace_text", prop: "old_text"},
		{tool: "fs.edit_lines", prop: "path"},
		{tool: "fs.apply_unified_diff", prop: "path"},
		{tool: "fs.apply_unified_diff", prop: "diff"},
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
			tool: "fs.read_file",
			args: map[string]any{
				"path": []any{"existing.txt"},
			},
			wantErr: "path must be a string",
		},
		{
			name: "write_file text",
			tool: "fs.write_file",
			args: map[string]any{
				"path": "write-type.txt",
				"text": 123,
			},
			wantErr: "text must be a string",
		},
		{
			name: "move_path src",
			tool: "fs.move_path",
			args: map[string]any{
				"src": []any{"existing.txt"},
				"dst": "moved.txt",
			},
			wantErr: "src must be a string",
		},
		{
			name: "search_text query",
			tool: "fs.search_text",
			args: map[string]any{
				"path":  "existing.txt",
				"query": 123,
			},
			wantErr: "query must be a string",
		},
		{
			name: "replace_text old_text",
			tool: "fs.replace_text",
			args: map[string]any{
				"path":     "existing.txt",
				"old_text": 123,
				"new_text": "gamma",
			},
			wantErr: "old_text must be a string",
		},
		{
			name: "replace_text new_text",
			tool: "fs.replace_text",
			args: map[string]any{
				"path":     "existing.txt",
				"old_text": "alpha",
				"new_text": 123,
			},
			wantErr: "new_text must be a string",
		},
		{
			name: "edit_lines new_text",
			tool: "fs.edit_lines",
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
			tool: "fs.apply_unified_diff",
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

func TestEditLinesHonorsEmptyExpectedOldText(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.edit_lines")
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
	tool := findTool(t, cfg, "fs.edit_lines")
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

func TestEditLinesRejectsFractionalLineNumber(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.edit_lines")
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

func TestSearchTextAllowsWhitespaceOnlyQuery(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.search_text")
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
	tool := findTool(t, cfg, "fs.search_text")
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
	tool := findTool(t, cfg, "fs.search_text")
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
	tool := findTool(t, cfg, "fs.search_text")
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

func TestReplaceTextRejectsNonBooleanReplaceAll(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.replace_text")
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

func TestReplaceTextRejectsFractionalExpectedReplacements(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.replace_text")
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
	tool := findTool(t, cfg, "fs.replace_text")
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

func TestApplyUnifiedDiffRejectsFractionalContextLines(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.apply_unified_diff")
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
	tool := findTool(t, cfg, "fs.apply_unified_diff")
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
	tool := findTool(t, cfg, "fs.apply_unified_diff")
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

func TestApplyUnifiedDiffRejectsNonBooleanDryRun(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.apply_unified_diff")
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
	tool := findTool(t, cfg, "fs.apply_unified_diff")
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
	tool := findTool(t, cfg, "fs.apply_unified_diff")
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

func TestApplyUnifiedDiffAllowsRelativeHeaderForDotDotPrefixedName(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "fs.apply_unified_diff")
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
	tool := findTool(t, cfg, "fs.apply_unified_diff")
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
