package mcp

import (
	"bytes"
	"context"
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
