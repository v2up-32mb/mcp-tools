package execx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
			"gomod": {Command: []string{"go", "env", "GOMOD"}, Category: "build", RequiresConfirmation: true, Timeout: 5 * time.Second},
		},
	}
	tool := findTool(t, cfg, "exec.run_template")
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"template": "gomod",
		"workdir":  ".",
		"confirm":  true,
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
	if res.StructuredContent["category"] != "build" || res.StructuredContent["destructive"] != false || res.StructuredContent["requires_confirmation"] != true {
		t.Fatalf("unexpected template metadata in result: %#v", res.StructuredContent)
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

func TestRunTemplateSchemaEnumeratesConfiguredTemplates(t *testing.T) {
	cfg := config.Config{CommandTemplates: map[string]config.CommandTemplate{
		"make_test":  {Command: []string{"make", "test"}, Category: "test", Destructive: false, RequiresConfirmation: false, Timeout: time.Second},
		"make_build": {Command: []string{"make", "build"}, Category: "build", Destructive: true, RequiresConfirmation: true, Timeout: time.Second},
	}}
	tool := findTool(t, cfg, "exec.run_template")
	schema := tool.Schema()
	props := schema["properties"].(map[string]any)
	templateProp := props["template"].(map[string]any)
	enum := templateProp["enum"].([]string)
	if len(enum) != 2 || enum[0] != "make_build" || enum[1] != "make_test" {
		t.Fatalf("unexpected template enum: %#v", enum)
	}
	meta := schema["x-template-metadata"].(map[string]any)
	makeBuild := meta["make_build"].(map[string]any)
	if makeBuild["category"] != "build" || makeBuild["destructive"] != true || makeBuild["requires_confirmation"] != true {
		t.Fatalf("unexpected template metadata in schema: %#v", makeBuild)
	}
}

func TestRunTemplateInjectsConfiguredEnv(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		CommandTemplates: map[string]config.CommandTemplate{
			"envdump": {Command: []string{"env"}, Env: map[string]string{"MCP_TEMPLATE_TEST": "hello"}, Timeout: 5 * time.Second},
		},
	}
	tool := findTool(t, cfg, "exec.run_template")
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"template": "envdump",
		"workdir":  ".",
	})
	if err != nil {
		t.Fatalf("run_template failed: %v", err)
	}
	if !strings.Contains(res.Content[0].Text, "MCP_TEMPLATE_TEST=hello") {
		t.Fatalf("expected injected env in output, got %#v", res.Content)
	}
}

func TestRunTemplateRejectsWorkdirOutsideTemplateScope(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		CommandTemplates: map[string]config.CommandTemplate{
			"pwd": {Command: []string{"pwd"}, AllowedWorkdirs: []string{"sub"}, Timeout: 5 * time.Second},
		},
	}
	tool := findTool(t, cfg, "exec.run_template")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"template": "pwd",
		"workdir":  ".",
	})
	if err == nil {
		t.Fatal("expected workdir scope error")
	}
}

func TestRunTemplateRequiresConfirmation(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		CommandTemplates: map[string]config.CommandTemplate{
			"cleanup": {Command: []string{"pwd"}, Category: "cleanup", Destructive: true, RequiresConfirmation: true, Timeout: 5 * time.Second},
		},
	}
	tool := findTool(t, cfg, "exec.run_template")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"template": "cleanup",
		"workdir":  ".",
	})
	if err == nil {
		t.Fatal("expected confirmation required error")
	}
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"template": "cleanup",
		"workdir":  ".",
		"confirm":  true,
	})
	if err != nil {
		t.Fatalf("expected confirmed execution to pass, got %v", err)
	}
	if res.StructuredContent["confirm"] != true {
		t.Fatalf("expected confirm flag in result, got %#v", res.StructuredContent)
	}
}
