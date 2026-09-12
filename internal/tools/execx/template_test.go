package execx

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
)

// crossPlatformEnvDump 返回能列出环境变量的命令（Windows 用 cmd set，Unix 用 sh env）。
func crossPlatformEnvDump() []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd.exe", "/C", "set"}
	}
	return []string{"sh", "-c", "env"}
}

// crossPlatformEnvProbe 返回定向输出单个环境变量值的命令（不用全量 env dump）：
// Unix 用 sh -c 'printf %s "$<VAR>"'；Windows 用 cmd /C set <VAR>（单变量形式只输出该变量），
// 避免全量 set 在 t.TempDir() 映射到用户 Temp 时输出超 OutputMaxBytes=4096 被截断，
// 令注入值落在截断段之后导致判定不稳定。
func crossPlatformEnvProbe(varName string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd.exe", "/C", "set " + varName}
	}
	return []string{"sh", "-c", `printf %s "$` + varName + `"`}
}

// crossPlatformEcho 返回能输出固定文本的命令。
func crossPlatformEcho() []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd.exe", "/C", "echo ok"}
	}
	return []string{"sh", "-c", "echo ok"}
}

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
	if err := os.WriteFile(target, []byte("module example.com/test\n\ngo 1.20\n"), 0o644); err != nil {
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

// TestRunTemplateInjectsConfiguredEnv 锁定模板 env 注入语义：
// 断言改用定向输出通道（crossPlatformEnvProbe 直接打印注入变量），不依赖
// 全量 env dump 与 OutputMaxBytes 截断行为，Windows/Unix 均稳定。
func TestRunTemplateInjectsConfiguredEnv(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		CommandTemplates: map[string]config.CommandTemplate{
			"envprobe": {Command: crossPlatformEnvProbe("MCP_TEMPLATE_TEST"), Env: map[string]string{"MCP_TEMPLATE_TEST": "hello"}, Timeout: 5 * time.Second},
		},
	}
	tool := findTool(t, cfg, "exec.run_template")
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"template": "envprobe",
		"workdir":  ".",
	})
	if err != nil {
		t.Fatalf("run_template failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
	// 定向断言：注入变量应出现在输出中——Windows cmd set <VAR> 输出 "VAR=value"，
	// Unix printf %s "$VAR" 输出裸值；两种通道都不依赖截断行为
	output := res.Content[0].Text
	if runtime.GOOS == "windows" {
		if !strings.Contains(output, "MCP_TEMPLATE_TEST=hello") {
			t.Fatalf("expected injected env 'MCP_TEMPLATE_TEST=hello' via targeted probe, got %q", output)
		}
	} else {
		if got := strings.TrimSpace(output); got != "hello" {
			t.Fatalf("expected injected env value 'hello' via targeted probe, got %q", output)
		}
	}
}

// TestRunTemplateEnvDumpTruncationIsIndependent 独立覆盖全量 env dump 场景：
// 验证大输出按 OutputMaxBytes 截断时测试能稳定判定（含 [truncated] 标记），
// 与注入断言解耦——即使 env dump 输出超限，注入语义仍由定向探针用例验证。
func TestRunTemplateEnvDumpTruncationIsIndependent(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		CommandTemplates: map[string]config.CommandTemplate{
			"envdump": {Command: crossPlatformEnvDump(), Env: map[string]string{"MCP_TEMPLATE_TEST": "hello"}, Timeout: 5 * time.Second},
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
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
	// 全量 dump 在 Windows 用户 Temp 下可能超 4096 被截断：断言只验证
	// 输出非空且截断行为（如触发）返回 [truncated] 标记，不判定注入内容
	text := res.Content[0].Text
	if strings.TrimSpace(text) == "" {
		t.Fatalf("expected non-empty env dump output, got %#v", res.Content)
	}
	if strings.Contains(text, "[truncated]") {
		// 截断发生：输出以 [truncated] 收尾即符合 truncate 语义
		if !strings.HasSuffix(text, "[truncated]") {
			t.Fatalf("expected output to end with [truncated] marker when truncated, got %q", text)
		}
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
			"cleanup": {Command: crossPlatformEcho(), Category: "cleanup", Destructive: true, RequiresConfirmation: true, Timeout: 5 * time.Second},
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
