package git

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/example/mcp-tools/internal/util"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
	"github.com/example/mcp-tools/internal/numconv"
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
		tool{name: "git_status", desc: "Run git status --short --branch. repo_path is optional; if omitted, the startup directory is used and must resolve to an allowed repository.", schema: schemaRepo(), readOnly: true, call: fixed(cfg, "status", []string{"status", "--short", "--branch"})},
		tool{name: "git_diff", desc: "Run git diff. Optional paths must be explicit repo-relative paths; absolute paths, .. traversal, and option-like paths are rejected.", schema: schemaPaths(false), readOnly: true, call: gitDiff(cfg)},
		tool{name: "git_log", desc: "Run git log --oneline with a bounded limit. limit defaults to 20 and is capped at 200.", schema: schemaLog(), readOnly: true, call: gitLog(cfg)},
		tool{name: "git_add", desc: "Run git add on explicit repo-relative paths only. paths is required.", schema: schemaPaths(true), call: gitAdd(cfg)},
		tool{name: "git_restore", desc: "Run git restore on explicit repo-relative paths only. paths is required.", schema: schemaPaths(true), call: gitRestore(cfg)},
		tool{name: "git_commit", desc: "Run git commit -m <message> in an allowed repository. message must be non-empty.", schema: schemaCommit(), call: gitCommit(cfg)},
		tool{name: "git_branch", desc: "List branches in an allowed repository. repo_path is optional and defaults to the startup directory.", schema: schemaRepo(), readOnly: true, call: fixed(cfg, "branch", []string{"branch"})},
		tool{name: "git_switch", desc: "Run git switch <branch>. branch cannot be empty, start with -, or contain whitespace.", schema: schemaBranch(), call: gitSwitch(cfg)},
		tool{name: "git_pull", desc: "Run git pull --ff-only in an allowed repository. No other pull mode is exposed.", schema: schemaRepo(), call: fixed(cfg, "pull", []string{"pull", "--ff-only"})},
	}
}

func gitDiff(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		repo, err := resolveRepo(ctx, cfg, args)
		if err != nil {
			return mcp.Result{}, err
		}
		argv := []string{"diff"}
		paths, err := repoRelativePaths(args["paths"])
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: repo, Allowed: true, ResultDigest: "validation failed"})
		}
		if len(paths) > 0 {
			argv = append(argv, "--")
			argv = append(argv, paths...)
		}
		return execGit(ctx, cfg, repo, "diff", argv)
	}
}

func gitLog(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		repo, err := resolveRepo(ctx, cfg, args)
		if err != nil {
			return mcp.Result{}, err
		}
		limit, err := parseLogLimit(args["limit"])
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: repo, Allowed: true, ResultDigest: "validation failed"})
		}
		return execGit(ctx, cfg, repo, "log", []string{"log", "--oneline", fmt.Sprintf("-%d", limit)})
	}
}

func gitAdd(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		repo, err := resolveRepo(ctx, cfg, args)
		if err != nil {
			return mcp.Result{}, err
		}
		paths, err := repoRelativePaths(args["paths"])
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: repo, Allowed: true, ResultDigest: "validation failed"})
		}
		if len(paths) == 0 {
			return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("paths required"), mcp.AuditData{Workdir: repo, Allowed: true, ResultDigest: "validation failed"})
		}
		argv := append([]string{"add", "--"}, paths...)
		return execGit(ctx, cfg, repo, "add", argv)
	}
}

func gitRestore(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		repo, err := resolveRepo(ctx, cfg, args)
		if err != nil {
			return mcp.Result{}, err
		}
		paths, err := repoRelativePaths(args["paths"])
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: repo, Allowed: true, ResultDigest: "validation failed"})
		}
		if len(paths) == 0 {
			return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("paths required"), mcp.AuditData{Workdir: repo, Allowed: true, ResultDigest: "validation failed"})
		}
		argv := append([]string{"restore", "--"}, paths...)
		return execGit(ctx, cfg, repo, "restore", argv)
	}
}

func gitCommit(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		repo, err := resolveRepo(ctx, cfg, args)
		if err != nil {
			return mcp.Result{}, err
		}
		message, err := requiredStringArg(args, "message")
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: repo, Allowed: true, ResultDigest: "validation failed"})
		}
		return execGit(ctx, cfg, repo, "commit", []string{"commit", "-m", message})
	}
}

func gitSwitch(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		repo, err := resolveRepo(ctx, cfg, args)
		if err != nil {
			return mcp.Result{}, err
		}
		branch, err := requiredStringArg(args, "branch")
		if err != nil {
			return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: repo, Allowed: true, ResultDigest: "validation failed"})
		}
		if strings.TrimSpace(branch) == "" || strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, " \t\n") {
			return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("valid branch required"), mcp.AuditData{Workdir: repo, Allowed: true, ResultDigest: "validation failed"})
		}
		return execGit(ctx, cfg, repo, "switch", []string{"switch", branch})
	}
}

func fixed(cfg config.Config, sub string, argv []string) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		repo, err := resolveRepo(ctx, cfg, args)
		if err != nil {
			return mcp.Result{}, err
		}
		return execGit(ctx, cfg, repo, sub, argv)
	}
}

func requiredStringArg(args map[string]any, key string) (string, error) {
	value, ok := args[key]
	if !ok || value == nil {
		return "", fmt.Errorf("%s required", key)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s required", key)
	}
	return text, nil
}

func resolveRepo(ctx context.Context, cfg config.Config, args map[string]any) (string, error) {
	raw := cfg.StartupDirectory
	if value, ok := args["repo_path"]; ok && value != nil {
		text, ok := value.(string)
		if !ok {
			return "", mcp.WrapToolError(fmt.Errorf("repo_path must be a string"), mcp.AuditData{Allowed: true, ResultDigest: "validation failed"})
		}
		if strings.TrimSpace(text) != "" {
			raw = text
		}
	}
	candidate := raw
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(cfg.StartupDirectory, candidate)
	}
	var workdir string
	var err error
	if cfg.UnsafeAllowAll {
		workdir, err = security.RequireExistingWorkdir(candidate)
	} else {
		workdir, err = security.RequireAllowedWorkdir(candidate, cfg.AllowedRoots)
	}
	if err != nil {
		allowed := !errors.Is(err, security.ErrPathOutsideAllowedRoots)
		return "", mcp.WrapToolError(fmt.Errorf("repo_path: %w", err), mcp.AuditData{TargetPath: raw, Allowed: allowed, ResultDigest: "repo rejected"})
	}

	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = workdir
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", mcp.WrapToolError(fmt.Errorf("repo_path is not a git repository"), mcp.AuditData{Workdir: workdir, Allowed: true, Stderr: util.Truncate(string(output), cfg.OutputMaxBytes), ResultDigest: "repo discovery failed"})
	}
	repoRoot := strings.TrimSpace(string(output))
	if cfg.UnsafeAllowAll {
		repoRoot, err = security.RequireExistingWorkdir(repoRoot)
	} else {
		repoRoot, err = security.RequireAllowedWorkdir(repoRoot, cfg.AllowedRoots)
	}
	if err != nil {
		allowed := !errors.Is(err, security.ErrPathOutsideAllowedRoots)
		return "", mcp.WrapToolError(fmt.Errorf("repo root outside allowed roots"), mcp.AuditData{TargetPath: repoRoot, Allowed: allowed, ResultDigest: "repo rejected"})
	}
	return repoRoot, nil
}

func execGit(ctx context.Context, cfg config.Config, repo string, sub string, argv []string) (mcp.Result, error) {
	if !cfg.GitAllowed[sub] {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("git subcommand %q not allowed", sub), mcp.AuditData{Workdir: repo, Allowed: true, ResultDigest: "subcommand blocked"})
	}
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = repo
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	stdout := util.Truncate(stdoutBuf.String(), cfg.OutputMaxBytes)
	stderr := util.Truncate(stderrBuf.String(), cfg.OutputMaxBytes)
	auditData := mcp.AuditData{Workdir: repo, Allowed: true, Stdout: stdout, Stderr: stderr, ResultDigest: fmt.Sprintf("git %s", sub)}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code := exitErr.ExitCode()
			auditData.ExitCode = &code
		}
		message := strings.TrimSpace(stderr)
		if message == "" {
			message = err.Error()
		}
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("git %s failed: %s", sub, message), auditData)
	}
	summary := fmt.Sprintf("git %s ok", sub)
	auditData.ResultDigest = summary
	text := strings.TrimSpace(stdout)
	if text == "" {
		text = summary
	}
	return mcp.TextResult(text, map[string]any{
		"summary":   summary,
		"repo_path": repo,
		"stdout":    stdout,
		"stderr":    stderr,
	}, auditData), nil
}

func repoRelativePaths(raw any) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	var values []string
	switch typed := raw.(type) {
	case []string:
		values = typed
	case []any:
		values = make([]string, 0, len(typed))
		for _, value := range typed {
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("paths must be non-empty strings")
			}
			values = append(values, text)
		}
	default:
		return nil, fmt.Errorf("paths must be an array")
	}
	out := make([]string, 0, len(values))
	for _, text := range values {
		if strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("paths must be non-empty strings")
		}
		clean := filepath.Clean(text)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." || strings.HasPrefix(clean, "-") || strings.HasPrefix(clean, ":") {
			return nil, fmt.Errorf("path %q must be repo-relative", text)
		}
		out = append(out, filepath.ToSlash(clean))
	}
	return out, nil
}

func parseLogLimit(raw any) (int, error) {
	const (
		defaultLimit = 20
		maxLimit     = 200
	)
	if raw == nil {
		return defaultLimit, nil
	}
	var limit int
	switch value := raw.(type) {
	case int:
		limit = value
	case float64:
		parsed, err := util.ParseIntegerFloat(value, "limit")
		if err != nil {
			return 0, err
		}
		limit = parsed
	case json.Number:
		parsed, err := numconv.IntFromJSONNumber(value, "limit")
		if err != nil {
			return 0, err
		}
		limit = parsed
	default:
		return 0, fmt.Errorf("limit must be an integer")
	}
	if limit <= 0 {
		return 0, fmt.Errorf("limit must be > 0")
	}
	if limit > maxLimit {
		return maxLimit, nil
	}
	return limit, nil
}

func schemaRepo() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"repo_path": map[string]any{"type": "string"},
		},
	}
}

func schemaPaths(required bool) map[string]any {
	pathsSchema := map[string]any{
		"type":  "array",
		"items": map[string]any{"type": "string"},
	}
	if required {
		pathsSchema["minItems"] = 1
	}
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"repo_path": map[string]any{"type": "string"},
			"paths":     pathsSchema,
		},
	}
	if required {
		schema["required"] = []string{"paths"}
	}
	return schema
}

func schemaCommit() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"repo_path": map[string]any{"type": "string"},
			"message":   map[string]any{"type": "string", "minLength": 1},
		},
		"required": []string{"message"},
	}
}

func schemaBranch() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"repo_path": map[string]any{"type": "string"},
			"branch":    map[string]any{"type": "string", "minLength": 1},
		},
		"required": []string{"branch"},
	}
}

func schemaLog() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"repo_path": map[string]any{"type": "string"},
			"limit":     map[string]any{"type": "integer", "minimum": 1},
		},
	}
}
