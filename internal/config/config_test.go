package config

import (
	"os"
	"path/filepath"
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
debug_http_log: true
allowed_roots:
  - ./extra
allowed_origins:
  - https://ui.example
  - https://ui.example
audit_log_path: ./logs/audit.jsonl
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
	if !cfg.DebugHTTPLog {
		t.Fatal("expected debug http log from yaml")
	}
	if cfg.AuditLogPath != auditPath {
		t.Fatalf("unexpected audit path: %s", cfg.AuditLogPath)
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

func TestLoadEnvOverridesServerLimits(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MCP_BEARER_TOKEN", "env-token")
	t.Setenv("MCP_DEBUG_HTTP_LOG", "true")
	t.Setenv("MCP_MAX_REQUEST_BYTES", "2048")
	t.Setenv("MCP_STREAM_QUEUE_SIZE", "256")
	t.Setenv("MCP_READ_HEADER_TIMEOUT_SEC", "7")
	t.Setenv("MCP_READ_TIMEOUT_SEC", "17")
	t.Setenv("MCP_WRITE_TIMEOUT_SEC", "27")
	t.Setenv("MCP_IDLE_TIMEOUT_SEC", "37")

	cfg, err := LoadWithOptions(LoadOptions{WorkDir: root})
	if err != nil {
		t.Fatalf("LoadWithOptions error: %v", err)
	}
	if cfg.MaxRequestBytes != 2048 {
		t.Fatalf("unexpected MaxRequestBytes: %d", cfg.MaxRequestBytes)
	}
	if !cfg.DebugHTTPLog {
		t.Fatal("expected debug http log env override")
	}
	if cfg.StreamQueueSize != 256 {
		t.Fatalf("unexpected StreamQueueSize: %d", cfg.StreamQueueSize)
	}
	if cfg.ReadHeaderTimeout != 7*time.Second || cfg.ReadTimeout != 17*time.Second || cfg.WriteTimeout != 27*time.Second || cfg.IdleTimeout != 37*time.Second {
		t.Fatalf("unexpected timeout overrides: %+v", cfg)
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
