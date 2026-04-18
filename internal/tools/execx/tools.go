package execx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
	"github.com/example/mcp-tools/internal/security"
)

var blockedFlags = map[string]bool{
	"-vettool": true,
}

var inlineValueAllowedFlags = map[string]bool{
	"-run":          true,
	"-count":        true,
	"-timeout":      true,
	"-tags":         true,
	"-o":            true,
	"-coverprofile": true,
	"-go":           true,
	"-compat":       true,
}

var inlinePathFlags = map[string]bool{
	"-o":            true,
	"-coverprofile": true,
}

type configuredTool struct {
	cfg config.Config
}

type templateTool struct {
	cfg config.Config
}

func NewTools(cfg config.Config) []mcp.Tool {
	return []mcp.Tool{configuredTool{cfg: cfg}, templateTool{cfg: cfg}}
}

func (configuredTool) Name() string { return "exec.run" }
func (configuredTool) Description() string {
	return "Run a predefined Go toolchain preset inside an allowed working directory. workdir is required; go_test/go_generate/go_build/go_vet default to ./... when no target is provided; go_mod_download and go_mod_tidy run directly in workdir; timeout_override_sec can only shorten the preset timeout; inline output paths are revalidated; -vettool is not supported."
}
func (configuredTool) ReadOnly() bool { return false }
func (configuredTool) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"preset":               map[string]any{"type": "string"},
			"workdir":              map[string]any{"type": "string"},
			"args":                 map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"timeout_override_sec": map[string]any{"type": "integer"},
		},
		"required": []string{"preset", "workdir"},
	}
}

func (templateTool) Name() string { return "exec.run_template" }
func (templateTool) Description() string {
	return "Run a configured command template inside an allowed working directory. The template provides a fixed argv; clients may only choose template name, workdir, and an optional timeout override that can only shorten execution."
}
func (templateTool) ReadOnly() bool { return false }
func (templateTool) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"template":             map[string]any{"type": "string"},
			"workdir":              map[string]any{"type": "string"},
			"timeout_override_sec": map[string]any{"type": "integer"},
		},
		"required": []string{"template", "workdir"},
	}
}

func (t templateTool) Call(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
	templateName, _ := args["template"].(string)
	template, ok := t.cfg.CommandTemplates[templateName]
	if !ok {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("template %q not allowed", templateName), mcp.AuditData{Allowed: true, ResultDigest: "template blocked"})
	}
	resolvedWorkdir, rawWorkdir, err := resolveWorkdir(t.cfg, args)
	if err != nil {
		return mcp.Result{}, err
	}
	argv := cloneStrings(template.Command)
	timeout := applyTimeoutOverride(template.Timeout, args["timeout_override_sec"])
	return runExecCommand(ctx, t.cfg, templateName, "template", argv, resolvedWorkdir, rawWorkdir, timeout)
}

func (t configuredTool) Call(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
	presetName, _ := args["preset"].(string)
	preset, ok := t.cfg.ExecPresets[presetName]
	if !ok {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("preset %q not allowed", presetName), mcp.AuditData{Allowed: true, ResultDigest: "preset blocked"})
	}

	resolvedWorkdir, rawWorkdir, err := resolveWorkdir(t.cfg, args)
	if err != nil {
		return mcp.Result{}, err
	}

	extraArgs, err := validateArgs(presetName, preset, args["args"])
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: resolvedWorkdir, Allowed: true, ResultDigest: "validation failed"})
	}
	argv := append(append([]string{}, preset.FixedArgs...), extraArgs...)
	if len(extraArgs) == 0 {
		argv = append(argv, defaultTargetsForPreset(presetName)...)
	}

	timeout := applyTimeoutOverride(preset.Timeout, args["timeout_override_sec"])
	fullArgv := append([]string{preset.Command}, argv...)
	return runExecCommand(ctx, t.cfg, presetName, "preset", fullArgv, resolvedWorkdir, rawWorkdir, timeout)
}

func resolveWorkdir(cfg config.Config, args map[string]any) (string, string, error) {
	rawWorkdir, _ := args["workdir"].(string)
	if strings.TrimSpace(rawWorkdir) == "" {
		return "", "", mcp.WrapToolError(fmt.Errorf("workdir required"), mcp.AuditData{Allowed: true, ResultDigest: "validation failed"})
	}
	workdir := rawWorkdir
	if !filepath.IsAbs(workdir) {
		workdir = filepath.Join(cfg.StartupDirectory, workdir)
	}
	resolvedWorkdir, err := security.RequireAllowedWorkdir(workdir, cfg.AllowedRoots)
	if err != nil {
		return "", rawWorkdir, mcp.WrapToolError(fmt.Errorf("workdir: %w", err), mcp.AuditData{TargetPath: rawWorkdir, Allowed: false, ResultDigest: "workdir rejected"})
	}
	return resolvedWorkdir, rawWorkdir, nil
}

func applyTimeoutOverride(timeout time.Duration, raw any) time.Duration {
	if value, ok := raw.(float64); ok && value > 0 {
		override := time.Duration(int(value)) * time.Second
		if override < timeout {
			return override
		}
	}
	if value, ok := raw.(int); ok && value > 0 {
		override := time.Duration(value) * time.Second
		if override < timeout {
			return override
		}
	}
	return timeout
}

func runExecCommand(ctx context.Context, cfg config.Config, name string, mode string, argv []string, resolvedWorkdir string, rawWorkdir string, timeout time.Duration) (mcp.Result, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = resolvedWorkdir
	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	stdout := truncate(stdoutBuf.String(), cfg.OutputMaxBytes)
	stderr := truncate(stderrBuf.String(), cfg.OutputMaxBytes)
	auditData := mcp.AuditData{Workdir: resolvedWorkdir, Allowed: true, Stdout: stdout, Stderr: stderr, ResultDigest: fmt.Sprintf("exec %s %s", mode, name)}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code := exitErr.ExitCode()
			auditData.ExitCode = &code
		}
		message := strings.TrimSpace(stderr)
		if message == "" {
			message = err.Error()
		}
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("exec %s %s failed: %s", mode, name, message), auditData)
	}
	summary := fmt.Sprintf("exec %s %s ok", mode, name)
	auditData.ResultDigest = summary
	text := strings.TrimSpace(stdout)
	if text == "" {
		text = summary
	}
	structured := map[string]any{
		"summary": summary,
		"workdir": resolvedWorkdir,
		"stdout":  stdout,
		"stderr":  stderr,
		"argv":    argv,
		mode:      name,
	}
	if strings.TrimSpace(rawWorkdir) != "" {
		structured["requested_workdir"] = rawWorkdir
	}
	return mcp.TextResult(text, structured, auditData), nil
}

func validateArgs(presetName string, preset config.ExecPreset, raw any) ([]string, error) {
	values, ok := raw.([]any)
	if !ok {
		return nil, nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		arg, ok := value.(string)
		if !ok || strings.TrimSpace(arg) == "" {
			return nil, fmt.Errorf("args must be non-empty strings")
		}
		if strings.HasPrefix(arg, "-") {
			name, inlineValue, hasInlineValue := splitFlagValue(arg)
			if blockedFlags[name] {
				return nil, fmt.Errorf("arg %q not allowed for preset %s", name, presetName)
			}
			if !isAllowedFlagName(name, preset.AllowedArgs) {
				return nil, fmt.Errorf("arg %q not allowed for preset %s", arg, presetName)
			}
			if hasInlineValue {
				if !inlineValueAllowedFlags[name] {
					return nil, fmt.Errorf("arg %q does not allow inline values", name)
				}
				if strings.TrimSpace(inlineValue) == "" {
					return nil, fmt.Errorf("arg %q requires a non-empty value", name)
				}
				if inlinePathFlags[name] {
					if !isLocalTarget(inlineValue) {
						return nil, fmt.Errorf("arg %q must stay within the working directory", arg)
					}
					inlineValue = filepath.ToSlash(filepath.Clean(inlineValue))
				}
				out = append(out, name+"="+inlineValue)
				continue
			}
			out = append(out, name)
			continue
		}
		if !isLocalTarget(arg) {
			return nil, fmt.Errorf("arg %q must stay within the working directory", arg)
		}
		out = append(out, filepath.ToSlash(filepath.Clean(arg)))
	}
	return out, nil
}

func defaultTargetsForPreset(preset string) []string {
	switch preset {
	case "go_test", "go_generate", "go_build", "go_vet":
		return []string{"./..."}
	default:
		return nil
	}
}

func isAllowedFlagName(arg string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if arg == prefix {
			return true
		}
	}
	return false
}

func splitFlagValue(arg string) (string, string, bool) {
	name, value, ok := strings.Cut(arg, "=")
	if !ok {
		return arg, "", false
	}
	return name, value, true
}

func isLocalTarget(arg string) bool {
	if strings.TrimSpace(arg) == "" || filepath.IsAbs(arg) || strings.Contains(arg, "://") || strings.HasPrefix(arg, "-") {
		return false
	}
	clean := filepath.Clean(arg)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "\n[truncated]"
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}
