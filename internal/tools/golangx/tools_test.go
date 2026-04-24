package golangx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
)

func newTestConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	return config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		AuditLogPath:     filepath.Join(root, "audit.jsonl"),
		OutputMaxBytes:   1 << 16,
		CommandTimeout:   5 * time.Second,
	}
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

func writeModuleFile(t *testing.T, root string, rel string, content string) string {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func lineColumnOf(t *testing.T, content string, needle string) (int, int) {
	t.Helper()
	idx := strings.Index(content, needle)
	if idx < 0 {
		t.Fatalf("needle %q not found", needle)
	}
	line := 1
	lastLineStart := 0
	for i := 0; i < idx; i++ {
		if content[i] == '\n' {
			line++
			lastLineStart = i + 1
		}
	}
	column := idx - lastLineStart + 1
	return line, column
}

func TestListSymbolsReturnsTopLevelDeclarations(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "go.list_symbols")
	writeModuleFile(t, cfg.StartupDirectory, "go.mod", "module example.com/navtest\n\ngo 1.20\n")
	writeModuleFile(t, cfg.StartupDirectory, "sample.go", `package sample

type Service struct{}

const DefaultName = "x"

var Global = 1

func Build() {}

func (s *Service) Do() {}
`)

	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{"path": "sample.go"})
	if err != nil {
		t.Fatalf("go.list_symbols failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res.StructuredContent)
	}
	if res.StructuredContent["package"] != "sample" {
		t.Fatalf("expected package sample, got %#v", res.StructuredContent)
	}
	symbols, ok := res.StructuredContent["symbols"].([]map[string]any)
	if !ok {
		t.Fatalf("expected structured symbols slice, got %#v", res.StructuredContent["symbols"])
	}
	if len(symbols) != 5 {
		t.Fatalf("expected 5 symbols, got %#v", symbols)
	}
	kinds := map[string]string{}
	for _, sym := range symbols {
		kinds[sym["name"].(string)] = sym["kind"].(string)
	}
	if kinds["Service"] != "type" || kinds["DefaultName"] != "const" || kinds["Global"] != "var" || kinds["Build"] != "func" || kinds["Do"] != "method" {
		t.Fatalf("unexpected symbol kinds: %#v", kinds)
	}
}

func TestListSymbolsRejectsNonGoFile(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "go.list_symbols")
	writeModuleFile(t, cfg.StartupDirectory, "notes.txt", "hello")

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{"path": "notes.txt"})
	if err == nil {
		t.Fatal("expected error for non-go file")
	}
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.StructuredContent["reason"] != "not_go_file" {
		t.Fatalf("unexpected structured error: %#v", toolErr.StructuredContent)
	}
}

func TestFindDefinitionResolvesSamePackageOtherFile(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "go.find_definition")
	writeModuleFile(t, cfg.StartupDirectory, "go.mod", "module example.com/navtest\n\ngo 1.20\n")
	helper := writeModuleFile(t, cfg.StartupDirectory, "helper.go", `package sample

func Helper() {}
`)
	mainContent := `package sample

func Use() {
	Helper()
}
`
	writeModuleFile(t, cfg.StartupDirectory, "use.go", mainContent)
	line, column := lineColumnOf(t, mainContent, "Helper")

	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":   "use.go",
		"line":   line,
		"column": column,
	})
	if err != nil {
		t.Fatalf("go.find_definition failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res.StructuredContent)
	}
	if res.StructuredContent["symbol_name"] != "Helper" {
		t.Fatalf("unexpected symbol name: %#v", res.StructuredContent)
	}
	if got := res.StructuredContent["definition_path"].(string); filepath.Clean(got) != filepath.Clean(helper) {
		t.Fatalf("expected definition path %s, got %s", helper, got)
	}
	if res.StructuredContent["in_allowed_roots"] != true {
		t.Fatalf("expected in_allowed_roots=true, got %#v", res.StructuredContent)
	}
}

func TestFindDefinitionReturnsExternalLocationMarkedOutsideAllowedRoots(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "go.find_definition")
	writeModuleFile(t, cfg.StartupDirectory, "go.mod", "module example.com/navtest\n\ngo 1.20\n")
	content := `package sample

import "fmt"

func Use() {
	fmt.Println("x")
}
`
	writeModuleFile(t, cfg.StartupDirectory, "main.go", content)
	line, column := lineColumnOf(t, content, "Println")

	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":   "main.go",
		"line":   line,
		"column": column,
	})
	if err != nil {
		t.Fatalf("go.find_definition failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res.StructuredContent)
	}
	if res.StructuredContent["symbol_name"] != "Println" {
		t.Fatalf("unexpected symbol name: %#v", res.StructuredContent)
	}
	if res.StructuredContent["in_allowed_roots"] != false {
		t.Fatalf("expected in_allowed_roots=false, got %#v", res.StructuredContent)
	}
	defPath, _ := res.StructuredContent["definition_path"].(string)
	if strings.TrimSpace(defPath) == "" || filepath.Ext(defPath) != ".go" {
		t.Fatalf("expected external definition path, got %#v", res.StructuredContent)
	}
}

func TestFindDefinitionRejectsOutOfBoundsPosition(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "go.find_definition")
	writeModuleFile(t, cfg.StartupDirectory, "go.mod", "module example.com/navtest\n\ngo 1.20\n")
	writeModuleFile(t, cfg.StartupDirectory, "main.go", `package sample

func Use() {}
`)

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":   "main.go",
		"line":   99,
		"column": 1,
	})
	if err == nil {
		t.Fatal("expected out-of-bounds error")
	}
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.StructuredContent["reason"] != "position_out_of_bounds" {
		t.Fatalf("unexpected structured error: %#v", toolErr.StructuredContent)
	}
}

func TestFindDefinitionRejectsLocalVariableTargets(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "go.find_definition")
	writeModuleFile(t, cfg.StartupDirectory, "go.mod", "module example.com/navtest\n\ngo 1.20\n")
	content := `package sample

func Use() {
	value := 1
	_ = value
}
`
	writeModuleFile(t, cfg.StartupDirectory, "main.go", content)
	line, column := lineColumnOf(t, content, "value")

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":   "main.go",
		"line":   line,
		"column": column,
	})
	if err == nil {
		t.Fatal("expected unsupported local variable error")
	}
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.StructuredContent["reason"] != "definition_not_resolved" {
		t.Fatalf("unexpected structured error: %#v", toolErr.StructuredContent)
	}
}

func TestFindDefinitionRejectsNonIdentifierPosition(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "go.find_definition")
	writeModuleFile(t, cfg.StartupDirectory, "go.mod", "module example.com/navtest\n\ngo 1.20\n")
	content := `package sample

func Use() {
	Helper()
}

func Helper() {}
`
	writeModuleFile(t, cfg.StartupDirectory, "main.go", content)

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":   "main.go",
		"line":   4,
		"column": 1,
	})
	if err == nil {
		t.Fatal("expected identifier_not_found error")
	}
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.StructuredContent["reason"] != "identifier_not_found" {
		t.Fatalf("unexpected structured error: %#v", toolErr.StructuredContent)
	}
}
