package git

import (
	"context"
	"encoding/json"
	"fmt"
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
	for _, toolName := range []string{"git_add", "git_restore"} {
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
		{toolName: "git_commit", argName: "message"},
		{toolName: "git_switch", argName: "branch"},
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
		if candidate.Name() == "git_add" {
			tool = candidate
			break
		}
	}
	if tool == nil {
		t.Fatal("git_add tool not found")
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
	tool := findGitTool(t, cfg, "git_commit")
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
	tool := findGitTool(t, cfg, "git_switch")
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
		if candidate.Name() == "git_status" {
			tool = candidate
			break
		}
	}
	if tool == nil {
		t.Fatal("git_status tool not found")
	}
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{"repo_path": repoDir})
	if err != nil {
		t.Fatalf("git_status failed: %v", err)
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

func TestGitStatusRejectsUnauthorizedSubcommand(t *testing.T) {
	repoDir := initGitRepo(t)
	cfg := config.Config{
		AllowedRoots:     []string{repoDir},
		StartupDirectory: repoDir,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		GitAllowed:       map[string]bool{"add": true},
	}
	tool := findGitTool(t, cfg, "git_status")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"repo_path": repoDir,
	})
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected not-allowed error, got %v", err)
	}
}

func TestGitPushToolNotRegistered(t *testing.T) {
	cfg := config.Config{}
	for _, candidate := range NewTools(cfg) {
		if candidate.Name() == "git.push" {
			t.Fatalf("git.push tool should not be registered, found %#v", candidate)
		}
	}
}

func TestRepoRelativePathsRejectsOptionPrefix(t *testing.T) {
	_, err := repoRelativePaths([]any{"-foo"})
	if err == nil {
		t.Fatal("expected option-prefixed path to be rejected")
	}
	if !strings.Contains(err.Error(), "repo-relative") {
		t.Fatalf("expected repo-relative error, got %v", err)
	}
}

func TestGitCommitHappyPath(t *testing.T) {
	repoDir := initGitRepo(t)
	for _, c := range []struct{ args []string }{
		{args: []string{"config", "user.name", "test"}},
		{args: []string{"config", "user.email", "test@example.com"}},
	} {
		cmd := exec.Command("git", c.args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v output=%s", c.args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	addCmd := exec.Command("git", "add", "a.txt")
	addCmd.Dir = repoDir
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %v output=%s", err, out)
	}

	cfg := config.Config{
		AllowedRoots:     []string{repoDir},
		StartupDirectory: repoDir,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		GitAllowed:       map[string]bool{"commit": true},
	}
	tool := findGitTool(t, cfg, "git_commit")
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"repo_path": repoDir,
		"message":   "init",
	})
	if err != nil {
		t.Fatalf("git_commit failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "master") && !strings.Contains(text, "main") && !strings.Contains(text, "init") {
		t.Fatalf("expected commit output to reference branch or commit, got %q", text)
	}
}

func TestGitSwitchHappyPath(t *testing.T) {
	repoDir := initGitRepo(t)
	for _, args := range [][]string{
		{"config", "user.name", "test"},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v output=%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	addCmd := exec.Command("git", "add", "a.txt")
	addCmd.Dir = repoDir
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %v output=%s", err, out)
	}
	commitCmd := exec.Command("git", "commit", "-m", "init")
	commitCmd.Dir = repoDir
	if out, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v output=%s", err, out)
	}
	branchCmd := exec.Command("git", "branch", "new-branch")
	branchCmd.Dir = repoDir
	if out, err := branchCmd.CombinedOutput(); err != nil {
		t.Fatalf("git branch failed: %v output=%s", err, out)
	}

	cfg := config.Config{
		AllowedRoots:     []string{repoDir},
		StartupDirectory: repoDir,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		GitAllowed:       map[string]bool{"switch": true},
	}
	tool := findGitTool(t, cfg, "git_switch")
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"repo_path": repoDir,
		"branch":    "new-branch",
	})
	if err != nil {
		t.Fatalf("git_switch failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}

	verifyCmd := exec.Command("git", "branch", "--show-current")
	verifyCmd.Dir = repoDir
	out, err := verifyCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --show-current failed: %v output=%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "new-branch" {
		t.Fatalf("expected current branch new-branch, got %q", strings.TrimSpace(string(out)))
	}
}

func TestGitLogDefaultLimitReturns20CapButShowsAllCommits(t *testing.T) {
	repoDir := initGitRepo(t)
	for _, args := range [][]string{
		{"config", "user.name", "test"},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v output=%s", args, err, out)
		}
	}
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("file%d.txt", i)
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		addCmd := exec.Command("git", "add", name)
		addCmd.Dir = repoDir
		if out, err := addCmd.CombinedOutput(); err != nil {
			t.Fatalf("git add %s failed: %v output=%s", name, err, out)
		}
		commitCmd := exec.Command("git", "commit", "-m", "commit "+name)
		commitCmd.Dir = repoDir
		if out, err := commitCmd.CombinedOutput(); err != nil {
			t.Fatalf("git commit %s failed: %v output=%s", name, err, out)
		}
	}

	cfg := config.Config{
		AllowedRoots:     []string{repoDir},
		StartupDirectory: repoDir,
		OutputMaxBytes:   1 << 16,
		CommandTimeout:   5 * time.Second,
		GitAllowed:       map[string]bool{"log": true},
	}
	tool := findGitTool(t, cfg, "git_log")
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"repo_path": repoDir,
	})
	if err != nil {
		t.Fatalf("git_log failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
	text := res.Content[0].Text
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 log lines, got %d: %q", len(lines), text)
	}
}

func TestGitOutputMaxBytesTruncation(t *testing.T) {
	repoDir := initGitRepo(t)
	for _, args := range [][]string{
		{"config", "user.name", "test"},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v output=%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "longfile.txt"), []byte("this is a long enough line to exceed 5 bytes\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	addCmd := exec.Command("git", "add", "longfile.txt")
	addCmd.Dir = repoDir
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %v output=%s", err, out)
	}

	cfg := config.Config{
		AllowedRoots:     []string{repoDir},
		StartupDirectory: repoDir,
		OutputMaxBytes:   5,
		CommandTimeout:   5 * time.Second,
		GitAllowed:       map[string]bool{"status": true},
	}
	tool := findGitTool(t, cfg, "git_status")
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"repo_path": repoDir,
	})
	if err != nil {
		t.Fatalf("git_status failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
	stdout, _ := res.StructuredContent["stdout"].(string)
	if !strings.Contains(stdout, "[truncated]") {
		t.Fatalf("expected stdout to be truncated, got %q", stdout)
	}
}

func TestParseLogLimitNilReturnsDefault20(t *testing.T) {
	got, err := parseLogLimit(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 20 {
		t.Fatalf("expected default limit 20, got %d", got)
	}
}

func TestGitAddRejectsEmptyPaths(t *testing.T) {
	repoDir := initGitRepo(t)
	cfg := config.Config{
		AllowedRoots:     []string{repoDir},
		StartupDirectory: repoDir,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		GitAllowed:       map[string]bool{"add": true},
	}
	tool := findGitTool(t, cfg, "git_add")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"repo_path": repoDir,
	})
	if err == nil {
		t.Fatal("expected error for empty paths")
	}
	if !strings.Contains(err.Error(), "paths required") {
		t.Fatalf("expected 'paths required' error, got %v", err)
	}
}

func TestGitCommitNothingToCommitFailsGracefully(t *testing.T) {
	repoDir := initGitRepo(t)
	for _, args := range [][]string{
		{"config", "user.name", "test"},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v output=%s", args, err, out)
		}
	}
	// No files staged — git commit should fail.
	cfg := config.Config{
		AllowedRoots:     []string{repoDir},
		StartupDirectory: repoDir,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		GitAllowed:       map[string]bool{"commit": true},
	}
	tool := findGitTool(t, cfg, "git_commit")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"repo_path": repoDir,
		"message":   "empty",
	})
	if err == nil {
		t.Fatal("expected error when committing with nothing staged")
	}
}

func TestGitAddAddsFilesInRepo(t *testing.T) {
	repoDir := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(repoDir, "test.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	cfg := config.Config{
		AllowedRoots:     []string{repoDir},
		StartupDirectory: repoDir,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		GitAllowed:       map[string]bool{"add": true},
	}
	tool := findGitTool(t, cfg, "git_add")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"repo_path": repoDir,
		"paths":     []any{"test.txt"},
	})
	if err != nil {
		t.Fatalf("git_add failed: %v", err)
	}
	verifyCmd := exec.Command("git", "diff", "--cached", "--name-only")
	verifyCmd.Dir = repoDir
	out, err := verifyCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git diff --cached failed: %v output=%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "test.txt" {
		t.Fatalf("expected test.txt in staged files, got %q", out)
	}
}

func TestGitRestoreRestoresModifiedFile(t *testing.T) {
	repoDir := initGitRepo(t)
	for _, args := range [][]string{
		{"config", "user.name", "test"},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v output=%s", args, err, out)
		}
	}
	// Commit a file so the index has a known-good version.
	if err := os.WriteFile(filepath.Join(repoDir, "a.txt"), []byte("committed\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	addCmd := exec.Command("git", "add", "a.txt")
	addCmd.Dir = repoDir
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %v output=%s", err, out)
	}
	commitCmd := exec.Command("git", "commit", "-m", "init")
	commitCmd.Dir = repoDir
	if out, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v output=%s", err, out)
	}

	// Modify the working-copy of the committed file (not staged).
	if err := os.WriteFile(filepath.Join(repoDir, "a.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	cfg := config.Config{
		AllowedRoots:     []string{repoDir},
		StartupDirectory: repoDir,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		GitAllowed:       map[string]bool{"restore": true},
	}
	tool := findGitTool(t, cfg, "git_restore")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"repo_path": repoDir,
		"paths":     []any{"a.txt"},
	})
	if err != nil {
		t.Fatalf("git_restore failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(repoDir, "a.txt"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(got) != "committed\n" {
		t.Fatalf("expected restore to revert working copy to committed state, got %q", got)
	}
}

func TestGitResolveRepoRejectsRelativePathNotInAllowedRoots(t *testing.T) {
	root := t.TempDir()
	initGitRepoAt(t, root)
	repoDir := root
	// Create cfg with a different allowed root
	otherDir := t.TempDir()
	cfg := config.Config{
		AllowedRoots:     []string{otherDir},
		StartupDirectory: otherDir,
		OutputMaxBytes:   4096,
	}
	_, err := resolveRepo(context.Background(), cfg, map[string]any{"repo_path": repoDir})
	if err == nil {
		t.Fatal("expected error for repo_path outside allowed roots")
	}
}

func TestGitAddRejectsTraversalPath(t *testing.T) {
	repoDir := initGitRepo(t)
	cfg := config.Config{
		AllowedRoots:     []string{repoDir},
		StartupDirectory: repoDir,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		GitAllowed:       map[string]bool{"add": true},
	}
	tool := findGitTool(t, cfg, "git_add")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"repo_path": repoDir,
		"paths":     []any{"../outside"},
	})
	if err == nil {
		t.Fatal("expected error for traversal path")
	}
}

func TestGitLogRejectsNegativeLimit(t *testing.T) {
	_, err := parseLogLimit(float64(-5))
	if err == nil {
		t.Fatal("expected error for negative limit")
	}
}

func TestGitLogRejectsZeroLimit(t *testing.T) {
	_, err := parseLogLimit(float64(0))
	if err == nil {
		t.Fatal("expected error for zero limit")
	}
}

func TestRepoRelativePathsRejectsMixedValidAndInvalid(t *testing.T) {
	// A mixed slice with one valid and one invalid path should fail on the invalid one.
	_, err := repoRelativePaths([]any{"valid.go", "../outside"})
	if err == nil {
		t.Fatal("expected error for mixed valid/invalid paths")
	}
}

func TestRepoRelativePathsRejectsAbsolutePathInMixedSlice(t *testing.T) {
	_, err := repoRelativePaths([]any{"valid.go", "/etc/passwd"})
	if err == nil {
		t.Fatal("expected error for absolute path in mixed slice")
	}
}

func TestGitAddRejectsPathspecMagicBang(t *testing.T) {
	repoDir := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(repoDir, "test.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		AllowedRoots:     []string{repoDir},
		StartupDirectory: repoDir,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		GitAllowed:       map[string]bool{"add": true},
	}
	tool := findGitTool(t, cfg, "git_add")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"repo_path": repoDir,
		"paths":     []any{"!test.txt"},
	})
	if err == nil {
		t.Fatal("expected pathspec magic ! to be rejected")
	}
}

func TestGitDiffHappyPath(t *testing.T) {
	repoDir := initGitRepo(t)
	for _, args := range [][]string{
		{"config", "user.name", "test"},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v output=%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "a.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	addCmd := exec.Command("git", "add", "a.txt")
	addCmd.Dir = repoDir
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %v output=%s", err, out)
	}
	commitCmd := exec.Command("git", "commit", "-m", "init")
	commitCmd.Dir = repoDir
	if out, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v output=%s", err, out)
	}
	// Modify to create an unstaged diff
	if err := os.WriteFile(filepath.Join(repoDir, "a.txt"), []byte("modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		AllowedRoots:     []string{repoDir},
		StartupDirectory: repoDir,
		OutputMaxBytes:   1 << 16,
		CommandTimeout:   5 * time.Second,
		GitAllowed:       map[string]bool{"diff": true},
	}
	tool := findGitTool(t, cfg, "git_diff")
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"repo_path": repoDir,
	})
	if err != nil {
		t.Fatalf("git_diff failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
	stdout, _ := res.StructuredContent["stdout"].(string)
	if !strings.Contains(stdout, "modified") {
		t.Fatalf("expected diff output to contain 'modified', got %q", stdout)
	}
	if !strings.Contains(stdout, "-initial") {
		t.Fatalf("expected diff to show removal of 'initial', got %q", stdout)
	}
}

func TestGitStatusHappyPath(t *testing.T) {
	repoDir := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(repoDir, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		AllowedRoots:     []string{repoDir},
		StartupDirectory: repoDir,
		OutputMaxBytes:   1 << 16,
		CommandTimeout:   5 * time.Second,
		GitAllowed:       map[string]bool{"status": true},
	}
	tool := findGitTool(t, cfg, "git_status")
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"repo_path": repoDir,
	})
	if err != nil {
		t.Fatalf("git_status failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "new.txt") {
		t.Fatalf("expected status to mention new.txt, got %q", text)
	}
}
