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
