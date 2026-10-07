package execx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/example/mcp-tools/internal/util"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
	"github.com/example/mcp-tools/internal/numconv"
	"github.com/example/mcp-tools/internal/security"
)

var blockedFlags = map[string]bool{
	"-vettool": true,
}

var inlineValueAllowedFlags = map[string]bool{
	"-run":          true,
	"-skip":         true,
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
	mgr := NewProcessManager()
	return []mcp.Tool{
		configuredTool{cfg: cfg},
		templateTool{cfg: cfg},
		startProcessTool{cfg: cfg, mgr: mgr},
		listProcessesTool{cfg: cfg, mgr: mgr},
		processLogsTool{cfg: cfg, mgr: mgr},
		stopProcessTool{cfg: cfg, mgr: mgr},
		removeProcessTool{cfg: cfg, mgr: mgr},
		shellTool{cfg: cfg},
	}
}

func (configuredTool) Name() string { return "exec_run" }
func (configuredTool) Description() string {
	return "Run either a predefined Go toolchain preset or, when unsafe_allow_all is enabled, an arbitrary command inside the requested working directory. workdir is required; preset mode keeps the existing Go preset rules, while raw mode accepts command + args + optional env and gives the MCP client full local command execution."
}
func (configuredTool) ReadOnly() bool { return false }
func (configuredTool) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"preset":               map[string]any{"type": "string"},
			"command":              map[string]any{"type": "string", "minLength": 1},
			"workdir":              map[string]any{"type": "string", "minLength": 1},
			"args":                 map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"env":                  map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			"timeout_override_sec": map[string]any{"type": "integer", "minimum": 1},
		},
		"required": []string{"workdir"},
	}
}

func (templateTool) Name() string { return "exec_run_template" }
func (t templateTool) Description() string {
	names := sortedTemplateNames(t.cfg.CommandTemplates)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		tpl := t.cfg.CommandTemplates[name]
		parts = append(parts, fmt.Sprintf("%s(category=%s,destructive=%t,requires_confirmation=%t,read_only=%t)", name, util.FirstNonEmpty(tpl.Category, "uncategorized"), tpl.Destructive, tpl.RequiresConfirmation, tpl.ReadOnly))
	}
	detail := strings.Join(parts, ", ")
	if detail == "" {
		detail = "no templates configured"
	}
	return "Run a configured command template inside an allowed working directory. The template provides a fixed argv; clients may only choose template name, workdir, an optional timeout override that can only shorten execution, and confirm=true when required. Available templates: " + detail + "."
}
func (templateTool) ReadOnly() bool { return false }
func (t templateTool) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"template": map[string]any{
				"type":        "string",
				"minLength":   1,
				"enum":        sortedTemplateNames(t.cfg.CommandTemplates),
				"description": templateSummaryForSchema(t.cfg.CommandTemplates),
			},
			"workdir":              map[string]any{"type": "string", "minLength": 1},
			"confirm":              map[string]any{"type": "boolean"},
			"timeout_override_sec": map[string]any{"type": "integer", "minimum": 1},
		},
		"required":            []string{"template", "workdir"},
		"x-template-metadata": summarizeTemplateMetadata(t.cfg.CommandTemplates),
	}
}

func (t templateTool) Call(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
	templateName, err := requiredExecStringArg(args, "template")
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Allowed: true, ResultDigest: "validation failed"})
	}
	template, ok := t.cfg.CommandTemplates[templateName]
	if !ok {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("template %q not allowed", templateName), mcp.AuditData{Allowed: true, ResultDigest: "template blocked"})
	}
	resolvedWorkdir, rawWorkdir, err := resolveWorkdir(t.cfg, args)
	if err != nil {
		return mcp.Result{}, err
	}
	if err := validateTemplateWorkdir(t.cfg, template, resolvedWorkdir); err != nil {
		return mcp.Result{}, err
	}
	confirm, _, err := optionalBoolArg(args, "confirm")
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: resolvedWorkdir, Allowed: true, ResultDigest: "validation failed"})
	}
	if template.RequiresConfirmation && !confirm {
		return mcp.Result{}, mcp.WrapToolErrorWithStructured(
			fmt.Errorf("template %q requires confirmation; set confirm=true to execute", templateName),
			mcp.AuditData{Workdir: resolvedWorkdir, Allowed: true, ResultDigest: "confirmation required"},
			map[string]any{"confirmation_required": true},
		)
	}
	argv := util.CloneStrings(template.Command)
	timeout, err := applyTimeoutOverride(template.Timeout, args["timeout_override_sec"])
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: resolvedWorkdir, Allowed: true, ResultDigest: "validation failed"})
	}
	extra := map[string]any{
		"category":              template.Category,
		"destructive":           template.Destructive,
		"requires_confirmation": template.RequiresConfirmation,
	}
	if len(template.AllowedWorkdirs) > 0 {
		extra["allowed_workdirs"] = util.CloneStrings(template.AllowedWorkdirs)
	}
	if confirm {
		extra["confirm"] = true
	}
	return runExecCommand(ctx, t.cfg, templateName, "template", argv, resolvedWorkdir, rawWorkdir, timeout, template.Env, template.ReadOnly, extra)
}

func (t configuredTool) Call(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
	commandName, commandSet, err := optionalExecStringArg(args, "command")
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Allowed: true, ResultDigest: "validation failed"})
	}
	if commandSet {
		if strings.TrimSpace(commandName) == "" {
			return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("command required"), mcp.AuditData{Allowed: true, ResultDigest: "validation failed"})
		}
		if !t.cfg.UnsafeAllowAll {
			return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("raw command not allowed"), mcp.AuditData{Allowed: false, ResultDigest: "raw command blocked"})
		}
		return t.callRawCommand(ctx, args, commandName)
	}
	presetName, presetSet, err := optionalExecStringArg(args, "preset")
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Allowed: true, ResultDigest: "validation failed"})
	}
	if !presetSet || strings.TrimSpace(presetName) == "" {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("preset required"), mcp.AuditData{Allowed: true, ResultDigest: "validation failed"})
	}
	preset, ok := t.cfg.ExecPresets[presetName]
	if !ok {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("preset %q not allowed", presetName), mcp.AuditData{Allowed: true, ResultDigest: "preset blocked"})
	}

	resolvedWorkdir, rawWorkdir, err := resolveWorkdir(t.cfg, args)
	if err != nil {
		return mcp.Result{}, err
	}

	validatedArgs, err := validateArgsForWorkdirDetailed(presetName, preset, args["args"], resolvedWorkdir)
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: resolvedWorkdir, Allowed: true, ResultDigest: "validation failed"})
	}
	extraArgs := validatedArgs.args
	argv := append(append([]string{}, preset.FixedArgs...), extraArgs...)
	if !validatedArgs.hasTarget {
		argv = append(argv, defaultTargetsForPreset(presetName)...)
	}

	timeout, err := applyTimeoutOverride(preset.Timeout, args["timeout_override_sec"])
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: resolvedWorkdir, Allowed: true, ResultDigest: "validation failed"})
	}
	fullArgv := append([]string{preset.Command}, argv...)
	return runExecCommand(ctx, t.cfg, presetName, "preset", fullArgv, resolvedWorkdir, rawWorkdir, timeout, preset.Env, preset.ReadOnly, nil)
}

func (t configuredTool) callRawCommand(ctx context.Context, args map[string]any, commandName string) (mcp.Result, error) {
	resolvedWorkdir, rawWorkdir, err := resolveWorkdir(t.cfg, args)
	if err != nil {
		return mcp.Result{}, err
	}
	rawArgs, err := rawStringArgs(args["args"])
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: resolvedWorkdir, Allowed: true, ResultDigest: "validation failed"})
	}
	env, err := rawEnv(args["env"])
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: resolvedWorkdir, Allowed: true, ResultDigest: "validation failed"})
	}
	timeout, err := applyTimeoutOverride(t.cfg.CommandTimeout, args["timeout_override_sec"])
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: resolvedWorkdir, Allowed: true, ResultDigest: "validation failed"})
	}
	argv := append([]string{commandName}, rawArgs...)
	return runExecCommand(ctx, t.cfg, commandName, "raw", argv, resolvedWorkdir, rawWorkdir, timeout, env, false, map[string]any{
		"mode":    "raw",
		"command": commandName,
	})
}

func resolveWorkdir(cfg config.Config, args map[string]any) (string, string, error) {
	rawWorkdir, err := requiredExecStringArg(args, "workdir")
	if err != nil {
		return "", "", mcp.WrapToolError(err, mcp.AuditData{Allowed: true, ResultDigest: "validation failed"})
	}
	workdir := rawWorkdir
	if !filepath.IsAbs(workdir) {
		workdir = filepath.Join(cfg.StartupDirectory, workdir)
	}
	var resolvedWorkdir string
	var resolveErr error
	if cfg.UnsafeAllowAll {
		resolvedWorkdir, resolveErr = security.RequireExistingWorkdir(workdir)
	} else {
		resolvedWorkdir, resolveErr = security.RequireAllowedWorkdir(workdir, cfg.AllowedRoots)
	}
	if resolveErr != nil {
		return "", rawWorkdir, mcp.WrapToolError(fmt.Errorf("workdir: %w", resolveErr), mcp.AuditData{TargetPath: rawWorkdir, Allowed: false, ResultDigest: "workdir rejected"})
	}
	return resolvedWorkdir, rawWorkdir, nil
}

func applyTimeoutOverride(timeout time.Duration, raw any) (time.Duration, error) {
	if raw == nil {
		return timeout, nil
	}
	var seconds int
	switch value := raw.(type) {
	case int:
		seconds = value
	case float64:
		parsed, err := util.ParseIntegerFloat(value, "timeout_override_sec")
		if err != nil {
			return 0, err
		}
		seconds = parsed
	case json.Number:
		parsed, err := numconv.IntFromJSONNumber(value, "timeout_override_sec")
		if err != nil {
			return 0, err
		}
		seconds = parsed
	default:
		return 0, fmt.Errorf("timeout_override_sec must be an integer")
	}
	if seconds <= 0 {
		return 0, fmt.Errorf("timeout_override_sec must be > 0")
	}
	if timeoutSeconds := timeout / time.Second; timeoutSeconds <= 0 || int64(seconds) > int64(timeoutSeconds) {
		return timeout, nil
	}
	override := time.Duration(seconds) * time.Second
	if override < timeout {
		return override, nil
	}
	return timeout, nil
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

func optionalExecStringArg(args map[string]any, key string) (string, bool, error) {
	value, ok := args[key]
	if !ok || value == nil {
		return "", false, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", true, fmt.Errorf("%s must be a string", key)
	}
	return text, true, nil
}

func requiredExecStringArg(args map[string]any, key string) (string, error) {
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

func runExecCommand(ctx context.Context, cfg config.Config, name string, mode string, argv []string, resolvedWorkdir string, rawWorkdir string, timeout time.Duration, fixedEnv map[string]string, readOnly bool, extraStructured map[string]any) (mcp.Result, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		auditData := mcp.AuditData{Workdir: resolvedWorkdir, Allowed: true, ResultDigest: fmt.Sprintf("exec %s %s validation failed", mode, name)}
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("exec %s %s command required", mode, name), auditData)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ensureManagedEnvDirs(fixedEnv); err != nil {
		auditData := mcp.AuditData{Workdir: resolvedWorkdir, Allowed: true, ResultDigest: fmt.Sprintf("exec %s %s env setup failed", mode, name)}
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("prepare exec %s %s env: %w", mode, name, err), auditData)
	}
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = resolvedWorkdir
	cmd.Env = mergeCommandEnv(os.Environ(), fixedEnv)
	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	stdout := util.Truncate(stdoutBuf.String(), cfg.OutputMaxBytes)
	stderr := util.Truncate(stderrBuf.String(), cfg.OutputMaxBytes)
	auditData := mcp.AuditData{Workdir: resolvedWorkdir, Allowed: true, Stdout: stdout, Stderr: stderr, ResultDigest: fmt.Sprintf("exec %s %s", mode, name)}
	if len(fixedEnv) > 0 {
		auditData.EnvKeys = sortedEnvKeys(fixedEnv)
	}
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
		"summary":   summary,
		"workdir":   resolvedWorkdir,
		"stdout":    stdout,
		"stderr":    stderr,
		"argv":      argv,
		"read_only": readOnly,
		mode:        name,
	}
	if len(fixedEnv) > 0 {
		structured["env_keys"] = sortedEnvKeys(fixedEnv)
	}
	if strings.TrimSpace(rawWorkdir) != "" {
		structured["requested_workdir"] = rawWorkdir
	}
	for key, value := range extraStructured {
		structured[key] = value
	}
	return mcp.TextResult(text, structured, auditData), nil
}

func validateTemplateWorkdir(cfg config.Config, template config.CommandTemplate, resolvedWorkdir string) error {
	if len(template.AllowedWorkdirs) == 0 {
		return nil
	}
	for _, candidate := range template.AllowedWorkdirs {
		base := candidate
		if !filepath.IsAbs(base) {
			base = filepath.Join(cfg.StartupDirectory, base)
		}
		var (
			allowedDir string
			err        error
		)
		if cfg.UnsafeAllowAll {
			allowedDir, err = security.RequireExistingWorkdir(base)
		} else {
			allowedDir, err = security.RequireAllowedWorkdir(base, cfg.AllowedRoots)
		}
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(allowedDir, resolvedWorkdir)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil
		}
	}
	return mcp.WrapToolError(fmt.Errorf("workdir not allowed for template"), mcp.AuditData{TargetPath: resolvedWorkdir, Allowed: false, ResultDigest: "workdir rejected"})
}

// rawStringArgs parses the args array for raw command mode. Unlike
// stringSliceArg (used for preset args), it allows empty strings because
// command arguments may legitimately be empty strings.
func rawStringArgs(raw any) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	switch values := raw.(type) {
	case []string:
		out := make([]string, len(values))
		copy(out, values)
		return out, nil
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("args must be strings")
			}
			out = append(out, text)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("args must be an array")
	}
}

func rawEnv(raw any) (map[string]string, error) {
	if raw == nil {
		return nil, nil
	}
	switch values := raw.(type) {
	case map[string]string:
		out := make(map[string]string, len(values))
		for key, value := range values {
			if err := util.ValidateEnvKey(key); err != nil {
				return nil, err
			}
			out[key] = value
		}
		return out, nil
	case map[string]any:
		out := make(map[string]string, len(values))
		for key, value := range values {
			if err := util.ValidateEnvKey(key); err != nil {
				return nil, err
			}
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("env values must be strings")
			}
			out[key] = text
		}
		return out, nil
	default:
		return nil, fmt.Errorf("env must be an object")
	}
}

func mergeCommandEnv(base []string, fixed map[string]string) []string {
	if len(fixed) == 0 {
		return base
	}
	merged := util.CloneStrings(base)
	for key, value := range fixed {
		prefix := key + "="
		// 先删除 base 中同 key 的旧条目，避免出现重复 KEY=value：
		// Windows 取最后一个值，但 POSIX execve 对重复 key 的语义未定义，
		// 部分实现取首个，会令 env 覆盖失效。
		kept := merged[:0]
		for _, entry := range merged {
			if strings.HasPrefix(entry, prefix) {
				continue
			}
			kept = append(kept, entry)
		}
		merged = append(kept, prefix+value)
	}
	return merged
}

func sortedTemplateNames(values map[string]config.CommandTemplate) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func sortedEnvKeys(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func templateSummaryForSchema(values map[string]config.CommandTemplate) string {
	names := sortedTemplateNames(values)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		tpl := values[name]
		parts = append(parts, fmt.Sprintf("%s(category=%s,destructive=%t,requires_confirmation=%t,read_only=%t)", name, util.FirstNonEmpty(tpl.Category, "uncategorized"), tpl.Destructive, tpl.RequiresConfirmation, tpl.ReadOnly))
	}
	return strings.Join(parts, "; ")
}

func summarizeTemplateMetadata(values map[string]config.CommandTemplate) map[string]any {
	out := make(map[string]any, len(values))
	for name, tpl := range values {
		entry := map[string]any{
			"category":              tpl.Category,
			"destructive":           tpl.Destructive,
			"requires_confirmation": tpl.RequiresConfirmation,
			"read_only":             tpl.ReadOnly,
		}
		if len(tpl.Env) > 0 {
			entry["env_keys"] = sortedEnvKeys(tpl.Env)
		}
		if len(tpl.AllowedWorkdirs) > 0 {
			entry["allowed_workdirs"] = util.CloneStrings(tpl.AllowedWorkdirs)
		}
		out[name] = entry
	}
	return out
}

func validateArgs(presetName string, preset config.ExecPreset, raw any) ([]string, error) {
	return validateArgsForWorkdir(presetName, preset, raw, "")
}

func validateArgsForWorkdir(presetName string, preset config.ExecPreset, raw any, resolvedWorkdir string) ([]string, error) {
	validated, err := validateArgsForWorkdirDetailed(presetName, preset, raw, resolvedWorkdir)
	if err != nil {
		return nil, err
	}
	return validated.args, nil
}

type validatedPresetArgs struct {
	args      []string
	hasTarget bool
}

func validateArgsForWorkdirDetailed(presetName string, preset config.ExecPreset, raw any, resolvedWorkdir string) (validatedPresetArgs, error) {
	values, err := stringSliceArg(raw, "args")
	if err != nil {
		return validatedPresetArgs{}, err
	}
	out := make([]string, 0, len(values))
	hasTarget := false
	for i := 0; i < len(values); i++ {
		arg := values[i]
		if strings.HasPrefix(arg, "-") {
			name, inlineValue, hasInlineValue := splitFlagValue(arg)
			if blockedFlags[name] {
				return validatedPresetArgs{}, fmt.Errorf("arg %q not allowed for preset %s", name, presetName)
			}
			if !isAllowedFlagName(name, preset.AllowedArgs) {
				return validatedPresetArgs{}, fmt.Errorf("arg %q not allowed for preset %s", arg, presetName)
			}
			if hasInlineValue {
				if !inlineValueAllowedFlags[name] {
					return validatedPresetArgs{}, fmt.Errorf("arg %q does not allow inline values", name)
				}
				if strings.TrimSpace(inlineValue) == "" {
					return validatedPresetArgs{}, fmt.Errorf("arg %q requires a non-empty value", name)
				}
				if inlinePathFlags[name] {
					normalized, err := normalizeInlinePathValue(inlineValue, resolvedWorkdir)
					if err != nil {
						return validatedPresetArgs{}, fmt.Errorf("arg %q must stay within the working directory", arg)
					}
					inlineValue = normalized
				}
				out = append(out, name+"="+inlineValue)
				continue
			}
			if inlineValueAllowedFlags[name] {
				if i+1 >= len(values) {
					return validatedPresetArgs{}, fmt.Errorf("arg %q requires a non-empty value", name)
				}
				separateValue := values[i+1]
				if strings.TrimSpace(separateValue) == "" {
					return validatedPresetArgs{}, fmt.Errorf("arg %q requires a non-empty value", name)
				}
				if inlinePathFlags[name] {
					normalized, err := normalizeInlinePathValue(separateValue, resolvedWorkdir)
					if err != nil {
						return validatedPresetArgs{}, fmt.Errorf("arg %q must stay within the working directory", arg)
					}
					separateValue = normalized
				}
				out = append(out, name, separateValue)
				i++
				continue
			}
			out = append(out, name)
			continue
		}
		if !isAllowedPositionalArg(presetName, arg) {
			return validatedPresetArgs{}, fmt.Errorf("arg %q must stay within the working directory", arg)
		}
		if !presetAllowsPositionalArg(presetName) {
			return validatedPresetArgs{}, fmt.Errorf("preset %s does not accept positional args", presetName)
		}
		if err := validatePositionalTargetWithinWorkdir(presetName, arg, resolvedWorkdir); err != nil {
			return validatedPresetArgs{}, fmt.Errorf("arg %q must stay within the working directory", arg)
		}
		hasTarget = true
		out = append(out, normalizePositionalArg(presetName, arg))
	}
	return validatedPresetArgs{args: out, hasTarget: hasTarget}, nil
}

func validatePositionalTargetWithinWorkdir(presetName string, arg string, resolvedWorkdir string) error {
	if strings.TrimSpace(resolvedWorkdir) == "" || !isLocalTarget(arg) {
		return nil
	}
	if presetName == "go_get" && isModuleSpec(arg) {
		return nil
	}
	candidate := filepath.Join(resolvedWorkdir, filepath.Clean(arg))
	_, err := security.ResolvePath(candidate, []string{resolvedWorkdir})
	return err
}

func normalizeInlinePathValue(value string, resolvedWorkdir string) (string, error) {
	if !isLocalTarget(value) {
		return "", fmt.Errorf("path must be local")
	}
	clean := filepath.Clean(value)
	if strings.TrimSpace(resolvedWorkdir) != "" {
		candidate := filepath.Join(resolvedWorkdir, clean)
		if _, err := security.ResolvePath(candidate, []string{resolvedWorkdir}); err != nil {
			return "", err
		}
	}
	return filepath.ToSlash(clean), nil
}

func stringSliceArg(raw any, name string) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	switch values := raw.(type) {
	case []string:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("%s must be non-empty strings", name)
			}
			out = append(out, value)
		}
		return out, nil
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return nil, fmt.Errorf("%s must be non-empty strings", name)
			}
			out = append(out, text)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s must be an array", name)
	}
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

func isAllowedPositionalArg(presetName string, arg string) bool {
	if isModuleSpec(arg) {
		return presetName == "go_get"
	}
	return isLocalTarget(arg)
}

func presetAllowsPositionalArg(presetName string) bool {
	switch presetName {
	case "go_mod_download", "go_mod_tidy", "go_work_sync":
		return false
	default:
		return true
	}
}

func normalizePositionalArg(presetName string, arg string) string {
	if presetName == "go_get" && isModuleSpec(arg) {
		return arg
	}
	if isLocalTarget(arg) {
		return normalizeLocalTarget(arg)
	}
	return arg
}

func normalizeLocalTarget(arg string) string {
	clean := filepath.ToSlash(filepath.Clean(arg))
	raw := filepath.ToSlash(arg)
	if clean == "." {
		return clean
	}
	if strings.HasPrefix(raw, "./") && !strings.HasPrefix(clean, "./") {
		return "./" + clean
	}
	if !strings.HasPrefix(clean, "./") {
		return "./" + clean
	}
	return clean
}

func isModuleSpec(arg string) bool {
	arg = strings.TrimSpace(arg)
	if arg == "" || strings.Contains(arg, "://") || strings.HasPrefix(arg, "-") || filepath.IsAbs(arg) {
		return false
	}
	if strings.ContainsAny(arg, " \t\r\n") {
		return false
	}
	if arg == "." || arg == ".." || strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../") {
		return false
	}
	modulePath := arg
	if name, _, ok := strings.Cut(arg, "@"); ok {
		modulePath = name
	}
	if modulePath == "" {
		return false
	}
	first, _, hasSlash := strings.Cut(modulePath, "/")
	return strings.Contains(first, ".") && (hasSlash || strings.Contains(arg, "@"))
}

func ensureManagedEnvDirs(fixedEnv map[string]string) error {
	for _, key := range []string{"GOCACHE", "GOMODCACHE", "GOTMPDIR"} {
		path := strings.TrimSpace(fixedEnv[key])
		if path == "" {
			continue
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// shellTool 执行 shell 命令字符串（支持管道、重定向、环境变量展开）。
type shellTool struct {
	cfg config.Config
}

func (shellTool) Name() string { return "exec_shell" }
func (shellTool) Description() string {
	return "Execute a shell command string (supports pipes, redirects, env expansion). Only available when unsafe_allow_all is enabled. Uses /bin/sh -c on Unix and cmd /C on Windows."
}
func (shellTool) ReadOnly() bool { return false }
func (shellTool) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"command":              map[string]any{"type": "string", "minLength": 1},
			"workdir":              map[string]any{"type": "string", "minLength": 1},
			"env":                  map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			"timeout_override_sec": map[string]any{"type": "integer"},
		},
		"required": []string{"command", "workdir"},
	}
}

func (t shellTool) Call(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
	if !t.cfg.UnsafeAllowAll {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("exec_shell requires unsafe_allow_all=true"), mcp.AuditData{Allowed: true, ResultDigest: "feature disabled"})
	}
	command, err := requiredExecStringArg(args, "command")
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Allowed: true, ResultDigest: "validation failed"})
	}
	resolvedWorkdir, rawWorkdir, err := resolveWorkdir(t.cfg, args)
	if err != nil {
		return mcp.Result{}, err
	}
	env, err := rawEnv(args["env"])
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: resolvedWorkdir, Allowed: true, ResultDigest: "validation failed"})
	}
	timeout, err := applyTimeoutOverride(t.cfg.CommandTimeout, args["timeout_override_sec"])
	if err != nil {
		return mcp.Result{}, mcp.WrapToolError(err, mcp.AuditData{Workdir: resolvedWorkdir, Allowed: true, ResultDigest: "validation failed"})
	}
	shell := "/bin/sh"
	flag := "-c"
	if runtime.GOOS == "windows" {
		shell = "cmd.exe"
		flag = "/C"
	}
	argv := []string{shell, flag, command}
	return runExecCommand(ctx, t.cfg, "shell", "shell", argv, resolvedWorkdir, rawWorkdir, timeout, env, false, map[string]any{
		"mode":    "shell",
		"command": command,
	})
}
