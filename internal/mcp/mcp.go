package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/example/mcp-tools/internal/applog"
	"github.com/example/mcp-tools/internal/audit"
)

var ErrUnknownTool = errors.New("unknown tool")

type Tool interface {
	Name() string
	Description() string
	Schema() map[string]any
	Call(context.Context, CallContext, map[string]any) (Result, error)
	ReadOnly() bool
}

type CallContext struct {
	RemoteAddr      string
	RequestID       string
	SessionID       string
	ProtocolVersion string
}

type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type AuditData struct {
	TargetPath   string
	Workdir      string
	Allowed      bool
	Stdout       string
	Stderr       string
	EnvKeys      []string
	ExitCode     *int
	ResultDigest string
}

type Result struct {
	Content           []TextContent  `json:"content,omitempty"`
	StructuredContent map[string]any `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError"`
	Audit             AuditData      `json:"-"`
}

type ToolError struct {
	Err               error
	Audit             AuditData
	StructuredContent map[string]any
}

func (e *ToolError) Error() string {
	if e == nil || e.Err == nil {
		return "tool error"
	}
	return e.Err.Error()
}

func (e *ToolError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type Registry struct {
	mu      sync.RWMutex
	tools   map[string]Tool
	auditor audit.Logger
}

func NewRegistry(a audit.Logger) *Registry {
	return &Registry{tools: make(map[string]Tool), auditor: a}
}

func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
}

func (r *Registry) List() []map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)

	tools := make([]map[string]any, 0, len(names))
	for _, name := range names {
		t := r.tools[name]
		tools = append(tools, map[string]any{
			"name":        t.Name(),
			"title":       humanizeTitle(t.Name()),
			"description": t.Description(),
			"inputSchema": ensureSchema(t.Schema()),
			"annotations": map[string]any{
				"readOnlyHint":    t.ReadOnly(),
				"destructiveHint": !t.ReadOnly(),
				"idempotentHint":  t.ReadOnly(),
				"openWorldHint":   false,
			},
		})
	}
	return tools
}

func (r *Registry) Call(ctx context.Context, callCtx CallContext, name string, args map[string]any) (Result, error) {
	r.mu.RLock()
	t, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return Result{}, fmt.Errorf("%w: %s", ErrUnknownTool, name)
	}

	started := time.Now()
	result, callErr := t.Call(ctx, callCtx, args)
	event := audit.Event{
		Timestamp:       started.UTC(),
		RequestID:       callCtx.RequestID,
		SessionID:       callCtx.SessionID,
		ProtocolVersion: callCtx.ProtocolVersion,
		RemoteAddr:      callCtx.RemoteAddr,
		Tool:            name,
		Arguments:       args,
		DurationMS:      time.Since(started).Milliseconds(),
	}

	if callErr != nil {
		toolErr := extractToolError(callErr)
		event.Success = false
		event.Allowed = toolErr.Audit.Allowed
		event.TargetPath = toolErr.Audit.TargetPath
		event.Workdir = toolErr.Audit.Workdir
		event.Stdout = toolErr.Audit.Stdout
		event.Stderr = toolErr.Audit.Stderr
		event.EnvKeys = cloneStrings(toolErr.Audit.EnvKeys)
		event.ExitCode = toolErr.Audit.ExitCode
		event.Error = toolErr.Error()
		event.ResultDigest = toolErr.Audit.ResultDigest
		_ = r.auditor.Write(event)
		logToolCallError(callCtx, name, args, event.DurationMS, toolErr)
		return ErrorResultWithStructured(toolErr.Error(), toolErr.StructuredContent, toolErr.Audit), nil
	}

	event.Success = true
	event.Allowed = result.Audit.Allowed
	event.TargetPath = result.Audit.TargetPath
	event.Workdir = result.Audit.Workdir
	event.Stdout = result.Audit.Stdout
	event.Stderr = result.Audit.Stderr
	event.EnvKeys = cloneStrings(result.Audit.EnvKeys)
	event.ExitCode = result.Audit.ExitCode
	event.ResultDigest = result.Audit.ResultDigest
	_ = r.auditor.Write(event)
	logToolCallInfo(name, args, event.DurationMS, result.Audit)
	return normalizeResult(result), nil
}

func TextResult(text string, structured map[string]any, auditData AuditData) Result {
	return Result{
		Content:           []TextContent{{Type: "text", Text: text}},
		StructuredContent: structured,
		Audit:             auditData,
	}
}

func ErrorResult(message string, auditData AuditData) Result {
	return ErrorResultWithStructured(message, nil, auditData)
}

func ErrorResultWithStructured(message string, structured map[string]any, auditData AuditData) Result {
	auditData.ResultDigest = firstNonEmpty(auditData.ResultDigest, "error")
	merged := map[string]any{"error": message}
	for key, value := range structured {
		merged[key] = value
	}
	return Result{
		Content:           []TextContent{{Type: "text", Text: message}},
		StructuredContent: merged,
		IsError:           true,
		Audit:             auditData,
	}
}

func WrapToolError(err error, auditData AuditData) error {
	return WrapToolErrorWithStructured(err, auditData, nil)
}

func WrapToolErrorWithStructured(err error, auditData AuditData, structured map[string]any) error {
	if err == nil {
		return nil
	}
	return &ToolError{Err: err, Audit: auditData, StructuredContent: structured}
}

func normalizeResult(result Result) Result {
	if result.Content == nil && len(result.StructuredContent) > 0 {
		result.Content = []TextContent{{Type: "text", Text: "ok"}}
	}
	result.Audit.ResultDigest = firstNonEmpty(result.Audit.ResultDigest, summarizeStructured(result.StructuredContent), "ok")
	return result
}

func extractToolError(err error) *ToolError {
	var toolErr *ToolError
	if errors.As(err, &toolErr) {
		return toolErr
	}
	return &ToolError{Err: err, Audit: AuditData{Allowed: true, ResultDigest: "tool error"}}
}

func ensureSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return map[string]any{"type": "object", "additionalProperties": false}
	}
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
	if _, ok := schema["additionalProperties"]; !ok {
		schema["additionalProperties"] = false
	}
	return schema
}

func humanizeTitle(name string) string {
	replacer := strings.NewReplacer(".", " ", "_", " ", "-", " ")
	return strings.Title(replacer.Replace(name))
}

func summarizeStructured(structured map[string]any) string {
	if len(structured) == 0 {
		return ""
	}
	if summary, ok := structured["summary"].(string); ok && summary != "" {
		return summary
	}
	return "ok"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func logToolCallInfo(name string, args map[string]any, durationMS int64, auditData AuditData) {
	fields := summarizeToolFields(name, args)
	fields = append(fields,
		applog.Field{Key: "status", Value: "ok"},
		applog.Field{Key: "duration_ms", Value: durationMS},
	)
	if summary := firstNonEmpty(auditData.ResultDigest, summarizeArgs(name, args)); summary != "" {
		fields = append(fields, applog.Field{Key: "result_digest", Value: summary})
	}
	applog.Default().Info("mcp.tool", "tool call completed", fields...)
}

func logToolCallError(callCtx CallContext, name string, args map[string]any, durationMS int64, toolErr *ToolError) {
	fields := summarizeToolFields(name, args)
	fields = append(fields,
		applog.Field{Key: "status", Value: "error"},
		applog.Field{Key: "duration_ms", Value: durationMS},
	)
	if strings.TrimSpace(callCtx.RequestID) != "" {
		fields = append(fields, applog.Field{Key: "request_id", Value: callCtx.RequestID})
	}
	if strings.TrimSpace(callCtx.SessionID) != "" {
		fields = append(fields, applog.Field{Key: "session_id", Value: callCtx.SessionID})
	}
	fields = append(fields, applog.Field{Key: "error", Value: toolErr.Error()})
	applog.Default().Error("mcp.tool", "tool call failed", fields...)
}

func summarizeToolFields(name string, args map[string]any) []applog.Field {
	fields := []applog.Field{{Key: "tool", Value: name}}
	switch {
	case strings.HasPrefix(name, "fs."):
		fields = appendPathLikeField(fields, args, "path", "path")
		fields = appendPathLikeField(fields, args, "target_path", "target_path")
		fields = appendPathLikeField(fields, args, "uri", "uri")
		fields = appendLineRangeFields(fields, args)
		fields = appendLargeFieldSummaries(fields, args)
	case strings.HasPrefix(name, "git."):
		fields = appendPathLikeField(fields, args, "repo_path", "repo_path")
		fields = appendStringField(fields, args, "branch", "branch")
		fields = appendPathsCountField(fields, args, "paths")
	case name == "exec.run":
		fields = appendStringField(fields, args, "preset", "preset")
		fields = appendStringField(fields, args, "command", "command")
		fields = appendPathLikeField(fields, args, "workdir", "workdir")
		fields = appendArgsCountField(fields, args, "args")
	case name == "exec.run_template":
		fields = appendStringField(fields, args, "template", "template")
		fields = appendPathLikeField(fields, args, "workdir", "workdir")
	case strings.HasPrefix(name, "go."):
		fields = appendPathLikeField(fields, args, "path", "path")
		fields = appendIntField(fields, args, "line", "line")
		fields = appendIntField(fields, args, "column", "column")
	}
	return fields
}

func appendStringField(fields []applog.Field, args map[string]any, argKey, fieldKey string) []applog.Field {
	if value, ok := args[argKey].(string); ok && strings.TrimSpace(value) != "" {
		fields = append(fields, applog.Field{Key: fieldKey, Value: value})
	}
	return fields
}

func appendPathLikeField(fields []applog.Field, args map[string]any, argKey, fieldKey string) []applog.Field {
	return appendStringField(fields, args, argKey, fieldKey)
}

func appendIntField(fields []applog.Field, args map[string]any, argKey, fieldKey string) []applog.Field {
	switch value := args[argKey].(type) {
	case int:
		fields = append(fields, applog.Field{Key: fieldKey, Value: value})
	case int64:
		fields = append(fields, applog.Field{Key: fieldKey, Value: value})
	case float64:
		fields = append(fields, applog.Field{Key: fieldKey, Value: int(value)})
	}
	return fields
}

func appendArgsCountField(fields []applog.Field, args map[string]any, argKey string) []applog.Field {
	if values, ok := args[argKey].([]any); ok {
		fields = append(fields, applog.Field{Key: "args_count", Value: len(values)})
	}
	return fields
}

func appendPathsCountField(fields []applog.Field, args map[string]any, argKey string) []applog.Field {
	if values, ok := args[argKey].([]any); ok {
		fields = append(fields, applog.Field{Key: "paths_count", Value: len(values)})
	}
	return fields
}

func appendLineRangeFields(fields []applog.Field, args map[string]any) []applog.Field {
	fields = appendIntField(fields, args, "start_line", "start_line")
	fields = appendIntField(fields, args, "end_line", "end_line")
	return fields
}

func appendLargeFieldSummaries(fields []applog.Field, args map[string]any) []applog.Field {
	if content, ok := args["content"].(string); ok {
		fields = append(fields, applog.Field{Key: "content_bytes", Value: len(content)})
	}
	if newText, ok := args["new_text"].(string); ok {
		fields = append(fields, applog.Field{Key: "new_text_lines", Value: lineCount(newText)})
	}
	if diff, ok := args["diff"].(string); ok {
		fields = append(fields, applog.Field{Key: "diff_hunks", Value: countDiffHunks(diff)})
	}
	return fields
}

func lineCount(value string) int {
	if value == "" {
		return 0
	}
	return strings.Count(value, "\n") + 1
}

func countDiffHunks(diff string) int {
	if strings.TrimSpace(diff) == "" {
		return 0
	}
	count := 0
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "@@") {
			count++
		}
	}
	return count
}

func summarizeArgs(name string, args map[string]any) string {
	return fmt.Sprintf("%s call", name)
}
