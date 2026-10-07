package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/v2up-32mb/mcp-tools/internal/util"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/v2up-32mb/mcp-tools/internal/applog"
	"github.com/v2up-32mb/mcp-tools/internal/audit"
	"github.com/v2up-32mb/mcp-tools/internal/numconv"
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
	Host            string
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
	started := time.Now()
	r.mu.RLock()
	t, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		err := fmt.Errorf("%w: %s", ErrUnknownTool, name)
		auditData := AuditData{Allowed: false, ResultDigest: "unknown tool"}
		event := audit.Event{
			Timestamp:       started.UTC(),
			RequestID:       callCtx.RequestID,
			SessionID:       callCtx.SessionID,
			ProtocolVersion: callCtx.ProtocolVersion,
			RemoteAddr:      callCtx.RemoteAddr,
			Tool:            name,
			Arguments:       summarizeAuditArguments(name, args),
			Allowed:         auditData.Allowed,
			Success:         false,
			Error:           err.Error(),
			DurationMS:      time.Since(started).Milliseconds(),
			ResultDigest:    auditData.ResultDigest,
		}
		_ = r.auditor.Write(event)
		logToolCallError(callCtx, name, args, event.DurationMS, &ToolError{Err: err, Audit: auditData})
		return Result{}, err
	}

	result, callErr := t.Call(ctx, callCtx, args)
	event := audit.Event{
		Timestamp:       started.UTC(),
		RequestID:       callCtx.RequestID,
		SessionID:       callCtx.SessionID,
		ProtocolVersion: callCtx.ProtocolVersion,
		RemoteAddr:      callCtx.RemoteAddr,
		Tool:            name,
		Arguments:       summarizeAuditArguments(name, args),
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
		event.EnvKeys = util.CloneStrings(toolErr.Audit.EnvKeys)
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
	event.EnvKeys = util.CloneStrings(result.Audit.EnvKeys)
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
	auditData.ResultDigest = util.FirstNonEmpty(auditData.ResultDigest, "error")
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
	result.Audit.ResultDigest = util.FirstNonEmpty(result.Audit.ResultDigest, summarizeStructured(result.StructuredContent), "ok")
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
	words := strings.Fields(replacer.Replace(name))
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
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

func logToolCallInfo(name string, args map[string]any, durationMS int64, auditData AuditData) {
	fields := summarizeToolFields(name, args)
	fields = append(fields,
		applog.Field{Key: "status", Value: "ok"},
		applog.Field{Key: "duration_ms", Value: durationMS},
	)
	if summary := util.FirstNonEmpty(auditData.ResultDigest, name+" call"); summary != "" {
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
	case strings.HasPrefix(name, "fs_"):
		fields = appendPathLikeField(fields, args, "path", "path")
		fields = appendPathLikeField(fields, args, "target_path", "target_path")
		fields = appendPathLikeField(fields, args, "uri", "uri")
		fields = appendLineRangeFields(fields, args)
		fields = appendLargeFieldSummaries(fields, args)
	case strings.HasPrefix(name, "git_"):
		fields = appendPathLikeField(fields, args, "repo_path", "repo_path")
		fields = appendStringField(fields, args, "branch", "branch")
		fields = appendPathsCountField(fields, args, "paths")
	case strings.HasPrefix(name, "exec_"):
		switch name {
		case "exec_run":
			fields = appendStringField(fields, args, "preset", "preset")
			fields = appendStringField(fields, args, "command", "command")
			fields = appendPathLikeField(fields, args, "workdir", "workdir")
			fields = appendArgsCountField(fields, args, "args")
		case "exec_run_template":
			fields = appendStringField(fields, args, "template", "template")
			fields = appendPathLikeField(fields, args, "workdir", "workdir")
		case "exec_shell":
			fields = appendStringField(fields, args, "command", "command")
			fields = appendPathLikeField(fields, args, "workdir", "workdir")
		case "exec_start_process":
			fields = appendStringField(fields, args, "command", "command")
			fields = appendPathLikeField(fields, args, "workdir", "workdir")
			fields = appendArgsCountField(fields, args, "args")
		case "exec_process_logs", "exec_stop_process", "exec_remove_process":
			fields = appendStringField(fields, args, "id", "process_id")
		}
	case strings.HasPrefix(name, "go_"):
		fields = appendPathLikeField(fields, args, "path", "path")
		fields = appendIntField(fields, args, "line", "line")
		fields = appendIntField(fields, args, "column", "column")
	}
	return fields
}

func summarizeAuditArguments(name string, args map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]any, len(args))
	for key, value := range args {
		switch key {
		case "content", "text", "old_text", "new_text", "expected_old_text", "diff":
			if text, ok := value.(string); ok {
				out[key] = summarizeLargeString(text)
				continue
			}
		case "env":
			if summarized, ok := summarizeEnvArgument(value); ok {
				out[key] = summarized
				continue
			}
		case "args":
			if strings.HasPrefix(name, "exec_") {
				if summarized, ok := summarizeSliceArgument(value); ok {
					out[key] = summarized
					continue
				}
			}
		}
		out[key] = value
	}
	return out
}

func summarizeLargeString(value string) map[string]any {
	sum := sha256.Sum256([]byte(value))
	return map[string]any{
		"redacted": true,
		"bytes":    len(value),
		"lines":    lineCount(value),
		"sha256":   fmt.Sprintf("%x", sum),
	}
}

func summarizeEnvArgument(value any) (map[string]any, bool) {
	var keys []string
	switch env := value.(type) {
	case map[string]any:
		keys = make([]string, 0, len(env))
		for key := range env {
			keys = append(keys, key)
		}
	case map[string]string:
		keys = make([]string, 0, len(env))
		for key := range env {
			keys = append(keys, key)
		}
	default:
		return nil, false
	}
	sort.Strings(keys)
	return map[string]any{"keys": keys, "count": len(keys)}, true
}

func summarizeSliceArgument(value any) (map[string]any, bool) {
	switch values := value.(type) {
	case []any:
		return map[string]any{"count": len(values)}, true
	case []string:
		return map[string]any{"count": len(values)}, true
	default:
		return nil, false
	}
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
		if parsed, ok := safeLogInt(value); ok {
			fields = append(fields, applog.Field{Key: fieldKey, Value: parsed})
		}
	case json.Number:
		if parsed, ok := safeLogJSONNumber(value); ok {
			fields = append(fields, applog.Field{Key: fieldKey, Value: parsed})
		}
	}
	return fields
}

func safeLogInt(value float64) (int, bool) {
	return numconv.IntFromFloat64OK(value)
}

func safeLogJSONNumber(value json.Number) (int, bool) {
	return numconv.IntFromJSONNumberOK(value)
}

func appendArgsCountField(fields []applog.Field, args map[string]any, argKey string) []applog.Field {
	if count, ok := sliceArgumentLen(args[argKey]); ok {
		fields = append(fields, applog.Field{Key: "args_count", Value: count})
	}
	return fields
}

func appendPathsCountField(fields []applog.Field, args map[string]any, argKey string) []applog.Field {
	if count, ok := sliceArgumentLen(args[argKey]); ok {
		fields = append(fields, applog.Field{Key: "paths_count", Value: count})
	}
	return fields
}

func sliceArgumentLen(value any) (int, bool) {
	switch values := value.(type) {
	case []any:
		return len(values), true
	case []string:
		return len(values), true
	default:
		return 0, false
	}
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
