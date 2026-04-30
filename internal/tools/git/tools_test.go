package git

import (
	"context"
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

func TestGitStatusAllowsRepoOutsideAllowedRootsWhenUnsafeAllowAllEnabled(t *testing.T) {
	allowedRoot := t.TempDir()
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init")
	cmd.Dir = repoDir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v output=%s", err, output)
	}
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
