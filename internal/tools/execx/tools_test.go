package execx

import (
	"testing"
	"time"

	"github.com/example/mcp-tools/internal/config"
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
