package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example/mcp-tools/internal/applog"
	"github.com/example/mcp-tools/internal/audit"
)

type captureLogger struct {
	events []audit.Event
}

func (c *captureLogger) Write(ev audit.Event) error {
	c.events = append(c.events, ev)
	return nil
}

func (c *captureLogger) Close() error { return nil }

type staticTool struct {
	name   string
	result Result
	err    error
}

func (t staticTool) Name() string           { return t.name }
func (t staticTool) Description() string    { return "test" }
func (t staticTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (t staticTool) ReadOnly() bool         { return true }
func (t staticTool) Call(context.Context, CallContext, map[string]any) (Result, error) {
	return t.result, t.err
}

func TestRegistryAuditsUnknownToolCalls(t *testing.T) {
	logger := &captureLogger{}
	registry := NewRegistry(logger)

	_, err := registry.Call(context.Background(), CallContext{
		RequestID:       "req-missing",
		SessionID:       "sess-missing",
		ProtocolVersion: "2025-11-25",
		RemoteAddr:      "127.0.0.1",
	}, "missing.tool", map[string]any{"path": "/tmp/demo"})
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("expected ErrUnknownTool, got %v", err)
	}
	if len(logger.events) != 1 {
		t.Fatalf("expected 1 audit event for unknown tool, got %d", len(logger.events))
	}
	ev := logger.events[0]
	if ev.Tool != "missing.tool" || ev.Success || ev.Allowed {
		t.Fatalf("unexpected audit event for unknown tool: %#v", ev)
	}
	if ev.RequestID != "req-missing" || ev.SessionID != "sess-missing" || ev.ProtocolVersion != "2025-11-25" {
		t.Fatalf("audit event lost call context: %#v", ev)
	}
	if ev.ResultDigest != "unknown tool" {
		t.Fatalf("unexpected result digest: %q", ev.ResultDigest)
	}
}

func TestRegistryAuditIncludesEnvKeys(t *testing.T) {
	logger := &captureLogger{}
	registry := NewRegistry(logger)
	registry.Register(staticTool{
		name: "test.tool",
		result: TextResult("ok", map[string]any{"summary": "ok"}, AuditData{
			Allowed:      true,
			ResultDigest: "ok",
			EnvKeys:      []string{"GOCACHE", "GOMODCACHE"},
		}),
	})

	_, err := registry.Call(context.Background(), CallContext{
		RequestID:  "req-1",
		SessionID:  "sess-1",
		RemoteAddr: "127.0.0.1",
	}, "test.tool", map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("registry call failed: %v", err)
	}
	if len(logger.events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(logger.events))
	}
	if got := logger.events[0].EnvKeys; len(got) != 2 || got[0] != "GOCACHE" || got[1] != "GOMODCACHE" {
		t.Fatalf("unexpected env keys in audit event: %#v", got)
	}
	if logger.events[0].DurationMS < 0 || logger.events[0].Timestamp.After(time.Now().Add(time.Second)) {
		t.Fatalf("unexpected event timing: %#v", logger.events[0])
	}
}

func TestRegistryAuditSummarizesSensitiveArguments(t *testing.T) {
	logger := &captureLogger{}
	registry := NewRegistry(logger)
	registry.Register(staticTool{
		name: "exec_run",
		result: TextResult("ok", map[string]any{"summary": "ok"}, AuditData{
			Allowed:      true,
			ResultDigest: "ok",
		}),
	})

	_, err := registry.Call(context.Background(), CallContext{}, "exec_run", map[string]any{
		"command": "sh",
		"args":    []any{"-c", "printf %s \"$SECRET\""},
		"env": map[string]any{
			"SECRET": "do-not-log",
		},
		"text": "large or sensitive payload\n",
	})
	if err != nil {
		t.Fatalf("registry call failed: %v", err)
	}
	if len(logger.events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(logger.events))
	}
	args := logger.events[0].Arguments
	if args["text"].(map[string]any)["redacted"] != true {
		t.Fatalf("expected text argument to be summarized, got %#v", args["text"])
	}
	if strings.Contains(fmt.Sprint(args), "do-not-log") || strings.Contains(fmt.Sprint(args), "large or sensitive payload") {
		t.Fatalf("audit arguments leaked sensitive values: %#v", args)
	}
	env := args["env"].(map[string]any)
	if env["count"] != 1 {
		t.Fatalf("expected env summary count, got %#v", env)
	}
	execArgs := args["args"].(map[string]any)
	if execArgs["count"] != 2 {
		t.Fatalf("expected args summary count, got %#v", execArgs)
	}
}

func TestRegistryLogsToolCallInfoSummary(t *testing.T) {
	var buf bytes.Buffer
	old := applog.Default()
	applog.SetDefault(applog.New(&buf, "INFO"))
	t.Cleanup(func() { applog.SetDefault(old) })

	logger := &captureLogger{}
	registry := NewRegistry(logger)
	registry.Register(staticTool{
		name: "fs_apply_unified_diff",
		result: TextResult("ok", map[string]any{"summary": "updated file"}, AuditData{
			Allowed:      true,
			TargetPath:   "/tmp/demo.go",
			ResultDigest: "updated file",
		}),
	})

	_, err := registry.Call(context.Background(), CallContext{
		RequestID:  "req-1",
		SessionID:  "sess-1",
		RemoteAddr: "127.0.0.1",
	}, "fs_apply_unified_diff", map[string]any{
		"path": "/tmp/demo.go",
		"diff": "@@ -1 +1 @@\n-old\n+new\n",
	})
	if err != nil {
		t.Fatalf("registry call failed: %v", err)
	}

	logs := buf.String()
	for _, needle := range []string{
		"tool call completed",
		"tool: fs_apply_unified_diff",
		"path: /tmp/demo.go",
		"diff_hunks: 1",
		"result_digest: updated file",
	} {
		if !strings.Contains(logs, needle) {
			t.Fatalf("expected %q in logs, got %s", needle, logs)
		}
	}
	if strings.Contains(logs, "request_id:") || strings.Contains(logs, "session_id:") {
		t.Fatalf("did not expect request/session id in info logs, got %s", logs)
	}
}

func TestRegistryLogsToolCallErrorWithRequestAndSession(t *testing.T) {
	var buf bytes.Buffer
	old := applog.Default()
	applog.SetDefault(applog.New(&buf, "INFO"))
	t.Cleanup(func() { applog.SetDefault(old) })

	logger := &captureLogger{}
	registry := NewRegistry(logger)
	registry.Register(staticTool{
		name: "exec_run",
		err:  WrapToolError(errors.New("exec go_test failed"), AuditData{Allowed: true, Workdir: "/tmp/project", ResultDigest: "exec failure"}),
	})

	_, err := registry.Call(context.Background(), CallContext{
		RequestID:  "req-2",
		SessionID:  "sess-2",
		RemoteAddr: "127.0.0.1",
	}, "exec_run", map[string]any{
		"preset":  "go_test",
		"workdir": "/tmp/project",
		"args":    []any{"-run", "TestOne"},
	})
	if err != nil {
		t.Fatalf("registry call returned unexpected error: %v", err)
	}

	logs := buf.String()
	for _, needle := range []string{
		"tool call failed",
		"tool: exec_run",
		"preset: go_test",
		"workdir: /tmp/project",
		"args_count: 2",
		"request_id: req-2",
		"session_id: sess-2",
		"error: exec go_test failed",
	} {
		if !strings.Contains(logs, needle) {
			t.Fatalf("expected %q in logs, got %s", needle, logs)
		}
	}
}

func TestSummarizeToolFieldsCountsTypedStringSlices(t *testing.T) {
	execFields := summarizeToolFields("exec_run", map[string]any{
		"args": []string{"-run", "TestOne"},
	})
	if got, ok := fieldValue(execFields, "args_count").(int); !ok || got != 2 {
		t.Fatalf("expected args_count=2 for []string, got %#v", fieldValue(execFields, "args_count"))
	}

	gitFields := summarizeToolFields("git_diff", map[string]any{
		"paths": []string{"internal/mcp/mcp.go", "internal/mcp/mcp_test.go"},
	})
	if got, ok := fieldValue(gitFields, "paths_count").(int); !ok || got != 2 {
		t.Fatalf("expected paths_count=2 for []string, got %#v", fieldValue(gitFields, "paths_count"))
	}
}

func fieldValue(fields []applog.Field, key string) any {
	for _, field := range fields {
		if field.Key == key {
			return field.Value
		}
	}
	return nil
}

func TestSafeLogIntRejectsNonIntegralAndOutOfRangeValues(t *testing.T) {
	if got, ok := safeLogInt(42); !ok || got != 42 {
		t.Fatalf("expected 42 to parse, got %d ok=%t", got, ok)
	}
	if _, ok := safeLogInt(1.5); ok {
		t.Fatal("expected fractional value to be rejected")
	}
	if _, ok := safeLogInt(1e100); ok {
		t.Fatal("expected out-of-range value to be rejected")
	}
	if got, ok := safeLogJSONNumber(json.Number("42")); !ok || got != 42 {
		t.Fatalf("expected JSON number 42 to parse, got %d ok=%t", got, ok)
	}
	if _, ok := safeLogJSONNumber(json.Number("9007199254740992.5")); ok {
		t.Fatal("expected fractional JSON number to be rejected")
	}
}

func TestRegistryListFormatsToolEntry(t *testing.T) {
	logger := &captureLogger{}
	registry := NewRegistry(logger)
	registry.Register(staticTool{name: "fs_read_file", result: TextResult("ok", nil, AuditData{Allowed: true, ResultDigest: "ok"})})

	tools := registry.List()
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	entry := tools[0]
	if entry["name"] != "fs_read_file" {
		t.Fatalf("unexpected name: %#v", entry["name"])
	}
	title, ok := entry["title"].(string)
	if !ok || title == "" {
		t.Fatalf("expected humanized title, got %#v", entry["title"])
	}
	if !strings.Contains(title, "Read") {
		t.Fatalf("expected humanized title to contain a Read word, got %q", title)
	}
	schema, ok := entry["inputSchema"].(map[string]any)
	if !ok {
		t.Fatalf("expected inputSchema, got %#v", entry["inputSchema"])
	}
	if schema["type"] != "object" {
		t.Fatalf("expected schema type=object, got %#v", schema["type"])
	}
	if additional, ok := schema["additionalProperties"].(bool); !ok || additional {
		t.Fatalf("expected additionalProperties=false, got %#v", schema["additionalProperties"])
	}
	annotations, ok := entry["annotations"].(map[string]any)
	if !ok {
		t.Fatalf("expected annotations, got %#v", entry["annotations"])
	}
	for _, key := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
		if _, ok := annotations[key]; !ok {
			t.Fatalf("expected annotation %q, got %#v", key, annotations)
		}
	}
	if annotations["readOnlyHint"] != true {
		t.Fatalf("expected readOnlyHint=true for read-only tool, got %#v", annotations["readOnlyHint"])
	}
	if annotations["destructiveHint"] != false {
		t.Fatalf("expected destructiveHint=false for read-only tool, got %#v", annotations["destructiveHint"])
	}
	if annotations["idempotentHint"] != true {
		t.Fatalf("expected idempotentHint=true for read-only tool, got %#v", annotations["idempotentHint"])
	}
	if annotations["openWorldHint"] != false {
		t.Fatalf("expected openWorldHint=false, got %#v", annotations["openWorldHint"])
	}
}

func TestRegistryCallWrapsPlainErrorAsIsErrorResult(t *testing.T) {
	logger := &captureLogger{}
	registry := NewRegistry(logger)
	registry.Register(staticTool{name: "test.boom", err: errors.New("boom")})

	res, err := registry.Call(context.Background(), CallContext{}, "test.boom", nil)
	if err != nil {
		t.Fatalf("expected plain tool error to be wrapped into result (err==nil), got err=%v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true, got %#v", res)
	}
	if !strings.Contains(res.Content[0].Text, "boom") {
		t.Fatalf("expected error text in content, got %#v", res.Content)
	}
}

func TestNormalizeResultFillsContentWhenStructuredOnly(t *testing.T) {
	// Simulates the tool-error path where StructuredContent is set but Content is left nil.
	result := normalizeResult(Result{StructuredContent: map[string]any{"x": 1}, IsError: true})
	if len(result.Content) == 0 {
		t.Fatal("expected normalizeResult to fill Content when StructuredContent is non-empty")
	}
	if text := result.Content[0].Text; strings.TrimSpace(text) == "" {
		t.Fatalf("expected non-empty Content text, got %q", text)
	}
	if result.Content[0].Type != "text" {
		t.Fatalf("expected Content type=text, got %q", result.Content[0].Type)
	}
	if !result.IsError {
		t.Fatal("normalizeResult must preserve IsError=true")
	}
}

func TestEnsureSchemaAddsDefaultsForNil(t *testing.T) {
	schema := ensureSchema(nil)
	if schema["type"] != "object" {
		t.Fatalf("expected type=object, got %#v", schema["type"])
	}
	if additional, ok := schema["additionalProperties"].(bool); !ok || additional {
		t.Fatalf("expected additionalProperties=false, got %#v", schema["additionalProperties"])
	}
}

func TestEnsureSchemaOnlyAddsAdditionalPropertiesWhenTypePresent(t *testing.T) {
	schema := ensureSchema(map[string]any{"type": "object"})
	if schema["type"] != "object" {
		t.Fatalf("expected existing type=object preserved, got %#v", schema["type"])
	}
	if additional, ok := schema["additionalProperties"].(bool); !ok || additional {
		t.Fatalf("expected additionalProperties=false to be added, got %#v", schema["additionalProperties"])
	}
	if _, ok := schema["type"]; !ok {
		t.Fatal("expected type key to still be present")
	}
}

func TestRegistryCallWrapsPlainErrorAuditsEvent(t *testing.T) {
	logger := &captureLogger{}
	registry := NewRegistry(logger)
	registry.Register(staticTool{name: "test.boom", err: errors.New("boom")})

	_, err := registry.Call(context.Background(), CallContext{}, "test.boom", nil)
	if err != nil {
		t.Fatalf("expected plain tool error to be wrapped into result (err==nil), got err=%v", err)
	}
	if len(logger.events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(logger.events))
	}
	ev := logger.events[0]
	if ev.Tool != "test.boom" {
		t.Fatalf("expected tool name test.boom, got %q", ev.Tool)
	}
	if ev.Success {
		t.Fatal("expected Success=false in audit event")
	}
	if !strings.Contains(ev.Error, "boom") {
		t.Fatalf("expected error to contain 'boom', got %q", ev.Error)
	}
}

func TestRegistryAuditsUnknownToolCallArguments(t *testing.T) {
	logger := &captureLogger{}
	registry := NewRegistry(logger)

	_, err := registry.Call(context.Background(), CallContext{}, "missing.tool", map[string]any{"path": "/tmp/demo"})
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("expected ErrUnknownTool, got %v", err)
	}
	if len(logger.events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(logger.events))
	}
	ev := logger.events[0]
	if ev.Arguments == nil {
		t.Fatal("expected Arguments to be populated in audit event for unknown tool")
	}
}

func TestRegistryConcurrentRegisterAndCall(t *testing.T) {
	logger := &captureLogger{}
	registry := NewRegistry(logger)

	var wg sync.WaitGroup
	const goroutines = 50

	// Concurrent Register
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			registry.Register(staticTool{
				name:   fmt.Sprintf("tool.%d", n),
				result: TextResult("ok", nil, AuditData{Allowed: true, ResultDigest: "ok"}),
			})
		}(i)
	}
	wg.Wait()

	// Concurrent Call after all registers
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := registry.Call(context.Background(), CallContext{}, fmt.Sprintf("tool.%d", n), nil)
			if err != nil {
				t.Errorf("call tool.%d failed: %v", n, err)
			}
		}(i)
	}
	wg.Wait()

	// Concurrent List
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tools := registry.List()
			if len(tools) != goroutines {
				t.Errorf("expected %d tools, got %d", goroutines, len(tools))
			}
		}()
	}
	wg.Wait()
}

// TestRegistryAuditSummarizesEnvForExecTools 锁定 unsafe 模式下新增 exec_* 工具
// 与 exec_run 共用同一套审计脱敏：env 只落 key 与 count，明文值不进入 JSONL 审计。
// 断言面向 json.Marshal(Event) 的真实落盘形态。
func TestRegistryAuditSummarizesEnvForExecTools(t *testing.T) {
	for _, toolName := range []string{"exec_shell", "exec_start_process", "exec_run"} {
		t.Run(toolName, func(t *testing.T) {
			logger := &captureLogger{}
			registry := NewRegistry(logger)
			registry.Register(staticTool{
				name: toolName,
				result: TextResult("ok", map[string]any{"summary": "ok"}, AuditData{
					Allowed:      true,
					ResultDigest: "ok",
				}),
			})

			args := map[string]any{
				"command": "echo",
				"workdir": "/tmp/project",
				"args":    []any{"a", "b"},
				"env": map[string]any{
					"DB_PASS":   "hunter2",
					"API_TOKEN": "secret-value",
				},
			}
			if _, err := registry.Call(context.Background(), CallContext{RequestID: "req-1"}, toolName, args); err != nil {
				t.Fatalf("registry call failed: %v", err)
			}
			if len(logger.events) != 1 {
				t.Fatalf("expected 1 audit event, got %d", len(logger.events))
			}

			payload, err := json.Marshal(logger.events[0])
			if err != nil {
				t.Fatalf("marshal audit event: %v", err)
			}
			for _, secret := range []string{"secret-value", "hunter2"} {
				if strings.Contains(string(payload), secret) {
					t.Fatalf("env value %q leaked into audit JSON: %s", secret, payload)
				}
			}

			env, ok := logger.events[0].Arguments["env"].(map[string]any)
			if !ok {
				t.Fatalf("expected env summary in audit arguments, got %#v", logger.events[0].Arguments)
			}
			if env["count"] != 2 {
				t.Fatalf("expected env count 2, got %#v", env)
			}
			keys, _ := env["keys"].([]string)
			if len(keys) != 2 || keys[0] != "API_TOKEN" || keys[1] != "DB_PASS" {
				t.Fatalf("expected sorted env keys preserved, got %#v", env["keys"])
			}

			// 脱敏副本不得污染调用方 args（否则真实进程注入的 env 会被替换掉）
			if args["env"].(map[string]any)["API_TOKEN"] != "secret-value" {
				t.Fatalf("caller args mutated by redaction: %#v", args["env"])
			}
		})
	}
}

// TestSummarizeToolFieldsCoversExecTools 锁定 summarizeToolFields 对新增
// exec_* 工具的字段摘要：command / workdir / process id / args count。
func TestSummarizeToolFieldsCoversExecTools(t *testing.T) {
	collect := func(name string, args map[string]any) map[string]any {
		got := map[string]any{}
		for _, f := range summarizeToolFields(name, args) {
			got[f.Key] = f.Value
		}
		return got
	}

	got := collect("exec_shell", map[string]any{"command": "make build", "workdir": "/tmp/p"})
	if got["tool"] != "exec_shell" || got["command"] != "make build" || got["workdir"] != "/tmp/p" {
		t.Fatalf("unexpected exec_shell fields: %#v", got)
	}

	got = collect("exec_start_process", map[string]any{"command": "go", "args": []any{"run", "."}})
	if got["args_count"] != 2 || got["command"] != "go" {
		t.Fatalf("unexpected exec_start_process fields: %#v", got)
	}

	got = collect("exec_stop_process", map[string]any{"id": "proc-1"})
	if got["process_id"] != "proc-1" {
		t.Fatalf("expected process_id proc-1, got %#v", got)
	}
}
