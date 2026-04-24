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

func TestValidateArgsRejectsAbsolutePath(t *testing.T) {
	preset := config.ExecPreset{AllowedArgs: []string{"-v"}, Timeout: time.Second}
	_, err := validateArgs("go_test", preset, []any{"/tmp/outside"})
	if err == nil {
		t.Fatal("expected absolute path to be rejected")
	}
}

func TestValidateArgsAllowsFlagsAndLocalTargets(t *testing.T) {
	preset := config.ExecPreset{AllowedArgs: []string{"-v", "-run"}, Timeout: time.Second}
	got, err := validateArgs("go_test", preset, []any{"-v", "./...", "./pkg"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("unexpected args: %#v", got)
	}
}

func TestValidateArgsRejectsInlineAbsoluteOutputPath(t *testing.T) {
	preset := config.ExecPreset{AllowedArgs: []string{"-o"}, Timeout: time.Second}
	_, err := validateArgs("go_build", preset, []any{"-o=/tmp/outside-binary", "./..."})
	if err == nil {
		t.Fatal("expected inline absolute output path to be rejected")
	}
}

func TestValidateArgsRejectsBlockedVettool(t *testing.T) {
	preset := config.ExecPreset{AllowedArgs: []string{"-vettool"}, Timeout: time.Second}
	_, err := validateArgs("go_vet", preset, []any{"-vettool=./tool"})
	if err == nil {
		t.Fatal("expected -vettool to be rejected")
	}
}

func TestValidateArgsAllowsInlineRelativePathFlags(t *testing.T) {
	preset := config.ExecPreset{AllowedArgs: []string{"-coverprofile", "-run"}, Timeout: time.Second}
	got, err := validateArgs("go_test", preset, []any{"-coverprofile=artifacts/cover.out", "-run=TestX", "./..."})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("unexpected args: %#v", got)
	}
	if got[0] != "-coverprofile=artifacts/cover.out" {
		t.Fatalf("unexpected normalized path arg: %#v", got)
	}
}

func TestValidateArgsAllowsGoModTidyInlineVersionFlags(t *testing.T) {
	preset := config.ExecPreset{AllowedArgs: []string{"-go", "-compat", "-v"}, Timeout: time.Second}
	got, err := validateArgs("go_mod_tidy", preset, []any{"-go=1.25", "-compat=1.24", "-v"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("unexpected args: %#v", got)
	}
	if got[0] != "-go=1.25" || got[1] != "-compat=1.24" {
		t.Fatalf("unexpected normalized flags: %#v", got)
	}
}

func TestDefaultTargetsForGoModTidyIsEmpty(t *testing.T) {
	if got := defaultTargetsForPreset("go_mod_tidy"); len(got) != 0 {
		t.Fatalf("expected no implicit targets, got %#v", got)
	}
}

func TestValidateArgsAllowsModuleSpecForGoGet(t *testing.T) {
	preset := config.ExecPreset{AllowedArgs: []string{"-u", "-t", "-x", "-tags"}, Timeout: time.Second}
	got, err := validateArgs("go_get", preset, []any{"github.com/example/project@v1.2.3", "./localpkg"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "github.com/example/project@v1.2.3" || got[1] != "localpkg" {
		t.Fatalf("unexpected args: %#v", got)
	}
}

func TestValidateArgsRejectsURLModuleSpecForGoGet(t *testing.T) {
	preset := config.ExecPreset{AllowedArgs: []string{"-u"}, Timeout: time.Second}
	_, err := validateArgs("go_get", preset, []any{"https://example.com/pkg"})
	if err == nil {
		t.Fatal("expected URL module spec to be rejected")
	}
}

func TestRunPresetInjectsConfiguredEnvAndCreatesCacheDirs(t *testing.T) {
	root := t.TempDir()
	cacheRoot := filepath.Join(root, ".mcp-cache")
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		ExecPresets: map[string]config.ExecPreset{
			"go_env_cache": {
				Command:   "go",
				FixedArgs: []string{"env", "GOCACHE"},
				Timeout:   5 * time.Second,
				ReadOnly:  true,
				Env: map[string]string{
					"GOCACHE":    filepath.Join(cacheRoot, "go-build"),
					"GOMODCACHE": filepath.Join(cacheRoot, "gomod"),
					"GOTMPDIR":   filepath.Join(cacheRoot, "tmp"),
				},
			},
		},
	}
	tool := findTool(t, cfg, "exec.run")
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"preset":  "go_env_cache",
		"workdir": ".",
	})
	if err != nil {
		t.Fatalf("exec.run failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
	text := strings.TrimSpace(res.Content[0].Text)
	if text != filepath.Join(cacheRoot, "go-build") {
		t.Fatalf("unexpected go env output: %q", text)
	}
	envKeys, ok := res.StructuredContent["env_keys"].([]string)
	if !ok || len(envKeys) != 3 {
		t.Fatalf("expected env_keys in structured result, got %#v", res.StructuredContent)
	}
	for _, dir := range []string{
		filepath.Join(cacheRoot, "go-build"),
		filepath.Join(cacheRoot, "gomod"),
		filepath.Join(cacheRoot, "tmp"),
	} {
		if stat, err := os.Stat(dir); err != nil || !stat.IsDir() {
			t.Fatalf("expected cache dir %q to exist, err=%v", dir, err)
		}
	}
}

func TestValidateArgsRejectsPositionalArgsForGoWorkSync(t *testing.T) {
	preset := config.ExecPreset{AllowedArgs: nil, Timeout: time.Second}
	_, err := validateArgs("go_work_sync", preset, []any{"./..."})
	if err == nil {
		t.Fatal("expected positional args to be rejected for go_work_sync")
	}
}

func TestValidateArgsRejectsPositionalArgsForGoModTidy(t *testing.T) {
	preset := config.ExecPreset{AllowedArgs: []string{"-v", "-e", "-diff", "-go", "-compat", "-x"}, Timeout: time.Second}
	_, err := validateArgs("go_mod_tidy", preset, []any{"./..."})
	if err == nil {
		t.Fatal("expected positional args to be rejected for go_mod_tidy")
	}
}
