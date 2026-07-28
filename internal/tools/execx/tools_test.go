package execx

import (
	"context"
	"encoding/json"
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

func TestValidateArgsPreservesExplicitRelativeGoTargets(t *testing.T) {
	preset := config.ExecPreset{AllowedArgs: []string{"-v"}, Timeout: time.Second}
	got, err := validateArgs("go_test", preset, []any{"./...", "./pkg"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"./...", "./pkg"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("go package targets should keep explicit ./ prefix\nwant: %#v\ngot:  %#v", want, got)
	}
}

func TestRunPresetAppendsDefaultTargetWhenOnlyFlagsProvided(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		ExecPresets: map[string]config.ExecPreset{
			"go_test": {
				Command:     "printf",
				FixedArgs:   []string{"%s\n"},
				AllowedArgs: []string{"-v"},
				Timeout:     5 * time.Second,
				ReadOnly:    true,
			},
		},
	}
	tool := findTool(t, cfg, "exec.run")
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"preset":  "go_test",
		"workdir": ".",
		"args":    []any{"-v"},
	})
	if err != nil {
		t.Fatalf("exec.run failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
	if got, want := strings.TrimSpace(res.StructuredContent["stdout"].(string)), "-v\n./..."; got != want {
		t.Fatalf("expected default target after flag-only args\nwant: %q\ngot:  %q", want, got)
	}
}

func TestApplyTimeoutOverrideRejectsNonInteger(t *testing.T) {
	_, err := applyTimeoutOverride(5*time.Second, 1.5)
	if err == nil {
		t.Fatal("expected fractional timeout override to be rejected")
	}
	_, err = applyTimeoutOverride(5*time.Second, "1")
	if err == nil {
		t.Fatal("expected string timeout override to be rejected")
	}
	_, err = applyTimeoutOverride(5*time.Second, 1e100)
	if err == nil {
		t.Fatal("expected out-of-range timeout override to be rejected")
	}
	_, err = applyTimeoutOverride(5*time.Second, json.Number("9007199254740992.5"))
	if err == nil {
		t.Fatal("expected fractional JSON number timeout override to be rejected")
	}
}

func TestApplyTimeoutOverrideOnlyShortens(t *testing.T) {
	got, err := applyTimeoutOverride(5*time.Second, float64(10))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 5*time.Second {
		t.Fatalf("expected longer override to be ignored, got %s", got)
	}
	got, err = applyTimeoutOverride(5*time.Second, float64(2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 2*time.Second {
		t.Fatalf("expected shorter override to apply, got %s", got)
	}
}

func TestApplyTimeoutOverrideRejectsNonPositive(t *testing.T) {
	for _, raw := range []any{0, -1, float64(0), json.Number("-2")} {
		if _, err := applyTimeoutOverride(5*time.Second, raw); err == nil {
			t.Fatalf("expected non-positive timeout override to be rejected for %#v", raw)
		}
	}
}

func TestSchemasDeclarePositiveTimeoutOverride(t *testing.T) {
	cfg := config.Config{CommandTemplates: map[string]config.CommandTemplate{
		"make_test": {Command: []string{"make", "test"}, Timeout: time.Second},
	}}
	for _, toolName := range []string{"exec.run", "exec.run_template"} {
		tool := findTool(t, cfg, toolName)
		props := tool.Schema()["properties"].(map[string]any)
		timeoutProp := props["timeout_override_sec"].(map[string]any)
		if got, ok := timeoutProp["minimum"].(int); !ok || got != 1 {
			t.Fatalf("%s schema should declare timeout_override_sec minimum=1, got %#v", toolName, timeoutProp)
		}
	}
}

func TestExecSchemasDeclareStringMinLength(t *testing.T) {
	cfg := config.Config{CommandTemplates: map[string]config.CommandTemplate{
		"make_test": {Command: []string{"make", "test"}, Timeout: time.Second},
	}}
	tests := []struct {
		toolName string
		propName string
	}{
		{toolName: "exec.run", propName: "command"},
		{toolName: "exec.run", propName: "workdir"},
		{toolName: "exec.run_template", propName: "template"},
		{toolName: "exec.run_template", propName: "workdir"},
	}
	for _, tt := range tests {
		tool := findTool(t, cfg, tt.toolName)
		props := tool.Schema()["properties"].(map[string]any)
		prop := props[tt.propName].(map[string]any)
		if got, ok := prop["minLength"].(int); !ok || got != 1 {
			t.Fatalf("%s schema should declare %s minLength=1, got %#v", tt.toolName, tt.propName, prop)
		}
	}
}

func TestApplyTimeoutOverrideIgnoresHugeLongerOverrideWithoutOverflow(t *testing.T) {
	got, err := applyTimeoutOverride(5*time.Second, float64(1_000_000_000_000))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 5*time.Second {
		t.Fatalf("expected huge longer override to be ignored, got %s", got)
	}
}

func TestRawCommandRejectsNonPositiveTimeoutOverrideBeforeExecution(t *testing.T) {
	allowedRoot := t.TempDir()
	workdir := t.TempDir()
	marker := filepath.Join(workdir, "marker.txt")
	cfg := config.Config{
		AllowedRoots:     []string{allowedRoot},
		StartupDirectory: allowedRoot,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		UnsafeAllowAll:   true,
	}
	tool := findTool(t, cfg, "exec.run")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"command":              "sh",
		"args":                 []any{"-c", "printf ran > marker.txt"},
		"workdir":              workdir,
		"timeout_override_sec": -1,
	})
	if err == nil {
		t.Fatal("expected non-positive timeout override to be rejected")
	}
	if !strings.Contains(err.Error(), "timeout_override_sec must be > 0") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("command should not execute on validation error, marker stat err=%v", statErr)
	}
}

func TestExecRunRejectsNonStringCommandBeforePresetFallback(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "marker.txt")
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		UnsafeAllowAll:   true,
		ExecPresets: map[string]config.ExecPreset{
			"mark": {
				Command:   "sh",
				FixedArgs: []string{"-c", "printf preset > marker.txt"},
				Timeout:   5 * time.Second,
			},
		},
	}
	tool := findTool(t, cfg, "exec.run")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"command": []any{"sh"},
		"preset":  "mark",
		"workdir": root,
	})
	if err == nil {
		t.Fatal("expected non-string command to be rejected")
	}
	if !strings.Contains(err.Error(), "command must be a string") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("preset should not execute on command validation error, marker stat err=%v", statErr)
	}
}

func TestExecRunRejectsNonStringWorkdirBeforeExecution(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "marker.txt")
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		UnsafeAllowAll:   true,
	}
	tool := findTool(t, cfg, "exec.run")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"command": "sh",
		"args":    []any{"-c", "printf ran > marker.txt"},
		"workdir": []any{root},
	})
	if err == nil {
		t.Fatal("expected non-string workdir to be rejected")
	}
	if !strings.Contains(err.Error(), "workdir must be a string") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("command should not execute on validation error, marker stat err=%v", statErr)
	}
}

func TestExecRunRejectsRawCommandWhenUnsafeDisabledBeforePresetFallback(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "marker.txt")
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		ExecPresets: map[string]config.ExecPreset{
			"mark": {
				Command:   "sh",
				FixedArgs: []string{"-c", "printf preset > marker.txt"},
				Timeout:   5 * time.Second,
			},
		},
	}
	tool := findTool(t, cfg, "exec.run")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"command": "sh",
		"preset":  "mark",
		"workdir": root,
	})
	if err == nil {
		t.Fatal("expected raw command to be rejected when unsafe_allow_all is disabled")
	}
	if !strings.Contains(err.Error(), "raw command not allowed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("preset should not execute when raw command is rejected, marker stat err=%v", statErr)
	}
}

func TestValidateArgsRejectsNonArrayArgs(t *testing.T) {
	preset := config.ExecPreset{AllowedArgs: []string{"-v"}, Timeout: time.Second}
	_, err := validateArgs("go_test", preset, "-v")
	if err == nil {
		t.Fatal("expected non-array args to be rejected")
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

func TestValidateArgsAllowsSeparateValueFlags(t *testing.T) {
	root := t.TempDir()
	preset := config.ExecPreset{AllowedArgs: []string{"-coverprofile", "-run"}, Timeout: time.Second}
	got, err := validateArgsForWorkdir("go_test", preset, []any{"-run", "TestX", "-coverprofile", "artifacts/cover.out"}, root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"-run", "TestX", "-coverprofile", "artifacts/cover.out"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected args\nwant: %#v\ngot:  %#v", want, got)
	}
}

func TestValidateArgsTreatsGoGenerateSkipValueAsFlagValue(t *testing.T) {
	preset := config.ExecPreset{AllowedArgs: []string{"-skip"}, Timeout: time.Second}
	tests := []struct {
		name string
		args []any
		want []string
	}{
		{
			name: "inline",
			args: []any{"-skip=Generated"},
			want: []string{"-skip=Generated"},
		},
		{
			name: "separate",
			args: []any{"-skip", "Generated"},
			want: []string{"-skip", "Generated"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateArgsForWorkdirDetailed("go_generate", preset, tt.args, "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.hasTarget {
				t.Fatalf("-skip value should not count as an explicit target: %#v", got)
			}
			if strings.Join(got.args, "\x00") != strings.Join(tt.want, "\x00") {
				t.Fatalf("unexpected args\nwant: %#v\ngot:  %#v", tt.want, got.args)
			}
		})
	}
}

func TestRunPresetRejectsInlinePathFlagThroughSymlinkOutsideWorkdir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "artifacts")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		ExecPresets: map[string]config.ExecPreset{
			"cover": {
				Command:     "true",
				AllowedArgs: []string{"-coverprofile"},
				Timeout:     5 * time.Second,
			},
		},
	}
	tool := findTool(t, cfg, "exec.run")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"preset":  "cover",
		"workdir": ".",
		"args":    []any{"-coverprofile=artifacts/cover.out"},
	})
	if err == nil {
		t.Fatal("expected inline path flag through symlink outside workdir to be rejected")
	}
}

func TestRunPresetRejectsSeparatePathFlagThroughSymlinkOutsideWorkdir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "artifacts")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		ExecPresets: map[string]config.ExecPreset{
			"cover": {
				Command:     "true",
				AllowedArgs: []string{"-coverprofile"},
				Timeout:     5 * time.Second,
			},
		},
	}
	tool := findTool(t, cfg, "exec.run")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"preset":  "cover",
		"workdir": ".",
		"args":    []any{"-coverprofile", "artifacts/cover.out"},
	})
	if err == nil {
		t.Fatal("expected separate path flag through symlink outside workdir to be rejected")
	}
}

func TestRunPresetRejectsPositionalTargetThroughSymlinkOutsideWorkdir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		ExecPresets: map[string]config.ExecPreset{
			"go_test": {
				Command: "true",
				Timeout: 5 * time.Second,
			},
		},
	}
	tool := findTool(t, cfg, "exec.run")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"preset":  "go_test",
		"workdir": ".",
		"args":    []any{"./linked/..."},
	})
	if err == nil {
		t.Fatal("expected positional target through symlink outside workdir to be rejected")
	}
}

func TestRunPresetRejectsGoGetLocalTargetThroughSymlinkOutsideWorkdir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		ExecPresets: map[string]config.ExecPreset{
			"go_get": {
				Command: "true",
				Timeout: 5 * time.Second,
			},
		},
	}
	tool := findTool(t, cfg, "exec.run")
	_, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"preset":  "go_get",
		"workdir": ".",
		"args":    []any{"./linked/..."},
	})
	if err == nil {
		t.Fatal("expected go_get local target through symlink outside workdir to be rejected")
	}
}

func TestValidateArgsAllowsGoModTidyInlineVersionFlags(t *testing.T) {
	preset := config.ExecPreset{AllowedArgs: []string{"-go", "-compat", "-v"}, Timeout: time.Second}
	got, err := validateArgs("go_mod_tidy", preset, []any{"-go=1.20", "-compat=1.19", "-v"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("unexpected args: %#v", got)
	}
	if got[0] != "-go=1.20" || got[1] != "-compat=1.19" {
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
	if len(got) != 2 || got[0] != "github.com/example/project@v1.2.3" || got[1] != "./localpkg" {
		t.Fatalf("unexpected args: %#v", got)
	}
}

func TestValidateArgsRejectsModuleLikeTargetsForNonGoGetPresets(t *testing.T) {
	preset := config.ExecPreset{AllowedArgs: []string{"-v"}, Timeout: time.Second}
	_, err := validateArgs("go_test", preset, []any{"github.com/example/project"})
	if err == nil {
		t.Fatal("expected module-like import path to be rejected for go_test")
	}
}

func TestValidateArgsNormalizesAmbiguousGoImportPathToLocalTarget(t *testing.T) {
	preset := config.ExecPreset{AllowedArgs: []string{"-v"}, Timeout: time.Second}
	got, err := validateArgs("go_test", preset, []any{"net/http", "..."})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"./net/http", "./..."}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("ambiguous package targets should execute as local paths\nwant: %#v\ngot:  %#v", want, got)
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

func TestRawCommandArgsRejectsNonArray(t *testing.T) {
	_, err := rawStringArgs("-c echo ignored")
	if err == nil {
		t.Fatal("expected non-array raw args to be rejected")
	}
}

func TestRawCommandArgsAllowEmptyStringValues(t *testing.T) {
	got, err := rawStringArgs([]any{"-n", ""})
	if err != nil {
		t.Fatalf("raw args should allow empty string values: %v", err)
	}
	want := []string{"-n", ""}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected raw args\nwant: %#v\ngot:  %#v", want, got)
	}
}

func TestRawEnvRejectsNonObject(t *testing.T) {
	_, err := rawEnv([]any{"MCP_BAD=value"})
	if err == nil {
		t.Fatal("expected non-object env to be rejected")
	}
}

func TestRawEnvRejectsInvalidKeys(t *testing.T) {
	for _, raw := range []any{
		map[string]any{"BAD=KEY": "value"},
		map[string]string{"BAD=KEY": "value"},
	} {
		if _, err := rawEnv(raw); err == nil {
			t.Fatalf("expected invalid env key to be rejected for %#v", raw)
		}
	}
}

func TestExecRunAllowsRawCommandOutsideAllowedRootsWhenUnsafeAllowAllEnabled(t *testing.T) {
	allowedRoot := t.TempDir()
	workdir := t.TempDir()
	cfg := config.Config{
		AllowedRoots:     []string{allowedRoot},
		StartupDirectory: allowedRoot,
		OutputMaxBytes:   4096,
		CommandTimeout:   5 * time.Second,
		UnsafeAllowAll:   true,
	}
	tool := findTool(t, cfg, "exec.run")
	res, err := tool.Call(context.Background(), mcp.CallContext{}, map[string]any{
		"command": "sh",
		"args":    []any{"-c", "printf %s \"$MCP_YOLO_TEST\""},
		"env": map[string]any{
			"MCP_YOLO_TEST": "raw-ok",
		},
		"workdir": workdir,
	})
	if err != nil {
		t.Fatalf("exec.run raw command failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %#v", res)
	}
	if text := strings.TrimSpace(res.Content[0].Text); text != "raw-ok" {
		t.Fatalf("unexpected stdout: %q", text)
	}
	if res.StructuredContent["mode"] != "raw" {
		t.Fatalf("expected raw mode in structured content, got %#v", res.StructuredContent)
	}
	envKeys, ok := res.StructuredContent["env_keys"].([]string)
	if !ok || len(envKeys) != 1 || envKeys[0] != "MCP_YOLO_TEST" {
		t.Fatalf("unexpected env_keys: %#v", res.StructuredContent)
	}
}
