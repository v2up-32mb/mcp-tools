package fs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
	"github.com/example/mcp-tools/internal/security"
)

type tool struct {
	name     string
	desc     string
	schema   map[string]any
	readOnly bool
	call     func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error)
}

func (t tool) Name() string           { return t.name }
func (t tool) Description() string    { return t.desc }
func (t tool) Schema() map[string]any { return t.schema }
func (t tool) ReadOnly() bool         { return t.readOnly }
func (t tool) Call(ctx context.Context, callCtx mcp.CallContext, args map[string]any) (mcp.Result, error) {
	return t.call(ctx, callCtx, args)
}

func NewTools(cfg config.Config) []mcp.Tool {
	return []mcp.Tool{
		tool{name: "fs.read_file", desc: "Read a UTF-8 text file within allowed roots.", schema: schemaPath(), readOnly: true, call: readFile(cfg)},
		tool{name: "fs.write_file", desc: "Write a UTF-8 text file atomically within allowed roots.", schema: schemaPathWithText(), call: writeFile(cfg)},
		tool{name: "fs.list_dir", desc: "List entries in a directory within allowed roots.", schema: schemaPath(), readOnly: true, call: listDir(cfg)},
		tool{name: "fs.stat_path", desc: "Return stat information for a path within allowed roots.", schema: schemaPath(), readOnly: true, call: statPath(cfg)},
		tool{name: "fs.make_dir", desc: "Create a directory recursively within allowed roots.", schema: schemaPath(), call: makeDir(cfg)},
		tool{name: "fs.move_path", desc: "Move or rename a path within allowed roots.", schema: schemaMove(), call: movePath(cfg)},
		tool{name: "fs.delete_path", desc: "Delete a file or an empty directory within allowed roots.", schema: schemaPath(), call: deletePath(cfg)},
		tool{name: "fs.search_text", desc: "Search text recursively under a file or directory within allowed roots.", schema: schemaSearch(), readOnly: true, call: searchText(cfg)},
		tool{name: "fs.edit_lines", desc: "Strictly replace a 1-based line range. new_text is interpreted as logical lines, so callers should control intended line breaks explicitly.", schema: schemaEditLines(), call: editLines(cfg)},
	}
}

func readFile(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(_ context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		path, err := resolvePathArg(args, "path", cfg)
		if err != nil {
			return mcp.Result{}, err
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "read failed"})
		}
		summary := fmt.Sprintf("read %d bytes", len(payload))
		return mcp.TextResult(string(payload), map[string]any{
			"summary": summary,
			"path":    path,
			"bytes":   len(payload),
		}, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: summary}), nil
	}
}

func writeFile(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(_ context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		path, err := resolvePathArg(args, "path", cfg)
		if err != nil {
			return mcp.Result{}, err
		}
		text, ok := args["text"].(string)
		if !ok {
			return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("text required"), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "mkdir failed"})
		}
		if err := atomicWrite(path, []byte(text)); err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "write failed"})
		}
		summary := fmt.Sprintf("wrote %d bytes", len(text))
		return mcp.TextResult(summary, map[string]any{
			"summary":       summary,
			"path":          path,
			"bytes_written": len(text),
		}, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: summary}), nil
	}
}

func listDir(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(_ context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		path, err := resolvePathArg(args, "path", cfg)
		if err != nil {
			return mcp.Result{}, err
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "list failed"})
		}
		items := make([]map[string]any, 0, len(entries))
		lines := make([]string, 0, len(entries))
		for _, entry := range entries {
			info, _ := entry.Info()
			line := entry.Name()
			if entry.IsDir() {
				line += "/"
			}
			lines = append(lines, line)
			items = append(items, map[string]any{
				"name":   entry.Name(),
				"is_dir": entry.IsDir(),
				"size":   fileSize(info),
				"mode":   fileMode(info),
			})
		}
		sort.Slice(items, func(i, j int) bool {
			return items[i]["name"].(string) < items[j]["name"].(string)
		})
		sort.Strings(lines)
		summary := fmt.Sprintf("listed %d entries", len(items))
		return mcp.TextResult(strings.Join(lines, "\n"), map[string]any{
			"summary": summary,
			"path":    path,
			"entries": items,
		}, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: summary}), nil
	}
}

func statPath(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(_ context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		path, err := resolvePathArg(args, "path", cfg)
		if err != nil {
			return mcp.Result{}, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "stat failed"})
		}
		summary := "stat ok"
		return mcp.TextResult(summary, map[string]any{
			"summary":  summary,
			"path":     path,
			"size":     info.Size(),
			"mode":     info.Mode().String(),
			"mod_time": info.ModTime().UTC().Format("2006-01-02T15:04:05Z07:00"),
			"is_dir":   info.IsDir(),
		}, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: summary}), nil
	}
}

func makeDir(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(_ context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		path, err := resolvePathArg(args, "path", cfg)
		if err != nil {
			return mcp.Result{}, err
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "mkdir failed"})
		}
		summary := "mkdir ok"
		return mcp.TextResult(summary, map[string]any{"summary": summary, "path": path}, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: summary}), nil
	}
}

func movePath(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(_ context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		src, err := resolvePathArg(args, "src", cfg)
		if err != nil {
			return mcp.Result{}, err
		}
		dst, err := resolvePathArg(args, "dst", cfg)
		if err != nil {
			return mcp.Result{}, err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: dst, Allowed: true, ResultDigest: "mkdir failed"})
		}
		if err := os.Rename(src, dst); err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: src, Allowed: true, ResultDigest: "move failed"})
		}
		summary := "move ok"
		return mcp.TextResult(summary, map[string]any{"summary": summary, "src": src, "dst": dst}, mcp.AuditData{TargetPath: dst, Allowed: true, ResultDigest: summary}), nil
	}
}

func deletePath(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(_ context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		path, err := resolvePathArg(args, "path", cfg)
		if err != nil {
			return mcp.Result{}, err
		}
		if err := os.Remove(path); err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "delete failed"})
		}
		summary := "delete ok"
		return mcp.TextResult(summary, map[string]any{"summary": summary, "path": path}, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: summary}), nil
	}
}

func searchText(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		path, err := resolvePathArg(args, "path", cfg)
		if err != nil {
			return mcp.Result{}, err
		}
		query, ok := args["query"].(string)
		if !ok || strings.TrimSpace(query) == "" {
			return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("query required"), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		limit := 200
		if v, ok := args["limit"].(float64); ok && v > 0 && v <= 1000 {
			limit = int(v)
		}

		matches := make([]map[string]any, 0)
		err = walkSearch(ctx, path, query, limit, &matches)
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "search failed"})
		}
		summary := fmt.Sprintf("found %d matches", len(matches))
		text := summary
		if len(matches) > 0 {
			rows := make([]string, 0, len(matches))
			for _, match := range matches {
				rows = append(rows, fmt.Sprintf("%s:%v: %s", match["path"], match["line"], match["text"]))
			}
			text = strings.Join(rows, "\n")
		}
		return mcp.TextResult(text, map[string]any{
			"summary": summary,
			"path":    path,
			"query":   query,
			"matches": matches,
		}, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: summary}), nil
	}
}

func editLines(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(_ context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		path, err := resolvePathArg(args, "path", cfg)
		if err != nil {
			return mcp.Result{}, err
		}
		start, err := intArg(args, "start_line")
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		end, err := intArg(args, "end_line")
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		if start <= 0 || end < start {
			return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("invalid line range"), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		newText, ok := args["new_text"].(string)
		if !ok {
			return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("new_text required"), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		expected, _ := args["expected_old_text"].(string)
		contextLines := 2
		if v, ok := args["context_lines"].(float64); ok && v >= 0 && v <= 20 {
			contextLines = int(v)
		}

		payload, err := os.ReadFile(path)
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "read failed"})
		}
		lines := splitLinesPreserve(string(payload))
		if start > len(lines) || end > len(lines) {
			return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("line range out of bounds"), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}

		oldText := strings.Join(lines[start-1:end], "")
		if expected != "" && expected != oldText {
			return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("expected_old_text mismatch"), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "edit precondition failed"})
		}

		replacement := normalizeReplacementLines(newText)
		updated := append([]string{}, lines[:start-1]...)
		updated = append(updated, replacement...)
		updated = append(updated, lines[end:]...)
		merged := strings.Join(updated, "")
		if err := atomicWrite(path, []byte(merged)); err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "write failed"})
		}

		updatedLines := splitLinesPreserve(merged)
		snippet := renderLineContext(updatedLines, start, start+max(len(replacement), 1)-1, contextLines)
		summary := fmt.Sprintf("edited lines %d-%d", start, end)
		return mcp.TextResult(summary+"\n"+snippet, map[string]any{
			"summary":         summary,
			"path":            path,
			"start_line":      start,
			"end_line":        end,
			"old_text":        oldText,
			"new_text":        newText,
			"context_snippet": snippet,
		}, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: summary}), nil
	}
}

func resolvePathArg(args map[string]any, key string, cfg config.Config) (string, error) {
	raw, ok := args[key].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return "", mcp.WrapToolError(fmt.Errorf("%s required", key), mcp.AuditData{Allowed: true, ResultDigest: "validation failed"})
	}
	candidate := raw
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(cfg.StartupDirectory, candidate)
	}
	resolved, err := security.ResolvePath(candidate, cfg.AllowedRoots)
	if err != nil {
		allowed := !errors.Is(err, security.ErrPathOutsideAllowedRoots)
		return "", mcp.WrapToolError(fmt.Errorf("%s: %w", key, err), mcp.AuditData{TargetPath: raw, Allowed: allowed, ResultDigest: "path rejected"})
	}
	return resolved, nil
}

func walkSearch(ctx context.Context, path string, query string, limit int, matches *[]map[string]any) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return searchSingleFile(path, query, limit, matches)
	}
	return filepath.WalkDir(path, func(current string, entry iofs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if entry.IsDir() {
			return nil
		}
		if err := searchSingleFile(current, query, limit, matches); err != nil {
			if errors.Is(err, errSearchLimitReached) {
				return filepath.SkipAll
			}
			return nil
		}
		return nil
	})
}

var errSearchLimitReached = errors.New("search limit reached")

func searchSingleFile(path string, query string, limit int, matches *[]map[string]any) error {
	payload, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(bytes.NewReader(payload))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if strings.Contains(line, query) {
			*matches = append(*matches, map[string]any{
				"path": path,
				"line": lineNo,
				"text": line,
			})
			if len(*matches) >= limit {
				return errSearchLimitReached
			}
		}
	}
	return scanner.Err()
}

func splitLinesPreserve(text string) []string {
	if text == "" {
		return nil
	}
	parts := strings.SplitAfter(text, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func normalizeReplacementLines(text string) []string {
	if text == "" {
		return nil
	}
	parts := strings.Split(text, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	lines := make([]string, 0, len(parts))
	for _, part := range parts {
		lines = append(lines, part+"\n")
	}
	return lines
}

func renderLineContext(lines []string, start, end, contextLines int) string {
	if len(lines) == 0 {
		return ""
	}
	if start < 1 {
		start = 1
	}
	if end < start {
		end = start
	}
	lower := max(1, start-contextLines)
	upper := min(len(lines), end+contextLines)
	rows := make([]string, 0, upper-lower+1)
	for idx := lower; idx <= upper; idx++ {
		marker := " "
		if idx >= start && idx <= end {
			marker = ">"
		}
		rows = append(rows, fmt.Sprintf("%s %4d | %s", marker, idx, strings.TrimSuffix(lines[idx-1], "\n")))
	}
	return strings.Join(rows, "\n")
}

func atomicWrite(path string, payload []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func intArg(args map[string]any, key string) (int, error) {
	switch value := args[key].(type) {
	case float64:
		return int(value), nil
	case int:
		return value, nil
	default:
		return 0, fmt.Errorf("%s required", key)
	}
}

func fileSize(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	return info.Size()
}

func fileMode(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	return info.Mode().String()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func schemaPath() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
		},
		"required": []string{"path"},
	}
}

func schemaPathWithText() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
			"text": map[string]any{"type": "string"},
		},
		"required": []string{"path", "text"},
	}
}

func schemaMove() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"src": map[string]any{"type": "string"},
			"dst": map[string]any{"type": "string"},
		},
		"required": []string{"src", "dst"},
	}
}

func schemaSearch() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"path":  map[string]any{"type": "string"},
			"query": map[string]any{"type": "string"},
			"limit": map[string]any{"type": "integer"},
		},
		"required": []string{"path", "query"},
	}
}

func schemaEditLines() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"path":              map[string]any{"type": "string"},
			"start_line":        map[string]any{"type": "integer"},
			"end_line":          map[string]any{"type": "integer"},
			"new_text":          map[string]any{"type": "string"},
			"expected_old_text": map[string]any{"type": "string"},
			"context_lines":     map[string]any{"type": "integer"},
		},
		"required": []string{"path", "start_line", "end_line", "new_text"},
	}
}
