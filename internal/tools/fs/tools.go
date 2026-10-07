package fs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/example/mcp-tools/internal/applog"
	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
	"github.com/example/mcp-tools/internal/numconv"
	"github.com/example/mcp-tools/internal/pullfile"
	"github.com/example/mcp-tools/internal/security"
	"github.com/example/mcp-tools/internal/util"
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

func NewTools(cfg config.Config, pull ...*pullfile.Manager) []mcp.Tool {
	mgr := pullfile.NewManager(cfg, nil)
	if len(pull) > 0 && pull[0] != nil {
		mgr = pull[0]
	}
	return []mcp.Tool{
		tool{name: "fs_read_file", desc: "Read a UTF-8 text file from path. path may be relative to the startup directory and must resolve inside allowed roots.", schema: schemaPath(), readOnly: true, call: readFile(cfg)},
		tool{name: "fs_write_file", desc: "Atomically replace or create a UTF-8 text file. path may be relative to the startup directory; parent directories are created if needed.", schema: schemaPathWithText(), call: writeFile(cfg)},
		tool{name: "fs_list_dir", desc: "List direct children of a directory. Non-recursive; path must resolve inside allowed roots.", schema: schemaPath(), readOnly: true, call: listDir(cfg)},
		tool{name: "fs_stat_path", desc: "Return size, mode, mod_time, and is_dir for a file or directory inside allowed roots.", schema: schemaPath(), readOnly: true, call: statPath(cfg)},
		tool{name: "fs_make_dir", desc: "Create a directory recursively with mkdir -p semantics inside allowed roots.", schema: schemaPath(), call: makeDir(cfg)},
		tool{name: "fs_move_path", desc: "Move or rename src to dst. Both src and dst must stay inside allowed roots; missing destination parents are created.", schema: schemaMove(), call: movePath(cfg)},
		tool{name: "fs_delete_path", desc: "Delete a file or an empty directory. This is not recursive delete; non-empty directories will fail.", schema: schemaPath(), call: deletePath(cfg)},
		tool{name: "fs_search_text", desc: "Search by plain substring or regex pattern in one file or recursively under a directory. Supports .gitignore filtering by default. Default limit is 200, max 1000, and long lines are supported.", schema: schemaSearch(), readOnly: true, call: searchText(cfg)},
		tool{name: "fs_find_files", desc: "Find files matching a glob pattern recursively under a directory. Supports .gitignore filtering. Default limit is 100, max 1000.", schema: schemaFindFiles(), readOnly: true, call: findFiles(cfg)},
		tool{name: "fs_replace_text", desc: "Replace exact old_text with new_text in one file. Supports replace-first or replace-all and can assert expected_replacements before writing.", schema: schemaReplaceText(), call: replaceText(cfg)},
		tool{name: "fs_apply_unified_diff", desc: "Apply a standard unified diff to exactly one file using strict matching. path is passed separately, the diff header must match it, multiple hunks are allowed, dry_run validates without writing, and any hunk mismatch fails the whole patch with structured conflict details.", schema: schemaApplyUnifiedDiff(), call: applyUnifiedDiff(cfg)},
		tool{name: "fs_edit_lines", desc: "Strictly replace a 1-based line range. new_text is interpreted as logical lines; blank lines are preserved, empty string deletes the range, and \"\n\" inserts one blank line. expected_old_text can be used as an optimistic concurrency check.", schema: schemaEditLines(), call: editLines(cfg)},
		tool{name: "fs_pull_file", desc: "Issue a short-lived signed download URL for a file inside allowed roots. The client downloads the file with GET on the returned url; the url is relative to the client's MCP base URL unless pull_file.url.public_base_url is configured. Type and size limits come from the pull_file config.", schema: schemaPullFile(), readOnly: true, call: pullFile(cfg, mgr)},
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
		text, err := requiredStringArg(args, "text")
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
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
		src, err := resolvePathNoFollowFinalArg(args, "src", cfg)
		if err != nil {
			return mcp.Result{}, err
		}
		dst, err := resolvePathNoFollowFinalArg(args, "dst", cfg)
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
		path, err := resolvePathNoFollowFinalArg(args, "path", cfg)
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
		query, err := requiredStringArg(args, "query")
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		if query == "" {
			return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("query required"), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		limit := 200
		if v, ok, err := optionalIntArg(args, "limit"); err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		} else if ok {
			switch {
			case v <= 0:
				limit = 200
			case v > 1000:
				limit = 1000
			default:
				limit = v
			}
		}

		regexMode, _, err := optionalBoolArg(args, "regex")
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		useGitignore := true // 默认启用 .gitignore 过滤，与 README 文档一致
		if v, ok, err := optionalBoolArg(args, "use_gitignore"); err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		} else if ok {
			useGitignore = v
		}

		var matcher func(string, string) bool
		if regexMode {
			re, compileErr := regexp.Compile(query)
			if compileErr != nil {
				return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("invalid regex: %w", compileErr), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
			}
			matcher = func(line, _ string) bool {
				return re.MatchString(line)
			}
		} else {
			matcher = func(line, q string) bool {
				return strings.Contains(line, q)
			}
		}

		matches := make([]map[string]any, 0)
		gitignore, _ := loadGitignore(path)
		err = walkSearch(ctx, path, query, limit, matcher, gitignore, useGitignore, cfg, &matches)
		if err != nil && !errors.Is(err, errSearchLimitReached) {
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
			"regex":   regexMode,
			"matches": matches,
		}, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: summary}), nil
	}
}

func findFiles(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		path, err := resolvePathArg(args, "path", cfg)
		if err != nil {
			return mcp.Result{}, err
		}
		pattern, err := requiredStringArg(args, "pattern")
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		if strings.TrimSpace(pattern) == "" {
			return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("pattern required"), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		limit := 100
		if v, ok, err := optionalIntArg(args, "limit"); err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		} else if ok {
			switch {
			case v <= 0:
				limit = 100
			case v > 1000:
				limit = 1000
			default:
				limit = v
			}
		}
		useGitignore := true // 默认启用 .gitignore 过滤，与 README 文档一致
		if v, ok, err := optionalBoolArg(args, "use_gitignore"); err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		} else if ok {
			useGitignore = v
		}

		entries := make([]map[string]any, 0)
		gitignore, _ := loadGitignore(path)
		if err := walkGlob(ctx, path, pattern, limit, gitignore, useGitignore, cfg, &entries); err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "find_files failed"})
		}
		summary := fmt.Sprintf("found %d files", len(entries))
		text := summary
		if len(entries) > 0 {
			rows := make([]string, 0, len(entries))
			for _, entry := range entries {
				rows = append(rows, entry["path"].(string))
			}
			text = strings.Join(rows, "\n")
		}
		return mcp.TextResult(text, map[string]any{
			"summary": summary,
			"path":    path,
			"pattern": pattern,
			"files":   entries,
		}, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: summary}), nil
	}
}

func replaceText(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(_ context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		path, err := resolvePathArg(args, "path", cfg)
		if err != nil {
			return mcp.Result{}, err
		}
		oldText, err := requiredStringArg(args, "old_text")
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		if oldText == "" {
			return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("old_text required and cannot be empty"), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		newText, err := requiredStringArg(args, "new_text")
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		replaceAll, _, err := optionalBoolArg(args, "replace_all")
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		expectedReplacements := -1
		if v, ok, err := optionalIntArg(args, "expected_replacements"); err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		} else if ok {
			if v < 0 {
				return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("expected_replacements must be >= 0"), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
			}
			expectedReplacements = v
		}

		payload, err := os.ReadFile(path)
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "read failed"})
		}
		content := string(payload)
		matches := strings.Count(content, oldText)
		if matches == 0 {
			return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("old_text not found"), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "replace precondition failed"})
		}
		if expectedReplacements >= 0 && matches != expectedReplacements {
			return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("expected_replacements mismatch"), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "replace precondition failed"})
		}

		updated := content
		replacedCount := 1
		if replaceAll {
			updated = strings.ReplaceAll(content, oldText, newText)
			replacedCount = matches
		} else {
			updated = strings.Replace(content, oldText, newText, 1)
		}
		if err := atomicWrite(path, []byte(updated)); err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "write failed"})
		}
		summary := fmt.Sprintf("replaced %d occurrence(s)", replacedCount)
		return mcp.TextResult(summary, map[string]any{
			"summary":              summary,
			"path":                 path,
			"replace_all":          replaceAll,
			"matched_occurrences":  matches,
			"replaced_occurrences": replacedCount,
			"old_text":             oldText,
			"new_text":             newText,
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
		newText, err := requiredStringArg(args, "new_text")
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		expected, expectedSet, err := optionalStringArg(args, "expected_old_text")
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		}
		contextLines := 2
		if v, ok, err := optionalIntArg(args, "context_lines"); err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
		} else if ok {
			switch {
			case v < 0:
				return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("context_lines must be >= 0"), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
			case v > 20:
				contextLines = 20
			default:
				contextLines = v
			}
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
		if expectedSet && expected != oldText {
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

func pullFile(cfg config.Config, mgr *pullfile.Manager) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(_ context.Context, callCtx mcp.CallContext, args map[string]any) (mcp.Result, error) {
		if !cfg.PullFile.Enabled {
			return mcp.Result{}, mcp.WrapToolError(pullfile.ErrDisabled, mcp.AuditData{Allowed: true, ResultDigest: "pull_file disabled"})
		}
		path, err := resolvePathArg(args, "path", cfg)
		if err != nil {
			return mcp.Result{}, err
		}
		maxBytes := cfg.PullFile.MaxBytes
		if raw, ok := args["max_bytes"]; ok && raw != nil {
			parsed, err := parseInteger(raw, "max_bytes")
			if err != nil {
				return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
			}
			if parsed <= 0 {
				return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("max_bytes must be positive"), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "validation failed"})
			}
			if int64(parsed) < maxBytes {
				maxBytes = int64(parsed)
			}
		}
		dl, err := mgr.Issue(path, maxBytes, time.Now())
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "pull link rejected"})
		}
		summary := "issued pull link"
		url, urlKind := "/file/"+dl.Token, "relative"
		if cfg.PullFile.PublicBaseURL != "" {
			url, urlKind = cfg.PullFile.PublicBaseURL+"/file/"+dl.Token, "absolute"
		}
		return mcp.TextResult(summary, map[string]any{
			"summary":        summary,
			"path":           dl.Path,
			"filename":       dl.Filename,
			"bytes":          dl.Bytes,
			"mime_type":      dl.MIMEType,
			"url":            url,
			"url_kind":       urlKind,
			"host_hint":      callCtx.Host,
			"expires_in_sec": cfg.PullFile.TTLSeconds,
			"max_downloads":  cfg.PullFile.MaxDownloads,
		}, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: summary}), nil
	}
}

func schemaPullFile() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"path":      nonEmptyStringSchema(),
			"max_bytes": map[string]any{"type": "integer", "minimum": 1},
		},
		"required": []string{"path"},
	}
}

func resolvePathArg(args map[string]any, key string, cfg config.Config) (string, error) {
	return resolvePathArgWithMode(args, key, cfg, false)
}

func resolvePathNoFollowFinalArg(args map[string]any, key string, cfg config.Config) (string, error) {
	return resolvePathArgWithMode(args, key, cfg, true)
}

func resolvePathArgWithMode(args map[string]any, key string, cfg config.Config, noFollowFinal bool) (string, error) {
	raw, err := requiredStringArg(args, key)
	if err != nil {
		return "", mcp.WrapToolError(err, mcp.AuditData{Allowed: true, ResultDigest: "validation failed"})
	}
	if strings.TrimSpace(raw) == "" {
		return "", mcp.WrapToolError(fmt.Errorf("%s required", key), mcp.AuditData{Allowed: true, ResultDigest: "validation failed"})
	}
	candidate := raw
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(cfg.StartupDirectory, candidate)
	}
	var resolved string
	if cfg.UnsafeAllowAll {
		if noFollowFinal {
			resolved, err = security.ResolvePathUnsafeNoFollowFinal(candidate)
		} else {
			resolved, err = security.ResolvePathUnsafe(candidate)
		}
	} else if noFollowFinal {
		resolved, err = security.ResolvePathNoFollowFinal(candidate, cfg.AllowedRoots)
	} else {
		resolved, err = security.ResolvePath(candidate, cfg.AllowedRoots)
	}
	if err != nil {
		allowed := !errors.Is(err, security.ErrPathOutsideAllowedRoots)
		return "", mcp.WrapToolError(fmt.Errorf("%s: %w", key, err), mcp.AuditData{TargetPath: raw, Allowed: allowed, ResultDigest: "path rejected"})
	}
	return resolved, nil
}

// --- Search Enhancements ---

var errSearchLimitReached = errors.New("search limit reached")

type gitignoreMatcher struct {
	patterns []string
}

func loadGitignore(root string) (*gitignoreMatcher, error) {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	gitignorePath := filepath.Join(root, ".gitignore")
	data, err := os.ReadFile(gitignorePath)
	if err != nil {
		// .gitignore 存在但读取失败时不再完全静默：debug 日志提示，
		// 过滤按关闭处理（返回 nil matcher），不影响搜索本身。
		if !errors.Is(err, os.ErrNotExist) {
			applog.Default().Debug("fs.search", "failed to read .gitignore; gitignore filtering disabled for this search",
				applog.Field{Key: "path", Value: gitignorePath},
				applog.Field{Key: "error", Value: err.Error()})
		}
		return nil, nil
	}
	return &gitignoreMatcher{patterns: parseGitignore(string(data))}, nil
}

func parseGitignore(content string) []string {
	var patterns []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 显式跳过 ! 取反模式（当前实现不支持取反，与 README 声明一致），
		// 避免把 "!keep.log" 当作忽略模式误杀 keep.log。
		if strings.HasPrefix(line, "!") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}

func (g *gitignoreMatcher) ShouldIgnore(path string) bool {
	if g == nil {
		return false
	}
	for _, pattern := range g.patterns {
		if matchGitignorePattern(pattern, path) {
			return true
		}
	}
	return false
}

func matchGitignorePattern(pattern, path string) bool {
	// Simple gitignore matching: handle ** and * wildcards
	if pattern == path {
		return true
	}
	// 去掉目录标记后缀 "/"（"vendor/" 与 "vendor" 语义一致：目录整体忽略）
	pattern = strings.TrimSuffix(pattern, "/")

	if strings.HasPrefix(pattern, "**/") {
		// **/X：X 是相对任意深度的模式（可含多段与 * 通配），
		// 匹配整个路径（根层级）或任意 "/" 边界后的后缀。
		rest := pattern[3:]
		if rest == "" {
			return false
		}
		if matchGitignoreRelative(rest, path) {
			return true
		}
		for i := 0; i < len(path); i++ {
			if path[i] == '/' && matchGitignoreRelative(rest, path[i+1:]) {
				return true
			}
		}
		return false
	}

	if !strings.Contains(pattern, "*") {
		// 无通配符的纯名字：真实 gitignore 语义是目录整体忽略
		if path == pattern || strings.HasPrefix(path, pattern+"/") {
			return true
		}
		// best-effort 保留既有 basename 匹配（文件名模式在任意层级生效）
		return filepath.Base(path) == pattern
	}
	// Simple wildcard support
	matched, _ := filepath.Match(pattern, filepath.Base(path))
	return matched
}

// matchGitignoreRelative 匹配一个相对模式（无前导 /、无 **，可含 * 与多段）
// 与相对路径后缀。用于 **/ 前缀分支。
func matchGitignoreRelative(rest, suffix string) bool {
	if strings.Contains(rest, "*") {
		// 单星通配：先按相对路径整体匹配（filepath.Match 的 * 不跨 /），
		// 再回退到 basename 匹配，覆盖深层级文件（如 "**/test_*.py" 匹配 "pkg/test_a.py"）。
		if m, _ := filepath.Match(rest, suffix); m {
			return true
		}
		m, _ := filepath.Match(rest, filepath.Base(suffix))
		return m
	}
	// 纯多段名字：等于该路径或以其为目录前缀（子树忽略）
	return suffix == rest || strings.HasPrefix(suffix, rest+"/")
}

func shouldIgnorePath(path string, root string, gitignore *gitignoreMatcher, useGitignore bool) bool {
	if !useGitignore || gitignore == nil {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	// Windows 上 filepath.Rel 返回反斜杠路径；模式匹配统一按 "/" 语义处理
	rel = filepath.ToSlash(rel)
	return gitignore.ShouldIgnore(rel)
}

// resolveWalkPath 在非 unsafe 模式下把 walk 命中的条目重新经 allowed roots 校验，
// 防止 symlink 指向 allowed roots 之外；unsafe 模式保持原样。
func resolveWalkPath(current string, cfg config.Config) (string, bool) {
	if cfg.UnsafeAllowAll {
		return current, true
	}
	resolved, err := security.ResolvePath(current, cfg.AllowedRoots)
	if err != nil {
		return "", false
	}
	return resolved, true
}

func walkSearch(ctx context.Context, path string, query string, limit int, matcher func(string, string) bool, gitignore *gitignoreMatcher, useGitignore bool, cfg config.Config, matches *[]map[string]any) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		searchPath, ok := resolveWalkPath(path, cfg)
		if !ok {
			return nil
		}
		return searchSingleFile(searchPath, query, limit, matcher, matches)
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
		if shouldIgnorePath(current, path, gitignore, useGitignore) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		searchPath, ok := resolveWalkPath(current, cfg)
		if !ok {
			return nil
		}
		if err := searchSingleFile(searchPath, query, limit, matcher, matches); err != nil {
			if errors.Is(err, errSearchLimitReached) {
				return filepath.SkipAll
			}
			return nil
		}
		return nil
	})
}

func searchSingleFile(path string, query string, limit int, matcher func(string, string) bool, matches *[]map[string]any) error {
	payload, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lineNo := 0
	for _, lineBytes := range splitLinesBytes(payload) {
		lineNo++
		line := string(lineBytes)
		if matcher(line, query) {
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
	return nil
}

func walkGlob(ctx context.Context, root string, pattern string, limit int, gitignore *gitignoreMatcher, useGitignore bool, cfg config.Config, entries *[]map[string]any) error {
	// ** 跨层级 glob（CORR-5）：模式含独立 ** 段时按相对路径分段匹配；
	// 否则保持既有语义（递归遍历 + basename 匹配，单 * 不跨 /）。
	doublestar := hasDoublestarSegment(pattern)
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		var matched bool
		if doublestar {
			// 单文件根：相对路径即文件名，** 支持零段前缀（**/x 匹配 x）
			matched = matchDoublestarGlob(pattern, filepath.ToSlash(info.Name()))
		} else {
			matched, err = filepath.Match(pattern, info.Name())
			if err != nil {
				return err
			}
		}
		if matched {
			if resolved, ok := resolveWalkPath(root, cfg); ok {
				*entries = append(*entries, map[string]any{
					"path": resolved,
					"name": info.Name(),
					"size": fileSize(info),
				})
			}
		}
		return nil
	}

	return filepath.WalkDir(root, func(current string, entry iofs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if shouldIgnorePath(current, root, gitignore, useGitignore) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		var matched bool
		if doublestar {
			// ** 模式：按相对路径（Windows 反斜杠 ToSlash 归一化）跨段匹配
			rel, relErr := filepath.Rel(root, current)
			if relErr != nil {
				return nil
			}
			matched = matchDoublestarGlob(pattern, filepath.ToSlash(rel))
		} else {
			// 既有语义：递归遍历 + basename 匹配（单 * 不跨 /）
			m, err := filepath.Match(pattern, entry.Name())
			if err != nil {
				return nil
			}
			matched = m
		}
		if matched {
			resolved, ok := resolveWalkPath(current, cfg)
			if !ok {
				return nil
			}
			info, _ := entry.Info()
			*entries = append(*entries, map[string]any{
				"path": resolved,
				"name": entry.Name(),
				"size": fileSize(info),
			})
			if len(*entries) >= limit {
				return filepath.SkipAll
			}
		}
		return nil
	})
}

// hasDoublestarSegment 报告模式是否包含独立的 ** 段（跨层级 glob）。
// 仅完整段 "**" 生效；"**.go" 这类非独立段按单段通配处理（等价 *.go）。
func hasDoublestarSegment(pattern string) bool {
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "**" {
			return true
		}
	}
	return false
}

// matchDoublestarGlob 以 ** 跨段语义匹配斜杠分隔的相对路径：
// ** 段匹配任意数量（含零）的路径段——**/*.go 匹配根目录与所有子目录的 .go 文件；
// 单 * 段不跨 /（filepath.Match 语义）。非法模式段按不匹配处理，
// 与既有 per-entry 静默跳过行为一致。
func matchDoublestarGlob(pattern, relPath string) bool {
	return matchGlobSegments(strings.Split(pattern, "/"), strings.Split(relPath, "/"))
}

// matchGlobSegments 递归匹配模式段与路径段；** 段尝试消费零或多段路径。
func matchGlobSegments(pattern, path []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			// ** 匹配零或多段：逐一起点尝试剩余模式段
			rest := pattern[1:]
			for i := 0; i <= len(path); i++ {
				if matchGlobSegments(rest, path[i:]) {
					return true
				}
			}
			return false
		}
		if len(path) == 0 {
			return false
		}
		matched, err := filepath.Match(pattern[0], path[0])
		if err != nil || !matched {
			return false
		}
		pattern = pattern[1:]
		path = path[1:]
	}
	return len(path) == 0
}

func splitLinesBytes(payload []byte) [][]byte {
	if len(payload) == 0 {
		return nil
	}
	parts := bytes.Split(payload, []byte{'\n'})
	if len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	return parts
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
	if contextLines < 0 {
		contextLines = 0
	}
	if start < 1 {
		start = 1
	}
	if end < start {
		end = start
	}
	if start > len(lines) {
		return ""
	}
	if end > len(lines) {
		end = len(lines)
	}
	lower := max(1, start-contextLines)
	upper := min(len(lines), end+contextLines)
	if lower > upper {
		return ""
	}
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
	value, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("%s required", key)
	}
	return parseInteger(value, key)
}

func optionalIntArg(args map[string]any, key string) (int, bool, error) {
	value, ok := args[key]
	if !ok || value == nil {
		return 0, false, nil
	}
	parsed, err := parseInteger(value, key)
	if err != nil {
		return 0, true, err
	}
	return parsed, true, nil
}

func optionalBoolArg(args map[string]any, key string) (bool, bool, error) {
	value, ok := args[key]
	if !ok || value == nil {
		return false, false, nil
	}
	parsed, ok := value.(bool)
	if !ok {
		return false, true, fmt.Errorf("%s must be a boolean", key)
	}
	return parsed, true, nil
}

func optionalStringArg(args map[string]any, key string) (string, bool, error) {
	value, ok := args[key]
	if !ok || value == nil {
		return "", false, nil
	}
	parsed, ok := value.(string)
	if !ok {
		return "", true, fmt.Errorf("%s must be a string", key)
	}
	return parsed, true, nil
}

func requiredStringArg(args map[string]any, key string) (string, error) {
	value, ok := args[key]
	if !ok || value == nil {
		return "", fmt.Errorf("%s required", key)
	}
	parsed, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return parsed, nil
}

func parseInteger(value any, key string) (int, error) {
	switch typed := value.(type) {
	case int:
		return typed, nil
	case float64:
		parsed, err := util.ParseIntegerFloat(typed, key)
		if err != nil {
			return 0, err
		}
		return parsed, nil
	case json.Number:
		parsed, err := numconv.IntFromJSONNumber(typed, key)
		if err != nil {
			return 0, err
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("%s must be an integer", key)
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

// min and max are int-specific helpers. Go 1.21+ has built-in generic
// min/max, but this project declares go 1.20 for compatibility, so we
// keep these local versions.
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
			"path": nonEmptyStringSchema(),
		},
		"required": []string{"path"},
	}
}

func schemaPathWithText() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"path": nonEmptyStringSchema(),
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
			"src": nonEmptyStringSchema(),
			"dst": nonEmptyStringSchema(),
		},
		"required": []string{"src", "dst"},
	}
}

func schemaSearch() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"path":          nonEmptyStringSchema(),
			"query":         nonEmptyStringSchema(),
			"limit":         map[string]any{"type": "integer"},
			"regex":         map[string]any{"type": "boolean"},
			"use_gitignore": map[string]any{"type": "boolean"},
		},
		"required": []string{"path", "query"},
	}
}

func schemaFindFiles() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"path":          nonEmptyStringSchema(),
			"pattern":       nonEmptyStringSchema(),
			"limit":         map[string]any{"type": "integer"},
			"use_gitignore": map[string]any{"type": "boolean"},
		},
		"required": []string{"path", "pattern"},
	}
}

func schemaReplaceText() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"path":                  nonEmptyStringSchema(),
			"old_text":              nonEmptyStringSchema(),
			"new_text":              map[string]any{"type": "string"},
			"replace_all":           map[string]any{"type": "boolean"},
			"expected_replacements": map[string]any{"type": "integer", "minimum": 0},
		},
		"required": []string{"path", "old_text", "new_text"},
	}
}

func schemaEditLines() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"path":              nonEmptyStringSchema(),
			"start_line":        map[string]any{"type": "integer", "minimum": 1},
			"end_line":          map[string]any{"type": "integer", "minimum": 1},
			"new_text":          map[string]any{"type": "string"},
			"expected_old_text": map[string]any{"type": "string"},
			"context_lines":     map[string]any{"type": "integer", "minimum": 0},
		},
		"required": []string{"path", "start_line", "end_line", "new_text"},
	}
}

func nonEmptyStringSchema() map[string]any {
	return map[string]any{"type": "string", "minLength": 1}
}
