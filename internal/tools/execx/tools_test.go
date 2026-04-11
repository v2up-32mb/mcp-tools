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
