package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadWithYAMLAndEnvOverride(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "mcp-tools.yaml")
	auditPath := filepath.Join(root, "logs", "audit.jsonl")
	if err := os.MkdirAll(filepath.Join(root, "extra"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `listen_addr: 127.0.0.1:9999
bearer_token: from-yaml
log_level: DEBUG
allowed_roots:
  - ./extra
allowed_origins:
  - https://ui.example
  - https://ui.example
audit_log_path: ./logs/audit.jsonl
audit_rotate_max_mb: 12
audit_rotate_max_backups: 7
command_timeout_sec: 41
output_max_bytes: 1234
stream_queue_size: 55
max_request_bytes: 7777
read_header_timeout_sec: 6
read_timeout_sec: 16
write_timeout_sec: 26
idle_timeout_sec: 36
session_ttl_min: 15
server_name: yaml-server
server_version: 1.2.3
supported_protocols:
  - 2025-11-25
  - 2025-03-26
git:
  allowed_subcommands:
    - status
    - diff
exec:
  presets:
    go_test:
      allowed_args: ["-v"]
      timeout_sec: 11
    go_fmt:
      enabled: false
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MCP_LISTEN_ADDR", "0.0.0.0:8080")
	t.Setenv("MCP_BEARER_TOKEN", "from-env")
	t.Setenv("MCP_SESSION_TTL_MIN", "60")

	cfg, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	if cfg.ListenAddr != "0.0.0.0:8080" {
		t.Fatalf("env override failed: %s", cfg.ListenAddr)
	}
	if cfg.BearerToken != "from-env" {
		t.Fatalf("expected env token, got %q", cfg.BearerToken)
	}
	if cfg.LogLevel != "DEBUG" {
		t.Fatalf("expected log level DEBUG, got %q", cfg.LogLevel)
	}
	if cfg.AuditLogPath != auditPath {
		t.Fatalf("unexpected audit path: %s", cfg.AuditLogPath)
	}
	if cfg.AuditRotateMaxMB != 12 || cfg.AuditRotateMaxBackups != 7 {
		t.Fatalf("unexpected audit rotation config: mb=%d backups=%d", cfg.AuditRotateMaxMB, cfg.AuditRotateMaxBackups)
	}
	if cfg.CommandTimeout != 41*time.Second {
		t.Fatalf("unexpected timeout: %s", cfg.CommandTimeout)
	}
	if cfg.StreamQueueSize != 55 {
		t.Fatalf("unexpected stream queue size: %d", cfg.StreamQueueSize)
	}
	if cfg.MaxRequestBytes != 7777 {
		t.Fatalf("unexpected max request bytes: %d", cfg.MaxRequestBytes)
	}
	if cfg.ReadHeaderTimeout != 6*time.Second || cfg.ReadTimeout != 16*time.Second || cfg.WriteTimeout != 26*time.Second || cfg.IdleTimeout != 36*time.Second {
		t.Fatalf("unexpected server timeouts: %+v", cfg)
	}
	if cfg.SessionTTL != 60*time.Minute {
		t.Fatalf("expected env session ttl override, got %s", cfg.SessionTTL)
	}
	if !cfg.GitAllowed["status"] || !cfg.GitAllowed["diff"] || len(cfg.GitAllowed) != 2 {
		t.Fatalf("unexpected git allowed: %#v", cfg.GitAllowed)
	}
	if _, ok := cfg.ExecPresets["go_fmt"]; ok {
		t.Fatal("expected go_fmt preset to be disabled")
	}
	goTest := cfg.ExecPresets["go_test"]
	if goTest.Timeout != 11*time.Second {
		t.Fatalf("unexpected go_test timeout: %s", goTest.Timeout)
	}
	if !containsPath(cfg.AllowedRoots, root) {
		t.Fatalf("expected startup root in allowed roots, got %#v", cfg.AllowedRoots)
	}
	if !containsPath(cfg.AllowedRoots, filepath.Join(root, "extra")) {
		t.Fatalf("expected yaml root in allowed roots, got %#v", cfg.AllowedRoots)
	}
}

func TestLoadRelativeConfigPathAgainstWorkDir(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("bearer_token: relative\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOptions(LoadOptions{ConfigPath: "config.yaml", WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	if cfg.BearerToken != "relative" {
		t.Fatalf("unexpected token: %q", cfg.BearerToken)
	}
}

func TestLoadRejectsUnknownYAMLFields(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "unknown.yaml")
	content := `bearer_token: t
unsafe_allow_alll: false
exec:
  presets:
    go_test:
      timeout_secs: 5
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected unknown YAML fields to fail")
	}
}

func TestLoadRejectsMultipleYAMLDocuments(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "multiple-docs.yaml")
	content := `bearer_token: t
---
unsafe_allow_all: false
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected multiple YAML documents to fail")
	}
}

func TestLoadAcceptsEmptyYAMLFile(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "empty.yaml")
	if err := os.WriteFile(configPath, []byte("# intentionally empty; use env/defaults\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCP_BEARER_TOKEN", "from-env")

	cfg, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	if cfg.BearerToken != "from-env" {
		t.Fatalf("expected env token, got %q", cfg.BearerToken)
	}
}

func TestLoadRejectsExplicitNullYAMLFile(t *testing.T) {
	for name, content := range map[string]string{
		"null":  "null\n",
		"tilde": "~\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			configPath := filepath.Join(root, "null.yaml")
			if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("MCP_BEARER_TOKEN", "from-env")

			_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
			if err == nil {
				t.Fatal("expected explicit null YAML document to fail")
			}
		})
	}
}

func TestLoadRejectsExplicitNullYAMLField(t *testing.T) {
	for name, content := range map[string]string{
		"top_level":   "bearer_token: t\nunsafe_allow_all: null\n",
		"nested":      "bearer_token: t\nexec:\n  presets:\n    go_test:\n      command: null\n",
		"list_item":   "bearer_token: t\nallowed_roots:\n  - null\n",
		"empty_field": "bearer_token: t\nunsafe_allow_all:\n",
		"empty_list":  "bearer_token: t\nallowed_roots:\n  -\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			configPath := filepath.Join(root, "null-field.yaml")
			if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
			if err == nil {
				t.Fatal("expected explicit null YAML field to fail")
			}
			if !strings.Contains(err.Error(), "null") {
				t.Fatalf("expected null context in error, got %v", err)
			}
		})
	}
}

func TestLoadRejectsEmptyAllowedRootEntry(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "bad-root.yaml")
	content := "bearer_token: t\nallowed_roots: [\"\"]\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected empty allowed_roots entry to fail")
	}
}

func TestLoadRejectsEmptyEnvAllowedRootEntry(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MCP_BEARER_TOKEN", "env-token")
	t.Setenv("MCP_ALLOWED_ROOTS", filepath.Join(root, "extra")+",")

	_, err := LoadWithOptions(LoadOptions{WorkDir: root})
	if err == nil {
		t.Fatal("expected empty MCP_ALLOWED_ROOTS entry to fail")
	}
	if !strings.Contains(err.Error(), "MCP_ALLOWED_ROOTS") {
		t.Fatalf("expected MCP_ALLOWED_ROOTS context, got %v", err)
	}
}

func TestLoadRejectsEmptyAllowedOriginEntry(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "bad-origin.yaml")
	content := "bearer_token: t\nallowed_origins: [\"https://ui.example\", \"\"]\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected empty allowed_origins entry to fail")
	}
	if !strings.Contains(err.Error(), "allowed_origins") {
		t.Fatalf("expected allowed_origins context, got %v", err)
	}
}

func TestLoadRejectsEmptyEnvAllowedOriginEntry(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MCP_BEARER_TOKEN", "env-token")
	t.Setenv("MCP_ALLOWED_ORIGINS", "https://ui.example,")

	_, err := LoadWithOptions(LoadOptions{WorkDir: root})
	if err == nil {
		t.Fatal("expected empty MCP_ALLOWED_ORIGINS entry to fail")
	}
	if !strings.Contains(err.Error(), "MCP_ALLOWED_ORIGINS") {
		t.Fatalf("expected MCP_ALLOWED_ORIGINS context, got %v", err)
	}
}

func TestLoadResolvesTemplateAllowedWorkdirsRelativeToConfigFile(t *testing.T) {
	startup := t.TempDir()
	configDir := t.TempDir()
	workspace := filepath.Join(configDir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.yaml")
	content := `bearer_token: t
allowed_roots:
  - ./workspace
exec:
  command_templates:
    local:
      command: [pwd]
      allowed_workdirs: ["./workspace"]
      timeout_sec: 5
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: startup})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	got := cfg.CommandTemplates["local"].AllowedWorkdirs
	if len(got) != 1 || got[0] != workspace {
		t.Fatalf("expected allowed_workdirs to resolve against config dir, got %#v want %q", got, workspace)
	}
}

func TestLoadRejectsEmptyTemplateAllowedWorkdir(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "bad-template-workdir.yaml")
	content := "bearer_token: t\nexec:\n  command_templates:\n    bad:\n      command: [pwd]\n      allowed_workdirs: [\"\"]\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected empty command template allowed_workdirs entry to fail")
	}
}

func TestLoadKeepsBareExecCommandsAsPathLookups(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "commands.yaml")
	content := `bearer_token: t
exec:
  presets:
    custom_go:
      command: go
      timeout_sec: 5
  command_templates:
    custom_make:
      command: [make, test]
      timeout_sec: 5
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	if got := cfg.ExecPresets["custom_go"].Command; got != "go" {
		t.Fatalf("expected bare preset command to stay PATH lookup, got %q", got)
	}
	if got := cfg.CommandTemplates["custom_make"].Command[0]; got != "make" {
		t.Fatalf("expected bare template command to stay PATH lookup, got %q", got)
	}
}

func TestLoadResolvesPathLikeExecCommandsRelativeToConfigFile(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "commands.yaml")
	content := `bearer_token: t
exec:
  presets:
    custom_tool:
      command: ./bin/tool
      timeout_sec: 5
  command_templates:
    custom_template:
      command: [./bin/template-tool, arg]
      timeout_sec: 5
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	wantPreset := filepath.Join(root, "bin", "tool")
	if got := cfg.ExecPresets["custom_tool"].Command; got != wantPreset {
		t.Fatalf("expected path-like preset command %q, got %q", wantPreset, got)
	}
	wantTemplate := filepath.Join(root, "bin", "template-tool")
	if got := cfg.CommandTemplates["custom_template"].Command[0]; got != wantTemplate {
		t.Fatalf("expected path-like template command %q, got %q", wantTemplate, got)
	}
}

func TestLoadRejectsUnsupportedGitCommand(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "bad.yaml")
	if err := os.WriteFile(configPath, []byte("bearer_token: t\ngit:\n  allowed_subcommands: [push]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected unsupported git command to fail")
	}
}

func TestLoadAllowsEmptyGitSubcommandWhitelist(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "no-git.yaml")
	if err := os.WriteFile(configPath, []byte("bearer_token: t\ngit:\n  allowed_subcommands: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	if len(cfg.GitAllowed) != 0 {
		t.Fatalf("expected empty git whitelist, got %#v", cfg.GitAllowed)
	}
}

func TestLoadRejectsEmptyGitSubcommandEntry(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "bad-git-empty.yaml")
	if err := os.WriteFile(configPath, []byte("bearer_token: t\ngit:\n  allowed_subcommands: [status, \"\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected empty git subcommand entry to fail")
	}
}

func TestLoadRejectsBlockedExecArg(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "bad-exec.yaml")
	content := "bearer_token: t\nexec:\n  presets:\n    go_vet:\n      allowed_args: [\"-tags\", \"-vettool\"]\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected blocked exec arg to fail")
	}
}

func TestLoadRejectsEmptyExecPresetArgEntries(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name:    "fixed_args",
			content: "bearer_token: t\nexec:\n  presets:\n    go_test:\n      fixed_args: [\"test\", \"\"]\n",
			wantErr: "fixed_args",
		},
		{
			name:    "allowed_args",
			content: "bearer_token: t\nexec:\n  presets:\n    go_test:\n      allowed_args: [\"-run\", \" \"]\n",
			wantErr: "allowed_args",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			configPath := filepath.Join(root, "bad-exec-"+tt.name+".yaml")
			if err := os.WriteFile(configPath, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
			if err == nil {
				t.Fatal("expected empty exec preset arg entry to fail")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error mentioning %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestLoadRejectsEmptyExecPresetCommandOverride(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "bad-exec-command.yaml")
	content := "bearer_token: t\nexec:\n  presets:\n    go_test:\n      command: \"\"\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected empty exec preset command override to fail")
	}
}

func TestLoadRejectsInvalidExecEnvKey(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "bad-exec-env.yaml")
	content := "bearer_token: t\nexec:\n  presets:\n    bad:\n      command: go\n      env:\n        BAD=KEY: value\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected invalid exec env key to fail")
	}
}

func TestLoadRejectsInvalidTemplateEnvKey(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "bad-template-env.yaml")
	content := "bearer_token: t\nexec:\n  command_templates:\n    bad:\n      command: [pwd]\n      env:\n        BAD=KEY: value\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected invalid command template env key to fail")
	}
}

func TestLoadRejectsEmptyTemplateCommandFirstPart(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "bad-template-command.yaml")
	content := "bearer_token: t\nexec:\n  command_templates:\n    bad:\n      command: [\"\"]\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected empty template command first part to fail")
	}
}

func TestLoadEnvOverridesServerLimits(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MCP_BEARER_TOKEN", "env-token")
	t.Setenv("MCP_LOG_LEVEL", "debug")
	t.Setenv("MCP_MAX_REQUEST_BYTES", "2048")
	t.Setenv("MCP_STREAM_QUEUE_SIZE", "256")
	t.Setenv("MCP_COMMAND_TIMEOUT_SEC", "19")
	t.Setenv("MCP_READ_HEADER_TIMEOUT_SEC", "7")
	t.Setenv("MCP_READ_TIMEOUT_SEC", "17")
	t.Setenv("MCP_WRITE_TIMEOUT_SEC", "27")
	t.Setenv("MCP_IDLE_TIMEOUT_SEC", "37")
	t.Setenv("MCP_AUDIT_ROTATE_MAX_MB", "9")
	t.Setenv("MCP_AUDIT_ROTATE_MAX_BACKUPS", "4")

	cfg, err := LoadWithOptions(LoadOptions{WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	if cfg.MaxRequestBytes != 2048 {
		t.Fatalf("unexpected MaxRequestBytes: %d", cfg.MaxRequestBytes)
	}
	if cfg.LogLevel != "DEBUG" {
		t.Fatalf("expected DEBUG log level from env override, got %q", cfg.LogLevel)
	}
	if cfg.StreamQueueSize != 256 {
		t.Fatalf("unexpected StreamQueueSize: %d", cfg.StreamQueueSize)
	}
	if cfg.CommandTimeout != 19*time.Second {
		t.Fatalf("unexpected CommandTimeout: %s", cfg.CommandTimeout)
	}
	if got := cfg.ExecPresets["go_test"].Timeout; got != 19*time.Second {
		t.Fatalf("expected env command timeout to update default exec preset timeout, got %s", got)
	}
	if got := cfg.CommandTemplates["make_build"].Timeout; got != 19*time.Second {
		t.Fatalf("expected env command timeout to update default command template timeout, got %s", got)
	}
	if cfg.AuditRotateMaxMB != 9 || cfg.AuditRotateMaxBackups != 4 {
		t.Fatalf("unexpected audit rotate overrides: mb=%d backups=%d", cfg.AuditRotateMaxMB, cfg.AuditRotateMaxBackups)
	}
	if cfg.ReadHeaderTimeout != 7*time.Second || cfg.ReadTimeout != 17*time.Second || cfg.WriteTimeout != 27*time.Second || cfg.IdleTimeout != 37*time.Second {
		t.Fatalf("unexpected timeout overrides: %+v", cfg)
	}
}

func TestLoadRejectsInvalidLogLevel(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "bad-log.yaml")
	if err := os.WriteFile(configPath, []byte("bearer_token: t\nlog_level: verbose\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected invalid log level to fail")
	}
}

func TestLoadYAMLCommandTimeoutUpdatesDefaultExecTimeouts(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "timeout.yaml")
	content := "bearer_token: t\ncommand_timeout_sec: 7\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	if cfg.CommandTimeout != 7*time.Second {
		t.Fatalf("unexpected command timeout: %s", cfg.CommandTimeout)
	}
	if got := cfg.ExecPresets["go_build"].Timeout; got != 7*time.Second {
		t.Fatalf("expected default exec preset timeout to inherit command_timeout_sec, got %s", got)
	}
	if got := cfg.CommandTemplates["make_test"].Timeout; got != 7*time.Second {
		t.Fatalf("expected default command template timeout to inherit command_timeout_sec, got %s", got)
	}
}

func TestLoadRejectsEmptySupportedProtocols(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "bad-protocols.yaml")
	content := "bearer_token: t\nsupported_protocols: [\"\"]\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected empty supported_protocols to fail")
	}
}

func TestLoadRejectsExplicitEmptySupportedProtocols(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "bad-empty-protocols.yaml")
	content := "bearer_token: t\nsupported_protocols: []\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected explicit empty supported_protocols to fail")
	}
}

func TestLoadRejectsMixedEmptySupportedProtocols(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "bad-mixed-protocols.yaml")
	content := "bearer_token: t\nsupported_protocols: [\"2025-11-25\", \"\"]\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected mixed empty supported_protocols to fail")
	}
}

func TestLoadRejectsOverflowingYAMLDuration(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "bad-timeout.yaml")
	content := "bearer_token: t\ncommand_timeout_sec: 9223372037\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected overflowing command_timeout_sec to fail")
	}
}

func TestLoadRejectsOverflowingEnvDuration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MCP_BEARER_TOKEN", "env-token")
	t.Setenv("MCP_COMMAND_TIMEOUT_SEC", "9223372037")
	_, err := LoadWithOptions(LoadOptions{WorkDir: root})
	if err == nil {
		t.Fatal("expected overflowing MCP_COMMAND_TIMEOUT_SEC to fail")
	}
}

func TestLoadRejectsAuditRotateSizeOverflow(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "bad-audit-rotate.yaml")
	content := "bearer_token: t\naudit_rotate_max_mb: 8796093022208\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err == nil {
		t.Fatal("expected overflowing audit_rotate_max_mb to fail")
	}
}

func TestLoadDefaultsUnsafeAllowAllOnYoloBranch(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MCP_BEARER_TOKEN", "env-token")

	cfg, err := LoadWithOptions(LoadOptions{WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	if !cfg.UnsafeAllowAll {
		t.Fatalf("expected UnsafeAllowAll to default true on yolo branch, got %#v", cfg)
	}
}

func TestLoadUsesDefaultHomeConfigPathWhenPresent(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	setTestHomeDir(t, home)
	configDir := filepath.Join(home, ".mcp-tools")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.yaml")
	content := `bearer_token: home-token
listen_addr: 127.0.0.1:9090
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadWithOptions(LoadOptions{WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	if cfg.BearerToken != "home-token" {
		t.Fatalf("expected token from default home config, got %q", cfg.BearerToken)
	}
	if cfg.ListenAddr != "127.0.0.1:9090" {
		t.Fatalf("unexpected listen addr: %s", cfg.ListenAddr)
	}
	if cfg.AuditLogPath != filepath.Join(home, ".mcp-tools", "mcp-audit.jsonl") {
		t.Fatalf("expected default audit log path under home config dir, got %q", cfg.AuditLogPath)
	}
	if !containsPath(cfg.AllowedRoots, filepath.Join(home, ".mcp-tools")) {
		t.Fatalf("expected allowed roots to include home config dir, got %#v", cfg.AllowedRoots)
	}
}

func TestLoadEnvAllowedRootsStillPreservesHomeConfigDir(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	extra := filepath.Join(root, "extra")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	setTestHomeDir(t, home)
	t.Setenv("MCP_BEARER_TOKEN", "env-token")
	t.Setenv("MCP_ALLOWED_ROOTS", extra)

	cfg, err := LoadWithOptions(LoadOptions{WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	if !containsPath(cfg.AllowedRoots, root) {
		t.Fatalf("expected startup dir in allowed roots, got %#v", cfg.AllowedRoots)
	}
	if !containsPath(cfg.AllowedRoots, filepath.Join(home, ".mcp-tools")) {
		t.Fatalf("expected home config dir in allowed roots, got %#v", cfg.AllowedRoots)
	}
	if !containsPath(cfg.AllowedRoots, extra) {
		t.Fatalf("expected env extra root in allowed roots, got %#v", cfg.AllowedRoots)
	}
}

func TestLoadDefaultAuditLogPathUsesHomeConfigDirWithoutConfigFile(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	setTestHomeDir(t, home)
	t.Setenv("MCP_BEARER_TOKEN", "env-token")

	cfg, err := LoadWithOptions(LoadOptions{WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	want := filepath.Join(home, ".mcp-tools", "mcp-audit.jsonl")
	if cfg.AuditLogPath != want {
		t.Fatalf("expected audit log path %q, got %q", want, cfg.AuditLogPath)
	}
}

func setTestHomeDir(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

func containsPath(values []string, target string) bool {
	cleanTarget := filepath.Clean(target)
	for _, value := range values {
		if filepath.Clean(value) == cleanTarget {
			return true
		}
	}
	return false
}

func TestLoadDefaultCommandTemplatesPresent(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	setTestHomeDir(t, home)
	t.Setenv("MCP_BEARER_TOKEN", "env-token")

	cfg, err := LoadWithOptions(LoadOptions{WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	for _, name := range []string{"make_test", "make_build", "go_clean_testcache"} {
		if _, ok := cfg.CommandTemplates[name]; !ok {
			t.Fatalf("expected default command template %q, got %#v", name, cfg.CommandTemplates)
		}
	}
	if cfg.CommandTemplates["make_test"].Category != "test" || cfg.CommandTemplates["make_test"].Destructive {
		t.Fatalf("unexpected make_test metadata: %#v", cfg.CommandTemplates["make_test"])
	}
	if cfg.CommandTemplates["go_clean_testcache"].Category != "cleanup" || !cfg.CommandTemplates["go_clean_testcache"].Destructive || !cfg.CommandTemplates["go_clean_testcache"].RequiresConfirmation {
		t.Fatalf("unexpected go_clean_testcache metadata: %#v", cfg.CommandTemplates["go_clean_testcache"])
	}
}

func TestLoadDefaultExecPresetsIncludeManagedGoCacheEnv(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	setTestHomeDir(t, home)
	t.Setenv("MCP_BEARER_TOKEN", "env-token")

	cfg, err := LoadWithOptions(LoadOptions{WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}

	for _, name := range []string{"go_get", "go_list", "go_work_sync"} {
		if _, ok := cfg.ExecPresets[name]; !ok {
			t.Fatalf("expected default exec preset %q, got %#v", name, cfg.ExecPresets)
		}
	}

	goGet := cfg.ExecPresets["go_get"]
	wantCacheRoot := filepath.Join(home, ".mcp-tools", "cache")
	if got := goGet.Env["GOCACHE"]; got != filepath.Join(wantCacheRoot, "go-build") {
		t.Fatalf("unexpected GOCACHE: %q", got)
	}
	if got := goGet.Env["GOMODCACHE"]; got != filepath.Join(wantCacheRoot, "gomod") {
		t.Fatalf("unexpected GOMODCACHE: %q", got)
	}
	if got := goGet.Env["GOTMPDIR"]; got != filepath.Join(wantCacheRoot, "tmp") {
		t.Fatalf("unexpected GOTMPDIR: %q", got)
	}
}

func TestLoadPullFileDefaultsAndOverrides(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "pull.yaml")
	content := `bearer_token: t
pull_file:
  allowed_extensions: [".PNG", ".JPG"]
  max_bytes: 2048
  url:
    ttl_sec: 120
    max_downloads: 3
    public_base_url: "https://files.example.com/mcp-tools/"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	if !cfg.PullFile.Enabled {
		t.Fatal("pull_file should default to enabled")
	}
	if cfg.PullFile.MaxBytes != 2048 {
		t.Fatalf("max_bytes=%d", cfg.PullFile.MaxBytes)
	}
	if cfg.PullFile.TTLSeconds != 120 {
		t.Fatalf("ttl_sec=%d", cfg.PullFile.TTLSeconds)
	}
	if cfg.PullFile.MaxDownloads != 3 {
		t.Fatalf("max_downloads=%d", cfg.PullFile.MaxDownloads)
	}
	if cfg.PullFile.PublicBaseURL != "https://files.example.com/mcp-tools" {
		t.Fatalf("public_base_url=%q", cfg.PullFile.PublicBaseURL)
	}
	// Extensions are lowercased.
	if len(cfg.PullFile.AllowedExtensions) != 2 || cfg.PullFile.AllowedExtensions[0] != ".png" || cfg.PullFile.AllowedExtensions[1] != ".jpg" {
		t.Fatalf("allowed_extensions=%#v", cfg.PullFile.AllowedExtensions)
	}
}

func TestLoadPullFileDefaultsWhenAbsent(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "basic.yaml")
	if err := os.WriteFile(configPath, []byte("bearer_token: t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	if !cfg.PullFile.Enabled || cfg.PullFile.MaxBytes != 10*1024*1024 || cfg.PullFile.TTLSeconds != 300 || cfg.PullFile.MaxDownloads != 0 || cfg.PullFile.PublicBaseURL != "" {
		t.Fatalf("unexpected pull_file defaults: %+v", cfg.PullFile)
	}
	if len(cfg.PullFile.AllowedExtensions) != 0 {
		t.Fatalf("expected empty allowed_extensions by default, got %#v", cfg.PullFile.AllowedExtensions)
	}
}

func TestLoadRejectsInvalidPullFileExtension(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		"no-dot.yaml":      "bearer_token: t\npull_file:\n  allowed_extensions: [png]\n",
		"slash.yaml":       "bearer_token: t\npull_file:\n  allowed_extensions: [\".a/b\"]\n",
		"empty.yaml":       "bearer_token: t\npull_file:\n  allowed_extensions: [\"\"]\n",
		"inner-space.yaml": "bearer_token: t\npull_file:\n  allowed_extensions: [\".p ng\"]\n",
	} {
		t.Run(name, func(t *testing.T) {
			configPath := filepath.Join(root, name)
			if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root}); err == nil {
				t.Fatal("expected invalid allowed_extensions to fail")
			}
		})
	}
}

func TestLoadRejectsInvalidPullFileNumbers(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		"max-bytes-zero.yaml":         "bearer_token: t\npull_file:\n  max_bytes: 0\n",
		"ttl-zero.yaml":               "bearer_token: t\npull_file:\n  url:\n    ttl_sec: 0\n",
		"max-downloads-negative.yaml": "bearer_token: t\npull_file:\n  url:\n    max_downloads: -1\n",
	} {
		t.Run(name, func(t *testing.T) {
			configPath := filepath.Join(root, name)
			if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root}); err == nil {
				t.Fatal("expected invalid pull_file number to fail")
			}
		})
	}
}

func TestLoadRejectsInvalidPullFilePublicBaseURL(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		"not-url.yaml": "bearer_token: t\npull_file:\n  url:\n    public_base_url: \"files.example.com\"\n",
		"query.yaml":   "bearer_token: t\npull_file:\n  url:\n    public_base_url: \"https://files.example.com?x=1\"\n",
		"ftp.yaml":     "bearer_token: t\npull_file:\n  url:\n    public_base_url: \"ftp://files.example.com\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			configPath := filepath.Join(root, name)
			if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root}); err == nil {
				t.Fatal("expected invalid public_base_url to fail")
			}
		})
	}
}

func TestLoadPullFileExtensionTrimsWhitespace(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "trim.yaml")
	content := "bearer_token: t\npull_file:\n  allowed_extensions: [\" .png \"]\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOptions(LoadOptions{ConfigPath: configPath, WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	if len(cfg.PullFile.AllowedExtensions) != 1 || cfg.PullFile.AllowedExtensions[0] != ".png" {
		t.Fatalf("expected trimmed .png, got %#v", cfg.PullFile.AllowedExtensions)
	}
}
