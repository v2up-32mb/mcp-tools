package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
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

// auditArgsJSON 把审计事件 Arguments 序列化为 JSON 字符串，
// 验证脱敏断言面向真实落盘形态（JSONL 由 json.Marshal(Event) 生成）。
func auditArgsJSON(t *testing.T, ev audit.Event) string {
	t.Helper()
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal audit event: %v", err)
	}
	return string(payload)
}

// TestRegistryAuditRedactsEnvArguments 锁定 MED-1 修复：
// 带敏感 env（API_TOKEN=secret-value）调用经 Registry.Call 后，
// 序列化的审计 Arguments 不含明文值、env 值替换为掩码、key 保留。
// 覆盖 exec.shell / exec.start_process / exec.run(raw env) 三个 env 入口的统一 args["env"] 处理。
func TestRegistryAuditRedactsEnvArguments(t *testing.T) {
	for _, toolName := range []string{"exec.shell", "exec.start_process", "exec.run"} {
		t.Run(toolName, func(t *testing.T) {
			logger := &captureLogger{}
			registry := NewRegistry(logger)
			registry.Register(staticTool{
				name: toolName,
				result: TextResult("ok", map[string]any{"summary": "ok"}, AuditData{
					Allowed:      true,
					ResultDigest: "ok",
					EnvKeys:      []string{"API_TOKEN"},
				}),
			})

			args := map[string]any{
				"command": "echo",
				"workdir": "/tmp/project",
				"env": map[string]any{
					"API_TOKEN":     "secret-value",
					"DB_PASS":       "hunter2",
					"MCP_PLAIN_KEY": "visible-value",
				},
			}
			if _, err := registry.Call(context.Background(), CallContext{RequestID: "req-1"}, toolName, args); err != nil {
				t.Fatalf("registry call failed: %v", err)
			}
			if len(logger.events) != 1 {
				t.Fatalf("expected 1 audit event, got %d", len(logger.events))
			}

			payload := auditArgsJSON(t, logger.events[0])
			for _, secret := range []string{"secret-value", "hunter2", "visible-value"} {
				if strings.Contains(payload, secret) {
					t.Fatalf("expected env value %q NOT to appear in audit JSON: %s", secret, payload)
				}
			}
			if !strings.Contains(payload, redactedEnvMask) {
				t.Fatalf("expected redacted env mask %q in audit JSON: %s", redactedEnvMask, payload)
			}
			for _, key := range []string{"API_TOKEN", "DB_PASS", "MCP_PLAIN_KEY"} {
				if !strings.Contains(payload, key) {
					t.Fatalf("expected env key %q preserved in audit JSON: %s", key, payload)
				}
			}
			// 其他字段不受影响：command/workdir 原样保留
			if !strings.Contains(payload, "echo") || !strings.Contains(payload, "/tmp/project") {
				t.Fatalf("expected command/workdir preserved in audit JSON: %s", payload)
			}

			// 脱敏不污染调用方 args：工具收到的原始 args 仍是明文（否则进程 env 注入会失效）
			envArg, ok := args["env"].(map[string]any)
			if !ok || envArg["API_TOKEN"] != "secret-value" {
				t.Fatalf("expected caller args unchanged after redaction, got %#v", args["env"])
			}
		})
	}
}

// TestRegistryAuditRedactsEnvArgumentsOnError 锁定错误路径脱敏：
// 工具调用失败时审计事件同样经脱敏，敏感 env 不落盘。
func TestRegistryAuditRedactsEnvArgumentsOnError(t *testing.T) {
	logger := &captureLogger{}
	registry := NewRegistry(logger)
	registry.Register(staticTool{
		name: "exec.shell",
		err:  WrapToolError(errors.New("exec failed"), AuditData{Allowed: true, Workdir: "/tmp/project"}),
	})

	_, err := registry.Call(context.Background(), CallContext{RequestID: "req-1"}, "exec.shell", map[string]any{
		"command": "boom",
		"workdir": "/tmp/project",
		"env":     map[string]any{"API_TOKEN": "secret-value"},
	})
	if err != nil {
		t.Fatalf("registry call returned unexpected error: %v", err)
	}
	if len(logger.events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(logger.events))
	}
	payload := auditArgsJSON(t, logger.events[0])
	if strings.Contains(payload, "secret-value") {
		t.Fatalf("expected env value NOT to appear in error-path audit JSON: %s", payload)
	}
	if !strings.Contains(payload, redactedEnvMask) || !strings.Contains(payload, "API_TOKEN") {
		t.Fatalf("expected mask and env key in error-path audit JSON: %s", payload)
	}
	if !logger.events[0].Success {
		return // 错误路径事件按 success=false 记录即可
	}
	t.Fatalf("expected success=false on error path: %#v", logger.events[0])
}

// TestRegistryAuditRedactsConsoleLogPath 锁定控制台日志同一脱敏：
// logToolCallInfo/logToolCallError 收到的是脱敏副本，env 值不进控制台输出。
func TestRegistryAuditRedactsConsoleLogPath(t *testing.T) {
	var buf bytes.Buffer
	old := applog.Default()
	applog.SetDefault(applog.New(&buf, "DEBUG"))
	t.Cleanup(func() { applog.SetDefault(old) })

	logger := &captureLogger{}
	registry := NewRegistry(logger)
	registry.Register(staticTool{
		name: "exec.run",
		result: TextResult("ok", map[string]any{"summary": "ok"}, AuditData{
			Allowed:      true,
			ResultDigest: "ok",
		}),
	})

	if _, err := registry.Call(context.Background(), CallContext{RequestID: "req-1"}, "exec.run", map[string]any{
		"command": "echo",
		"workdir": "/tmp/project",
		"env":     map[string]any{"API_TOKEN": "secret-value"},
	}); err != nil {
		t.Fatalf("registry call failed: %v", err)
	}

	logs := buf.String()
	if strings.Contains(logs, "secret-value") {
		t.Fatalf("expected env value NOT to appear in console logs: %s", logs)
	}
}

// TestRedactedArgsLeavesNonEnvUntouched 验证无 env 或非 map env 时
// 脱敏函数原样返回 args，不产生额外分配、不影响其他参数。
func TestRedactedArgsLeavesNonEnvUntouched(t *testing.T) {
	plain := map[string]any{"command": "echo", "workdir": "/tmp"}
	if got := redactedArgs(plain); len(got) != 2 || got["command"] != "echo" {
		t.Fatalf("expected args without env returned as-is, got %#v", got)
	}
	// env 非 map（畸形输入）不脱敏也不 panic
	malformed := map[string]any{"command": "echo", "env": "not-a-map"}
	if got := redactedArgs(malformed); got["env"] != "not-a-map" {
		t.Fatalf("expected malformed env untouched, got %#v", got["env"])
	}
	// 空 env map 原样返回
	empty := map[string]any{"command": "echo", "env": map[string]any{}}
	if got := redactedArgs(empty); len(got["env"].(map[string]any)) != 0 {
		t.Fatalf("expected empty env untouched, got %#v", got["env"])
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
		name: "fs.apply_unified_diff",
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
	}, "fs.apply_unified_diff", map[string]any{
		"path": "/tmp/demo.go",
		"diff": "@@ -1 +1 @@\n-old\n+new\n",
	})
	if err != nil {
		t.Fatalf("registry call failed: %v", err)
	}

	logs := buf.String()
	for _, needle := range []string{
		"tool call completed",
		"tool: fs.apply_unified_diff",
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
		name: "exec.run",
		err:  WrapToolError(errors.New("exec go_test failed"), AuditData{Allowed: true, Workdir: "/tmp/project", ResultDigest: "exec failure"}),
	})

	_, err := registry.Call(context.Background(), CallContext{
		RequestID:  "req-2",
		SessionID:  "sess-2",
		RemoteAddr: "127.0.0.1",
	}, "exec.run", map[string]any{
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
		"tool: exec.run",
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
