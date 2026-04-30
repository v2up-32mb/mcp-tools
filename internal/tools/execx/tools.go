package execx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	return "Run either a predefined Go toolchain preset or, when unsafe_allow_all is enabled, an arbitrary command inside the requested working directory. workdir is required; preset mode keeps the existing Go preset rules, while raw mode accepts command + args + optional env and gives the MCP client full local command execution."
}
func (configuredTool) ReadOnly() bool { return false }
func (configuredTool) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"preset":               map[string]any{"type": "string"},
			"command":              map[string]any{"type": "string"},
			"workdir":              map[string]any{"type": "string"},
			"args":                 map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"env":                  map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			"timeout_override_sec": map[string]any{"type": "integer"},
		},
		"required": []string{"workdir"},
	}
}

func (templateTool) Name() string { return "exec.run_template" }
func (t templateTool) Description() string {
	names := sortedTemplateNames(t.cfg.CommandTemplates)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		tpl := t.cfg.CommandTemplates[name]
		parts = append(parts, fmt.Sprintf("%s(category=%s,destructive=%t,requires_confirmation=%t,read_only=%t)", name, firstNonEmpty(tpl.Category, "uncategorized"), tpl.Destructive, tpl.RequiresConfirmation, tpl.ReadOnly))
	}
	detail := strings.Join(parts, ", ")
	if detail == "" {
		detail = "no templates configured"
	}
	return "Run a configured command template inside an allowed working directory. The template provides a fixed argv; clients may only choose template name, workdir, and an optional timeout override that can only shorten execution. Available templates: " + detail + "."
}
func (templateTool) ReadOnly() bool { return false }
func (t templateTool) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"template": map[string]any{
				"type":        "string",
				"enum":        sortedTemplateNames(t.cfg.CommandTemplates),
				"description": templateSummaryForSchema(t.cfg.CommandTemplates),
			},
			"workdir":              map[string]any{"type": "string"},
			"confirm":              map[string]any{"type": "boolean"},
			"timeout_override_sec": map[string]any{"type": "integer"},
		},
		"required":            []string{"template", "workdir"},
		"x-template-metadata": summarizeTemplateMetadata(t.cfg.CommandTemplates),
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
	if err := validateTemplateWorkdir(t.cfg, template, resolvedWorkdir); err != nil {
		return mcp.Result{}, err
	}
	confirm, _ := args["confirm"].(bool)
	if template.RequiresConfirmation && !confirm && !t.cfg.UnsafeAllowAll {
		return mcp.Result{}, mcp.WrapToolError(fmt.Errorf("template %q requires confirmation; set confirm=true to execute", templateName), mcp.AuditData{Workdir: resolvedWorkdir, Allowed: true, ResultDigest: "confirmation required"})
	}
	argv := cloneStrings(template.Command)
	timeout := applyTimeoutOverride(template.Timeout, args["timeout_override_sec"])
	extra := map[string]any{
		"category":              template.Category,
		"destructive":           template.Destructive,
		"requires_confirmation": template.RequiresConfirmation,
	}
	if len(template.AllowedWorkdirs) > 0 {
		extra["allowed_workdirs"] = cloneStrings(template.AllowedWorkdirs)
	}
	if confirm {
		extra["confirm"] = true
	}
	return runExecCommand(ctx, t.cfg, templateName, "template", argv, resolvedWorkdir, rawWorkdir, timeout, template.Env, template.ReadOnly, extra)
}

func (t configuredTool) Call(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
	if t.cfg.UnsafeAllowAll {
		if commandName, ok := args["command"].(string); ok && strings.TrimSpace(commandName) != "" {
			return t.callRawCommand(ctx, args, commandName)
		}
	}
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
	timeout := applyTimeoutOverride(t.cfg.CommandTimeout, args["timeout_override_sec"])
	argv := append([]string{commandName}, rawArgs...)
	return runExecCommand(ctx, t.cfg, commandName, "raw", argv, resolvedWorkdir, rawWorkdir, timeout, env, false, map[string]any{
		"mode":    "raw",
		"command": commandName,
	})
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
	var resolvedWorkdir string
	var err error
	if cfg.UnsafeAllowAll {
		resolvedWorkdir, err = security.RequireExistingWorkdir(workdir)
	} else {
		resolvedWorkdir, err = security.RequireAllowedWorkdir(workdir, cfg.AllowedRoots)
	}
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

func runExecCommand(ctx context.Context, cfg config.Config, name string, mode string, argv []string, resolvedWorkdir string, rawWorkdir string, timeout time.Duration, fixedEnv map[string]string, readOnly bool, extraStructured map[string]any) (mcp.Result, error) {
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
	stdout := truncate(stdoutBuf.String(), cfg.OutputMaxBytes)
	stderr := truncate(stderrBuf.String(), cfg.OutputMaxBytes)
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
	if cfg.UnsafeAllowAll {
		return nil
	}
	if len(template.AllowedWorkdirs) == 0 {
		return nil
	}
	for _, candidate := range template.AllowedWorkdirs {
		base := candidate
		if !filepath.IsAbs(base) {
			base = filepath.Join(cfg.StartupDirectory, base)
		}
		allowedDir, err := security.RequireAllowedWorkdir(base, cfg.AllowedRoots)
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

func rawStringArgs(raw any) ([]string, error) {
	values, ok := raw.([]any)
	if !ok {
		return nil, nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("args must be non-empty strings")
		}
		out = append(out, text)
	}
	return out, nil
}

func rawEnv(raw any) (map[string]string, error) {
	values, ok := raw.(map[string]any)
	if !ok {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		if strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("env keys must be non-empty strings")
		}
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("env values must be strings")
		}
		out[key] = text
	}
	return out, nil
}

func mergeCommandEnv(base []string, fixed map[string]string) []string {
	if len(fixed) == 0 {
		return base
	}
	merged := cloneStrings(base)
	for key, value := range fixed {
		merged = append(merged, key+"="+value)
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
		parts = append(parts, fmt.Sprintf("%s(category=%s,destructive=%t,requires_confirmation=%t,read_only=%t)", name, firstNonEmpty(tpl.Category, "uncategorized"), tpl.Destructive, tpl.RequiresConfirmation, tpl.ReadOnly))
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
			entry["allowed_workdirs"] = cloneStrings(tpl.AllowedWorkdirs)
		}
		out[name] = entry
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
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
		if !isAllowedPositionalArg(presetName, arg) {
			return nil, fmt.Errorf("arg %q must stay within the working directory", arg)
		}
		if !presetAllowsPositionalArg(presetName) {
			return nil, fmt.Errorf("preset %s does not accept positional args", presetName)
		}
		out = append(out, normalizePositionalArg(presetName, arg))
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

func isAllowedPositionalArg(presetName string, arg string) bool {
	if isLocalTarget(arg) {
		return true
	}
	switch presetName {
	case "go_get":
		return isModuleSpec(arg)
	default:
		return false
	}
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
	if isLocalTarget(arg) {
		return filepath.ToSlash(filepath.Clean(arg))
	}
	return arg
}

func isModuleSpec(arg string) bool {
	if strings.TrimSpace(arg) == "" || strings.Contains(arg, "://") || strings.HasPrefix(arg, "-") || filepath.IsAbs(arg) {
		return false
	}
	if strings.ContainsAny(arg, " \t\r\n") {
		return false
	}
	modulePath := arg
	if name, _, ok := strings.Cut(arg, "@"); ok {
		modulePath = name
	}
	if modulePath == "" {
		return false
	}
	first, _, _ := strings.Cut(modulePath, "/")
	return strings.Contains(first, ".")
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
