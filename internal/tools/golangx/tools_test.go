package golangx

import (
	"context"
	"encoding/json"
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

func TestFindDefinitionSchemaDeclaresPositiveLineAndColumn(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "go.find_definition")
	props := tool.Schema()["properties"].(map[string]any)
	for _, name := range []string{"line", "column"} {
		prop := props[name].(map[string]any)
		if got, ok := prop["minimum"].(int); !ok || got != 1 {
			t.Fatalf("go.find_definition schema should declare %s minimum=1, got %#v", name, prop)
		}
	}
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

func TestGoToolsRejectNonStringPath(t *testing.T) {
	cfg := newTestConfig(t)
	tests := []struct {
		name string
		tool string
		args map[string]any
	}{
		{
			name: "list symbols",
			tool: "go.list_symbols",
			args: map[string]any{"path": []any{"sample.go"}},
		},
		{
			name: "find definition",
			tool: "go.find_definition",
			args: map[string]any{"path": 123, "line": 1, "column": 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := findTool(t, cfg, tt.tool)
			_, err := tool.Call(context.Background(), mcp.CallContext{}, tt.args)
			if err == nil {
				t.Fatal("expected non-string path to be rejected")
			}
			if !strings.Contains(err.Error(), "path must be a string") {
				t.Fatalf("expected type error, got %v", err)
			}
			var toolErr *mcp.ToolError
			if !errors.As(err, &toolErr) {
				t.Fatalf("expected ToolError, got %T", err)
			}
			if toolErr.StructuredContent["reason"] != "validation_failed" {
				t.Fatalf("unexpected structured error: %#v", toolErr.StructuredContent)
			}
		})
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

func TestFindDefinitionRejectsFractionalPosition(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "go.find_definition")
	writeModuleFile(t, cfg.StartupDirectory, "go.mod", "module example.com/navtest\n\ngo 1.20\n")
	writeModuleFile(t, cfg.StartupDirectory, "sample.go", "package sample\n\nfunc Use() {}\n")

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":   "sample.go",
		"line":   1.5,
		"column": float64(1),
	})
	if err == nil {
		t.Fatal("expected fractional line to be rejected")
	}

	_, err = tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":   "sample.go",
		"line":   1e100,
		"column": float64(1),
	})
	if err == nil {
		t.Fatal("expected out-of-range line to be rejected")
	}

	_, err = tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":   "sample.go",
		"line":   json.Number("9007199254740992.5"),
		"column": json.Number("1"),
	})
	if err == nil {
		t.Fatal("expected fractional JSON number line to be rejected")
	}
}

func TestFindDefinitionRejectsNonIntegerPositionWithValidationReason(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "go.find_definition")
	writeModuleFile(t, cfg.StartupDirectory, "sample.go", "package sample\n\nfunc Use() {}\n")

	tests := []struct {
		name    string
		args    map[string]any
		wantErr string
	}{
		{
			name: "line",
			args: map[string]any{
				"path":   "sample.go",
				"line":   "1",
				"column": 1,
			},
			wantErr: "line must be an integer",
		},
		{
			name: "column",
			args: map[string]any{
				"path":   "sample.go",
				"line":   1,
				"column": "1",
			},
			wantErr: "column must be an integer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tool.Call(context.Background(), mcp.CallContext{}, tt.args)
			if err == nil {
				t.Fatal("expected non-integer position to be rejected")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want %q, got %v", tt.wantErr, err)
			}
			var toolErr *mcp.ToolError
			if !errors.As(err, &toolErr) {
				t.Fatalf("expected ToolError, got %T", err)
			}
			if toolErr.StructuredContent["reason"] != "validation_failed" {
				t.Fatalf("unexpected structured error: %#v", toolErr.StructuredContent)
			}
		})
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

func TestFindDefinitionRejectsPositionImmediatelyAfterIdentifier(t *testing.T) {
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
	line, column := lineColumnOf(t, content, "Helper")

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":   "main.go",
		"line":   line,
		"column": column + len("Helper"),
	})
	if err == nil {
		t.Fatal("expected position immediately after identifier to be rejected")
	}
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.StructuredContent["reason"] != "identifier_not_found" {
		t.Fatalf("unexpected structured error: %#v", toolErr.StructuredContent)
	}
}

func TestFindDefinitionRejectsNonGoFile(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "go.find_definition")
	writeModuleFile(t, cfg.StartupDirectory, "test.txt", "hello")

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":   "test.txt",
		"line":   1,
		"column": 1,
	})
	if err == nil {
		t.Fatal("expected error for non-go file")
	}
	if !strings.Contains(err.Error(), "must point to a .go file") {
		t.Fatalf("expected not-go-file error, got %v", err)
	}
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.StructuredContent["reason"] != "not_go_file" {
		t.Fatalf("unexpected structured error: %#v", toolErr.StructuredContent)
	}
}

func TestFindDefinitionRejectsColumnOutOfBounds(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "go.find_definition")
	writeModuleFile(t, cfg.StartupDirectory, "go.mod", "module example.com/navtest\n\ngo 1.20\n")
	writeModuleFile(t, cfg.StartupDirectory, "main.go", "package sample\n\nfunc Use() {}\n")

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":   "main.go",
		"line":   1,
		"column": 999,
	})
	if err == nil {
		t.Fatal("expected column out of bounds error")
	}
	if !strings.Contains(err.Error(), "column out of bounds") {
		t.Fatalf("expected column out of bounds error, got %v", err)
	}
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.StructuredContent["reason"] != "position_out_of_bounds" {
		t.Fatalf("unexpected structured error: %#v", toolErr.StructuredContent)
	}
}

func TestFindDefinitionRejectsLineZero(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "go.find_definition")
	writeModuleFile(t, cfg.StartupDirectory, "main.go", "package sample\n\nfunc Use() {}\n")

	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"path":   "main.go",
		"line":   0,
		"column": 1,
	})
	if err == nil {
		t.Fatal("expected line=0 to be rejected")
	}
	if !strings.Contains(err.Error(), "positive integer") {
		t.Fatalf("expected positive integer error, got %v", err)
	}
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if toolErr.StructuredContent["reason"] != "position_out_of_bounds" {
		t.Fatalf("unexpected structured error: %#v", toolErr.StructuredContent)
	}
}

func TestListSymbolsReturnsEmptyForPackageOnlyFile(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "go.list_symbols")
	writeModuleFile(t, cfg.StartupDirectory, "go.mod", "module example.com/navtest\n\ngo 1.20\n")
	writeModuleFile(t, cfg.StartupDirectory, "empty.go", "package empty\n")

	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{"path": "empty.go"})
	if err != nil {
		t.Fatalf("go.list_symbols failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res.StructuredContent)
	}
	symbols, ok := res.StructuredContent["symbols"].([]map[string]any)
	if !ok {
		t.Fatalf("expected structured symbols slice, got %#v", res.StructuredContent["symbols"])
	}
	if len(symbols) != 0 {
		t.Fatalf("expected 0 symbols for package-only file, got %#v", symbols)
	}
	summary, _ := res.StructuredContent["summary"].(string)
	if !strings.Contains(summary, "0 symbol") {
		t.Fatalf("expected summary to mention 0 symbols, got %q", summary)
	}
}

func TestListSymbolsPopulatesPositionFields(t *testing.T) {
	cfg := newTestConfig(t)
	tool := findTool(t, cfg, "go.list_symbols")
	writeModuleFile(t, cfg.StartupDirectory, "go.mod", "module example.com/navtest\n\ngo 1.20\n")
	writeModuleFile(t, cfg.StartupDirectory, "sample.go", "package sample\n\nfunc foo() {}\n")

	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{"path": "sample.go"})
	if err != nil {
		t.Fatalf("go.list_symbols failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res.StructuredContent)
	}
	symbols, ok := res.StructuredContent["symbols"].([]map[string]any)
	if !ok || len(symbols) != 1 {
		t.Fatalf("expected 1 symbol, got %#v", res.StructuredContent["symbols"])
	}
	startLine, _ := symbols[0]["start_line"].(int)
	startColumn, _ := symbols[0]["start_column"].(int)
	if startLine < 1 {
		t.Fatalf("expected start_line >= 1, got %d", startLine)
	}
	if startColumn < 1 {
		t.Fatalf("expected start_column >= 1, got %d", startColumn)
	}
	if symbols[0]["name"] != "foo" || symbols[0]["kind"] != "func" {
		t.Fatalf("unexpected symbol: %#v", symbols[0])
	}
}

func TestGolangxPathRelaxedUnderUnsafeAllowAll(t *testing.T) {
	allowedRoot := t.TempDir()
	externalRoot := t.TempDir()
	writeModuleFile(t, externalRoot, "go.mod", "module example.com/external\n\ngo 1.20\n")
	goPath := writeModuleFile(t, externalRoot, "main.go", "package external\n\nfunc Helper() {}\n")

	cfg := config.Config{
		AllowedRoots:     []string{allowedRoot},
		StartupDirectory: externalRoot,
		AuditLogPath:     filepath.Join(allowedRoot, "audit.jsonl"),
		OutputMaxBytes:   1 << 16,
		CommandTimeout:   5 * time.Second,
		UnsafeAllowAll:   true,
	}
	tool := findTool(t, cfg, "go.list_symbols")
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{"path": goPath})
	if err != nil {
		t.Fatalf("go.list_symbols under UnsafeAllowAll failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res.StructuredContent)
	}
	symbols, ok := res.StructuredContent["symbols"].([]map[string]any)
	if !ok || len(symbols) != 1 {
		t.Fatalf("expected 1 symbol, got %#v", res.StructuredContent["symbols"])
	}
	if symbols[0]["name"] != "Helper" {
		t.Fatalf("unexpected symbol: %#v", symbols[0])
	}
}
