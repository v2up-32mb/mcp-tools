package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	yaml "gopkg.in/yaml.v3"
)

const (
	ProtocolLatest = "2025-11-25"
	ProtocolCompat = "2025-06-18"
	ProtocolLegacy = "2025-03-26"
	ProtocolOld    = "2024-11-05"
)

var knownGitSubcommands = map[string]bool{
	"status":  true,
	"diff":    true,
	"log":     true,
	"add":     true,
	"restore": true,
	"commit":  true,
	"branch":  true,
	"switch":  true,
	"pull":    true,
}

type Config struct {
	ListenAddr         string
	BearerToken        string
	DebugHTTPLog       bool
	AllowedRoots       []string
	AllowedOrigins     []string
	AuditLogPath       string
	CommandTimeout     time.Duration
	OutputMaxBytes     int
	StreamQueueSize    int
	MaxRequestBytes    int64
	ReadHeaderTimeout  time.Duration
	ReadTimeout        time.Duration
	WriteTimeout       time.Duration
	IdleTimeout        time.Duration
	GitAllowed         map[string]bool
	ExecPresets        map[string]ExecPreset
	StartupDirectory   string
	SessionTTL         time.Duration
	ServerName         string
	ServerVersion      string
	SupportedProtocols []string
}

type ExecPreset struct {
	Command     string
	FixedArgs   []string
	AllowedArgs []string
	Timeout     time.Duration
	ReadOnly    bool
}

type LoadOptions struct {
	ConfigPath string
	WorkDir    string
}

type fileConfig struct {
	ListenAddr           string         `yaml:"listen_addr"`
	BearerToken          string         `yaml:"bearer_token"`
	DebugHTTPLog         *bool          `yaml:"debug_http_log"`
	AllowedRoots         []string       `yaml:"allowed_roots"`
	AllowedOrigins       []string       `yaml:"allowed_origins"`
	AuditLogPath         string         `yaml:"audit_log_path"`
	CommandTimeoutSec    *int           `yaml:"command_timeout_sec"`
	OutputMaxBytes       *int           `yaml:"output_max_bytes"`
	StreamQueueSize      *int           `yaml:"stream_queue_size"`
	MaxRequestBytes      *int64         `yaml:"max_request_bytes"`
	ReadHeaderTimeoutSec *int           `yaml:"read_header_timeout_sec"`
	ReadTimeoutSec       *int           `yaml:"read_timeout_sec"`
	WriteTimeoutSec      *int           `yaml:"write_timeout_sec"`
	IdleTimeoutSec       *int           `yaml:"idle_timeout_sec"`
	SessionTTLMin        *int           `yaml:"session_ttl_min"`
	ServerName           string         `yaml:"server_name"`
	ServerVersion        string         `yaml:"server_version"`
	SupportedProtocols   []string       `yaml:"supported_protocols"`
	Git                  fileGitConfig  `yaml:"git"`
	Exec                 fileExecConfig `yaml:"exec"`
}

type fileGitConfig struct {
	AllowedSubcommands []string `yaml:"allowed_subcommands"`
}

type fileExecConfig struct {
	Presets map[string]fileExecPreset `yaml:"presets"`
}

type fileExecPreset struct {
	Enabled     *bool    `yaml:"enabled"`
	Command     string   `yaml:"command"`
	FixedArgs   []string `yaml:"fixed_args"`
	AllowedArgs []string `yaml:"allowed_args"`
	TimeoutSec  *int     `yaml:"timeout_sec"`
	ReadOnly    *bool    `yaml:"read_only"`
}

func Load() (Config, error) {
	return LoadWithOptions(LoadOptions{})
}

func LoadWithOptions(opts LoadOptions) (Config, error) {
	cwd := opts.WorkDir
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return Config{}, fmt.Errorf("getwd: %w", err)
		}
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return Config{}, fmt.Errorf("abs workdir: %w", err)
	}
	cwd = filepath.Clean(cwd)

	cfg := defaultConfig(cwd)
	configPath := strings.TrimSpace(firstNonEmpty(opts.ConfigPath, os.Getenv("MCP_CONFIG_FILE")))
	if configPath != "" {
		resolvedConfigPath, err := resolveConfigPathAgainst(cwd, configPath)
		if err != nil {
			return Config{}, err
		}
		if err := applyYAMLFile(&cfg, resolvedConfigPath); err != nil {
			return Config{}, err
		}
	} else if resolvedDefaultPath, ok := defaultConfigPath(); ok {
		if _, err := os.Stat(resolvedDefaultPath); err == nil {
			if err := applyYAMLFile(&cfg, resolvedDefaultPath); err != nil {
				return Config{}, err
			}
		}
	}
	if err := applyEnv(&cfg, cwd); err != nil {
		return Config{}, err
	}
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	cfg.AllowedRoots = mergeUniquePaths(nil, cfg.AllowedRoots)
	cfg.AllowedOrigins = dedupeStrings(cfg.AllowedOrigins)
	cfg.SupportedProtocols = dedupeStrings(cfg.SupportedProtocols)
	return cfg, nil
}

func defaultConfig(cwd string) Config {
	timeout := 30 * time.Second
	return Config{
		ListenAddr:         "0.0.0.0:8080",
		AllowedRoots:       defaultAllowedRoots(cwd),
		AuditLogPath:       defaultAuditLogPath(cwd),
		CommandTimeout:     timeout,
		OutputMaxBytes:     65536,
		StreamQueueSize:    128,
		MaxRequestBytes:    1 << 20,
		ReadHeaderTimeout:  5 * time.Second,
		ReadTimeout:        15 * time.Second,
		WriteTimeout:       30 * time.Second,
		IdleTimeout:        60 * time.Second,
		GitAllowed:         defaultGitAllowed(),
		ExecPresets:        defaultExecPresets(timeout),
		StartupDirectory:   cwd,
		SessionTTL:         120 * time.Minute,
		ServerName:         "mcp-tools",
		ServerVersion:      "0.1.0",
		SupportedProtocols: []string{ProtocolLatest, ProtocolCompat, ProtocolLegacy, ProtocolOld},
	}
}

func applyYAMLFile(cfg *Config, path string) error {
	payload, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config file %q: %w", path, err)
	}
	var fc fileConfig
	if err := yaml.Unmarshal(payload, &fc); err != nil {
		return fmt.Errorf("parse config file %q: %w", path, err)
	}
	baseDir := filepath.Dir(path)
	if fc.ListenAddr != "" {
		cfg.ListenAddr = fc.ListenAddr
	}
	if fc.BearerToken != "" {
		cfg.BearerToken = fc.BearerToken
	}
	if fc.DebugHTTPLog != nil {
		cfg.DebugHTTPLog = *fc.DebugHTTPLog
	}
	if len(fc.AllowedRoots) > 0 {
		roots, err := resolvePathList(fc.AllowedRoots, baseDir)
		if err != nil {
			return err
		}
		cfg.AllowedRoots = mergeUniquePaths(cfg.AllowedRoots, roots)
	}
	if len(fc.AllowedOrigins) > 0 {
		cfg.AllowedOrigins = dedupeStrings(fc.AllowedOrigins)
	}
	if fc.AuditLogPath != "" {
		cfg.AuditLogPath = resolveMaybeRelative(baseDir, fc.AuditLogPath)
	}
	if fc.CommandTimeoutSec != nil {
		cfg.CommandTimeout = time.Duration(*fc.CommandTimeoutSec) * time.Second
	}
	if fc.OutputMaxBytes != nil {
		cfg.OutputMaxBytes = *fc.OutputMaxBytes
	}
	if fc.StreamQueueSize != nil {
		cfg.StreamQueueSize = *fc.StreamQueueSize
	}
	if fc.MaxRequestBytes != nil {
		cfg.MaxRequestBytes = *fc.MaxRequestBytes
	}
	if fc.ReadHeaderTimeoutSec != nil {
		cfg.ReadHeaderTimeout = time.Duration(*fc.ReadHeaderTimeoutSec) * time.Second
	}
	if fc.ReadTimeoutSec != nil {
		cfg.ReadTimeout = time.Duration(*fc.ReadTimeoutSec) * time.Second
	}
	if fc.WriteTimeoutSec != nil {
		cfg.WriteTimeout = time.Duration(*fc.WriteTimeoutSec) * time.Second
	}
	if fc.IdleTimeoutSec != nil {
		cfg.IdleTimeout = time.Duration(*fc.IdleTimeoutSec) * time.Second
	}
	if fc.SessionTTLMin != nil {
		cfg.SessionTTL = time.Duration(*fc.SessionTTLMin) * time.Minute
	}
	if fc.ServerName != "" {
		cfg.ServerName = fc.ServerName
	}
	if fc.ServerVersion != "" {
		cfg.ServerVersion = fc.ServerVersion
	}
	if len(fc.SupportedProtocols) > 0 {
		cfg.SupportedProtocols = dedupeStrings(fc.SupportedProtocols)
	}
	if len(fc.Git.AllowedSubcommands) > 0 {
		cfg.GitAllowed = make(map[string]bool, len(fc.Git.AllowedSubcommands))
		for _, subcommand := range dedupeStrings(fc.Git.AllowedSubcommands) {
			cfg.GitAllowed[subcommand] = true
		}
	}
	if len(fc.Exec.Presets) > 0 {
		merged, err := mergeExecPresets(cfg.ExecPresets, fc.Exec.Presets, baseDir, cfg.CommandTimeout)
		if err != nil {
			return err
		}
		cfg.ExecPresets = merged
	}
	return nil
}

func applyEnv(cfg *Config, cwd string) error {
	if value := os.Getenv("MCP_BEARER_TOKEN"); value != "" {
		cfg.BearerToken = value
	}
	if value := os.Getenv("MCP_LISTEN_ADDR"); value != "" {
		cfg.ListenAddr = value
	}
	if value := os.Getenv("MCP_DEBUG_HTTP_LOG"); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("invalid MCP_DEBUG_HTTP_LOG")
		}
		cfg.DebugHTTPLog = parsed
	}
	if value := os.Getenv("MCP_ALLOWED_ROOTS"); value != "" {
		roots, err := resolvePathList(splitCSV(value), cwd)
		if err != nil {
			return err
		}
		cfg.AllowedRoots = mergeUniquePaths(defaultAllowedRoots(cfg.StartupDirectory), roots)
	}
	if value := os.Getenv("MCP_ALLOWED_ORIGINS"); value != "" {
		cfg.AllowedOrigins = dedupeStrings(splitCSV(value))
	}
	if value := os.Getenv("MCP_AUDIT_LOG_PATH"); value != "" {
		cfg.AuditLogPath = resolveMaybeRelative(cwd, value)
	}
	if value := os.Getenv("MCP_OUTPUT_MAX_BYTES"); value != "" {
		parsed, err := parsePositiveInt(value, "MCP_OUTPUT_MAX_BYTES")
		if err != nil {
			return err
		}
		cfg.OutputMaxBytes = parsed
	}
	if value := os.Getenv("MCP_STREAM_QUEUE_SIZE"); value != "" {
		parsed, err := parsePositiveInt(value, "MCP_STREAM_QUEUE_SIZE")
		if err != nil {
			return err
		}
		cfg.StreamQueueSize = parsed
	}
	if value := os.Getenv("MCP_MAX_REQUEST_BYTES"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("invalid MCP_MAX_REQUEST_BYTES")
		}
		cfg.MaxRequestBytes = parsed
	}
	if value := os.Getenv("MCP_COMMAND_TIMEOUT_SEC"); value != "" {
		parsed, err := parsePositiveInt(value, "MCP_COMMAND_TIMEOUT_SEC")
		if err != nil {
			return err
		}
		cfg.CommandTimeout = time.Duration(parsed) * time.Second
	}
	if value := os.Getenv("MCP_SESSION_TTL_MIN"); value != "" {
		parsed, err := parsePositiveInt(value, "MCP_SESSION_TTL_MIN")
		if err != nil {
			return err
		}
		cfg.SessionTTL = time.Duration(parsed) * time.Minute
	}
	if value := os.Getenv("MCP_SERVER_NAME"); value != "" {
		cfg.ServerName = value
	}
	if value := os.Getenv("MCP_SERVER_VERSION"); value != "" {
		cfg.ServerVersion = value
	}
	if value := os.Getenv("MCP_READ_HEADER_TIMEOUT_SEC"); value != "" {
		parsed, err := parsePositiveInt(value, "MCP_READ_HEADER_TIMEOUT_SEC")
		if err != nil {
			return err
		}
		cfg.ReadHeaderTimeout = time.Duration(parsed) * time.Second
	}
	if value := os.Getenv("MCP_READ_TIMEOUT_SEC"); value != "" {
		parsed, err := parsePositiveInt(value, "MCP_READ_TIMEOUT_SEC")
		if err != nil {
			return err
		}
		cfg.ReadTimeout = time.Duration(parsed) * time.Second
	}
	if value := os.Getenv("MCP_WRITE_TIMEOUT_SEC"); value != "" {
		parsed, err := parsePositiveInt(value, "MCP_WRITE_TIMEOUT_SEC")
		if err != nil {
			return err
		}
		cfg.WriteTimeout = time.Duration(parsed) * time.Second
	}
	if value := os.Getenv("MCP_IDLE_TIMEOUT_SEC"); value != "" {
		parsed, err := parsePositiveInt(value, "MCP_IDLE_TIMEOUT_SEC")
		if err != nil {
			return err
		}
		cfg.IdleTimeout = time.Duration(parsed) * time.Second
	}
	return nil
}

func defaultGitAllowed() map[string]bool {
	allowed := make(map[string]bool, len(knownGitSubcommands))
	for key, value := range knownGitSubcommands {
		allowed[key] = value
	}
	return allowed
}

func defaultExecPresets(baseTimeout time.Duration) map[string]ExecPreset {
	return map[string]ExecPreset{
		"go_fmt": {
			Command:     "gofmt",
			FixedArgs:   []string{"-w"},
			AllowedArgs: []string{"-s", "-l"},
			Timeout:     baseTimeout,
		},
		"go_mod_download": {
			Command:     "go",
			FixedArgs:   []string{"mod", "download"},
			AllowedArgs: []string{"-x", "-json"},
			Timeout:     baseTimeout,
		},
		"go_test": {
			Command:     "go",
			FixedArgs:   []string{"test"},
			AllowedArgs: []string{"-run", "-count", "-timeout", "-v", "-race", "-cover", "-coverprofile"},
			Timeout:     baseTimeout,
			ReadOnly:    true,
		},
		"go_generate": {
			Command:     "go",
			FixedArgs:   []string{"generate"},
			AllowedArgs: []string{"-run", "-skip", "-v", "-x", "-n", "-tags"},
			Timeout:     baseTimeout,
		},
		"go_build": {
			Command:     "go",
			FixedArgs:   []string{"build"},
			AllowedArgs: []string{"-v", "-race", "-o", "-tags"},
			Timeout:     baseTimeout,
		},
		"go_vet": {
			Command:     "go",
			FixedArgs:   []string{"vet"},
			AllowedArgs: []string{"-tags"},
			Timeout:     baseTimeout,
			ReadOnly:    true,
		},
		"go_mod_tidy": {
			Command:     "go",
			FixedArgs:   []string{"mod", "tidy"},
			AllowedArgs: []string{"-v", "-e", "-diff", "-go", "-compat", "-x"},
			Timeout:     baseTimeout,
		},
	}
}

func mergeExecPresets(base map[string]ExecPreset, overrides map[string]fileExecPreset, baseDir string, defaultTimeout time.Duration) (map[string]ExecPreset, error) {
	merged := make(map[string]ExecPreset, len(base)+len(overrides))
	for name, preset := range base {
		merged[name] = preset
	}
	for name, override := range overrides {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("exec preset name cannot be empty")
		}
		if override.Enabled != nil && !*override.Enabled {
			delete(merged, name)
			continue
		}
		preset, ok := merged[name]
		if !ok {
			preset = ExecPreset{Timeout: defaultTimeout}
		}
		if override.Command != "" {
			preset.Command = resolveMaybeRelative(baseDir, override.Command)
		}
		if override.FixedArgs != nil {
			preset.FixedArgs = cloneStrings(override.FixedArgs)
		}
		if override.AllowedArgs != nil {
			preset.AllowedArgs = cloneStrings(override.AllowedArgs)
		}
		if override.TimeoutSec != nil {
			preset.Timeout = time.Duration(*override.TimeoutSec) * time.Second
		}
		if override.ReadOnly != nil {
			preset.ReadOnly = *override.ReadOnly
		}
		merged[name] = preset
	}
	return merged, nil
}

func validateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.BearerToken) == "" {
		return errors.New("MCP_BEARER_TOKEN required (or bearer_token in YAML)")
	}
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		return errors.New("listen_addr required")
	}
	if cfg.OutputMaxBytes <= 0 {
		return errors.New("output_max_bytes must be positive")
	}
	if cfg.StreamQueueSize <= 0 {
		return errors.New("stream_queue_size must be positive")
	}
	if cfg.MaxRequestBytes <= 0 {
		return errors.New("max_request_bytes must be positive")
	}
	if cfg.CommandTimeout <= 0 {
		return errors.New("command_timeout must be positive")
	}
	if cfg.ReadHeaderTimeout <= 0 {
		return errors.New("read_header_timeout must be positive")
	}
	if cfg.ReadTimeout <= 0 {
		return errors.New("read_timeout must be positive")
	}
	if cfg.WriteTimeout <= 0 {
		return errors.New("write_timeout must be positive")
	}
	if cfg.IdleTimeout <= 0 {
		return errors.New("idle_timeout must be positive")
	}
	if cfg.SessionTTL <= 0 {
		return errors.New("session_ttl must be positive")
	}
	if len(cfg.AllowedRoots) == 0 {
		return errors.New("at least one allowed root required")
	}
	cfg.AllowedRoots = mergeUniquePaths(nil, cfg.AllowedRoots)
	for _, protocol := range cfg.SupportedProtocols {
		if strings.TrimSpace(protocol) == "" {
			return errors.New("supported_protocols cannot contain empty values")
		}
	}
	for name := range cfg.GitAllowed {
		if !knownGitSubcommands[name] {
			return fmt.Errorf("git subcommand %q not supported", name)
		}
	}
	if cfg.GitAllowed["push"] {
		return errors.New("git push is not supported")
	}
	for name, preset := range cfg.ExecPresets {
		if strings.TrimSpace(name) == "" {
			return errors.New("exec preset name cannot be empty")
		}
		if strings.TrimSpace(preset.Command) == "" {
			return fmt.Errorf("exec preset %q command required", name)
		}
		if preset.Timeout <= 0 {
			return fmt.Errorf("exec preset %q timeout must be positive", name)
		}
		for _, arg := range preset.AllowedArgs {
			if strings.TrimSpace(arg) == "-vettool" {
				return fmt.Errorf("exec preset %q cannot allow -vettool", name)
			}
		}
	}
	return nil
}

func resolveConfigPathAgainst(cwd string, configPath string) (string, error) {
	resolvedConfigPath := configPath
	if !filepath.IsAbs(resolvedConfigPath) {
		resolvedConfigPath = filepath.Join(cwd, resolvedConfigPath)
	}
	resolvedConfigPath, err := filepath.Abs(resolvedConfigPath)
	if err != nil {
		return "", fmt.Errorf("abs config path: %w", err)
	}
	return filepath.Clean(resolvedConfigPath), nil
}

func defaultAllowedRoots(cwd string) []string {
	roots := []string{cwd}
	if configDir, ok := defaultConfigDir(); ok {
		roots = append(roots, configDir)
	}
	return mergeUniquePaths(nil, roots)
}

func defaultAuditLogPath(cwd string) string {
	if configDir, ok := defaultConfigDir(); ok {
		return filepath.Join(configDir, "mcp-audit.jsonl")
	}
	return filepath.Join(cwd, "mcp-audit.jsonl")
}

func defaultConfigPath() (string, bool) {
	configDir, ok := defaultConfigDir()
	if !ok {
		return "", false
	}
	return filepath.Join(configDir, "config.yaml"), true
}

func defaultConfigDir() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", false
	}
	absHome, err := filepath.Abs(home)
	if err != nil {
		return "", false
	}
	return filepath.Clean(filepath.Join(absHome, ".mcp-tools")), true
}

func resolvePathList(values []string, baseDir string) ([]string, error) {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		resolved := resolveMaybeRelative(baseDir, value)
		abs, err := filepath.Abs(resolved)
		if err != nil {
			return nil, fmt.Errorf("abs path %q: %w", value, err)
		}
		out = append(out, filepath.Clean(abs))
	}
	return out, nil
}

func resolveMaybeRelative(baseDir string, value string) string {
	if value == "" || filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Clean(filepath.Join(baseDir, value))
}

func mergeUniquePaths(base []string, extra []string) []string {
	seen := map[string]bool{}
	merged := make([]string, 0, len(base)+len(extra))
	for _, value := range append(cloneStrings(base), extra...) {
		if strings.TrimSpace(value) == "" {
			continue
		}
		clean := filepath.Clean(value)
		if !seen[clean] {
			seen[clean] = true
			merged = append(merged, clean)
		}
	}
	sort.Strings(merged)
	return merged
}

func dedupeStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	return dedupeStrings(parts)
}

func parsePositiveInt(raw, name string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return value, nil
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
