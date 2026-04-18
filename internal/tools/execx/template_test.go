package execx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
)

func findTool(t *testing.T, cfg config.Config, name string) mcp.Tool {
	t.Helper()
	for _, tool := range NewTools(cfg) {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("tool %s not found", name)
	return nil
}

func TestRunTemplateRejectsUnknownTemplate(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
		CommandTimeout:   time.Second,
		CommandTemplates: map[string]config.CommandTemplate{},
	}
	tool := findTool(t, cfg, "exec.run_template")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"template": "missing",
		"workdir":  ".",
	})
	if err == nil {
		t.Fatal("expected unknown template error")
	}
}

func TestRunTemplateExecutesConfiguredCommand(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "go.mod")
	if err := os.WriteFile(target, []byte("module example.com/test\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		CommandTemplates: map[string]config.CommandTemplate{
			"gomod": {Command: []string{"go", "env", "GOMOD"}, Timeout: 5 * time.Second},
		},
	}
	tool := findTool(t, cfg, "exec.run_template")
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"template": "gomod",
		"workdir":  ".",
	})
	if err != nil {
		t.Fatalf("run_template failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
	if len(res.Content) == 0 || res.Content[0].Text == "" {
		t.Fatalf("expected stdout in result, got %#v", res)
	}
}

func TestRunTemplateTimeoutOverrideOnlyShortens(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		CommandTemplates: map[string]config.CommandTemplate{
			"gomod": {Command: []string{"go", "env", "GOMOD"}, Timeout: time.Second},
		},
	}
	tool := findTool(t, cfg, "exec.run_template")
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"template":             "gomod",
		"workdir":              ".",
		"timeout_override_sec": 10,
	})
	if err != nil {
		t.Fatalf("run_template failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
}
