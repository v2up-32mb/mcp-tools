package git

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
)

func TestRepoRelativePathsRejectTraversal(t *testing.T) {
	_, err := repoRelativePaths([]any{"../outside"})
	if err == nil {
		t.Fatal("expected traversal path to be rejected")
	}
}

func TestRepoRelativePathsAllowLocalPaths(t *testing.T) {
	got, err := repoRelativePaths([]any{".", "subdir/file.go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("unexpected paths: %#v", got)
	}
}

func TestRepoRelativePathsRejectNonArray(t *testing.T) {
	_, err := repoRelativePaths("README.md")
	if err == nil {
		t.Fatal("expected non-array paths to be rejected")
	}
}

func TestGitRequiredPathSchemasDeclareMinItems(t *testing.T) {
	cfg := config.Config{}
	for _, toolName := range []string{"git.add", "git.restore"} {
		tool := findGitTool(t, cfg, toolName)
		props := tool.Schema()["properties"].(map[string]any)
		paths := props["paths"].(map[string]any)
		if got, ok := paths["minItems"].(int); !ok || got != 1 {
			t.Fatalf("%s schema should declare paths minItems=1, got %#v", toolName, paths)
		}
	}
}

func TestGitRequiredStringSchemasDeclareMinLength(t *testing.T) {
	cfg := config.Config{}
	tests := []struct {
		toolName string
		argName  string
	}{
		{toolName: "git.commit", argName: "message"},
		{toolName: "git.switch", argName: "branch"},
	}
	for _, tt := range tests {
		tool := findGitTool(t, cfg, tt.toolName)
		props := tool.Schema()["properties"].(map[string]any)
		arg := props[tt.argName].(map[string]any)
		if got, ok := arg["minLength"].(int); !ok || got != 1 {
			t.Fatalf("%s schema should declare %s minLength=1, got %#v", tt.toolName, tt.argName, arg)
		}
	}
}

func TestGitAddRejectsGitPathspecMagic(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init")
	cmd.Dir = repoDir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v output=%s", err, output)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	cfg := config.Config{
		AllowedRoots:     []string{repoDir},
		StartupDirectory: repoDir,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		GitAllowed:       map[string]bool{"add": true},
	}
	var tool mcp.Tool
	for _, candidate := range NewTools(cfg) {
		if candidate.Name() == "git.add" {
			tool = candidate
			break
		}
	}
	if tool == nil {
		t.Fatal("git.add tool not found")
	}

	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"repo_path": repoDir,
		"paths":     []any{":/"},
	})
	if err == nil {
		t.Fatalf("expected pathspec magic to be rejected, got result %#v", res)
	}

	cmd = exec.Command("git", "diff", "--cached", "--name-only")
	cmd.Dir = repoDir
	output, diffErr := cmd.CombinedOutput()
	if diffErr != nil {
		t.Fatalf("git diff --cached failed: %v output=%s", diffErr, output)
	}
	if strings.TrimSpace(string(output)) != "" {
		t.Fatalf("pathspec magic unexpectedly staged files: %s", output)
	}
}

func TestResolveRepoRejectsNonStringRepoPath(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
	}
	_, err := resolveRepo(context.Background(), cfg, map[string]any{"repo_path": 123})
	if err == nil {
		t.Fatal("expected non-string repo_path to be rejected")
	}
}

func TestParseLogLimitCapsAndValidatesType(t *testing.T) {
	got, err := parseLogLimit(float64(250))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 200 {
		t.Fatalf("expected limit to cap at 200, got %d", got)
	}
	if _, err := parseLogLimit(1.5); err == nil {
		t.Fatal("expected fractional limit to be rejected")
	}
	if _, err := parseLogLimit("10"); err == nil {
		t.Fatal("expected string limit to be rejected")
	}
	if _, err := parseLogLimit(1e100); err == nil {
		t.Fatal("expected out-of-range limit to be rejected")
	}
	if _, err := parseLogLimit(json.Number("9007199254740992.5")); err == nil {
		t.Fatal("expected fractional JSON number limit to be rejected")
	}
}

func TestParseLogLimitRejectsNonPositive(t *testing.T) {
	for _, raw := range []any{0, -1, float64(0), json.Number("-5")} {
		if _, err := parseLogLimit(raw); err == nil {
			t.Fatalf("expected non-positive limit %#v to be rejected", raw)
		}
	}
}

func TestGitCommitRejectsNonStringMessage(t *testing.T) {
	repoDir := initGitRepo(t)
	cfg := config.Config{
		AllowedRoots:     []string{repoDir},
		StartupDirectory: repoDir,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		GitAllowed:       map[string]bool{"commit": true},
	}
	tool := findGitTool(t, cfg, "git.commit")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"repo_path": repoDir,
		"message":   123,
	})
	if err == nil || !strings.Contains(err.Error(), "message must be a string") {
		t.Fatalf("expected message type error, got %v", err)
	}
}

func TestGitSwitchRejectsNonStringBranch(t *testing.T) {
	repoDir := initGitRepo(t)
	cfg := config.Config{
		AllowedRoots:     []string{repoDir},
		StartupDirectory: repoDir,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		GitAllowed:       map[string]bool{"switch": true},
	}
	tool := findGitTool(t, cfg, "git.switch")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"repo_path": repoDir,
		"branch":    []any{"main"},
	})
	if err == nil || !strings.Contains(err.Error(), "branch must be a string") {
		t.Fatalf("expected branch type error, got %v", err)
	}
}

func TestGitStatusAllowsRepoOutsideAllowedRootsWhenUnsafeAllowAllEnabled(t *testing.T) {
	allowedRoot := t.TempDir()
	repoDir := t.TempDir()
	initGitRepoAt(t, repoDir)
	cfg := config.Config{
		AllowedRoots:     []string{allowedRoot},
		StartupDirectory: allowedRoot,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		GitAllowed:       map[string]bool{"status": true},
		UnsafeAllowAll:   true,
	}
	var tool mcp.Tool
	for _, candidate := range NewTools(cfg) {
		if candidate.Name() == "git.status" {
			tool = candidate
			break
		}
	}
	if tool == nil {
		t.Fatal("git.status tool not found")
	}
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{"repo_path": repoDir})
	if err != nil {
		t.Fatalf("git.status failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
	if !strings.Contains(res.Content[0].Text, "##") {
		t.Fatalf("unexpected git status output: %q", res.Content[0].Text)
	}
	if got := res.StructuredContent["repo_path"]; got != filepath.Clean(repoDir) {
		t.Fatalf("unexpected repo_path: %#v", res.StructuredContent)
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	repoDir := t.TempDir()
	initGitRepoAt(t, repoDir)
	return repoDir
}

func initGitRepoAt(t *testing.T, repoDir string) {
	t.Helper()
	cmd := exec.Command("git", "init")
	cmd.Dir = repoDir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v output=%s", err, output)
	}
}

func findGitTool(t *testing.T, cfg config.Config, name string) mcp.Tool {
	t.Helper()
	for _, candidate := range NewTools(cfg) {
		if candidate.Name() == name {
			return candidate
		}
	}
	t.Fatalf("%s tool not found", name)
	return nil
}
