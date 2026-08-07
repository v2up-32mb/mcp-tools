package config

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/example/mcp-tools/internal/util"
	"io"
	"net/url"
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
	ListenAddr            string
	BearerToken           string
	LogLevel              string
	UnsafeAllowAll        bool
	AllowedRoots          []string
	AllowedOrigins        []string
	AuditLogPath          string
	AuditRotateMaxMB      int
	AuditRotateMaxBackups int
	CommandTimeout        time.Duration
	OutputMaxBytes        int
	StreamQueueSize       int
	MaxRequestBytes       int64
	ReadHeaderTimeout     time.Duration
	ReadTimeout           time.Duration
	WriteTimeout          time.Duration
	IdleTimeout           time.Duration
	GitAllowed            map[string]bool
	ExecPresets           map[string]ExecPreset
	CommandTemplates      map[string]CommandTemplate
	StartupDirectory      string
	SessionTTL            time.Duration
	ServerName            string
	ServerVersion         string
	SupportedProtocols    []string
	PullFile              PullFileConfig
}

type PullFileConfig struct {
	Enabled           bool
	AllowedExtensions []string
	MaxBytes          int64
	TTLSeconds        int
	MaxDownloads      int
	PublicBaseURL     string
}

type ExecPreset struct {
	Command     string
	FixedArgs   []string
	AllowedArgs []string
	Env         map[string]string
	Timeout     time.Duration
	ReadOnly    bool
}

type CommandTemplate struct {
	Command              []string
	Env                  map[string]string
	AllowedWorkdirs      []string
	Category             string
	Destructive          bool
	RequiresConfirmation bool
	Timeout              time.Duration
	ReadOnly             bool
}

type LoadOptions struct {
	ConfigPath string
	WorkDir    string
}

type fileConfig struct {
	ListenAddr            string              `yaml:"listen_addr"`
	BearerToken           string              `yaml:"bearer_token"`
	LogLevel              string              `yaml:"log_level"`
	UnsafeAllowAll        *bool               `yaml:"unsafe_allow_all"`
	AllowedRoots          []string            `yaml:"allowed_roots"`
	AllowedOrigins        []string            `yaml:"allowed_origins"`
	AuditLogPath          string              `yaml:"audit_log_path"`
	AuditRotateMaxMB      *int                `yaml:"audit_rotate_max_mb"`
	AuditRotateMaxBackups *int                `yaml:"audit_rotate_max_backups"`
	CommandTimeoutSec     *int                `yaml:"command_timeout_sec"`
	OutputMaxBytes        *int                `yaml:"output_max_bytes"`
	StreamQueueSize       *int                `yaml:"stream_queue_size"`
	MaxRequestBytes       *int64              `yaml:"max_request_bytes"`
	ReadHeaderTimeoutSec  *int                `yaml:"read_header_timeout_sec"`
	ReadTimeoutSec        *int                `yaml:"read_timeout_sec"`
	WriteTimeoutSec       *int                `yaml:"write_timeout_sec"`
	IdleTimeoutSec        *int                `yaml:"idle_timeout_sec"`
	SessionTTLMin         *int                `yaml:"session_ttl_min"`
	ServerName            string              `yaml:"server_name"`
	ServerVersion         string              `yaml:"server_version"`
	SupportedProtocols    *[]string           `yaml:"supported_protocols"`
	Git                   fileGitConfig       `yaml:"git"`
	Exec                  fileExecConfig      `yaml:"exec"`
	PullFile              *filePullFileConfig `yaml:"pull_file"`
}

type filePullFileConfig struct {
	Enabled           *bool                 `yaml:"enabled"`
	AllowedExtensions []string              `yaml:"allowed_extensions"`
	MaxBytes          *int64                `yaml:"max_bytes"`
	URL               filePullFileURLConfig `yaml:"url"`
}

type filePullFileURLConfig struct {
	TTLSeconds    *int   `yaml:"ttl_sec"`
	MaxDownloads  *int   `yaml:"max_downloads"`
	PublicBaseURL string `yaml:"public_base_url"`
}

type fileGitConfig struct {
	AllowedSubcommands *[]string `yaml:"allowed_subcommands"`
}

type fileExecConfig struct {
	Presets          map[string]fileExecPreset      `yaml:"presets"`
	CommandTemplates map[string]fileCommandTemplate `yaml:"command_templates"`
}

type fileExecPreset struct {
	Enabled     *bool             `yaml:"enabled"`
	Command     *string           `yaml:"command"`
	FixedArgs   []string          `yaml:"fixed_args"`
	AllowedArgs []string          `yaml:"allowed_args"`
	Env         map[string]string `yaml:"env"`
	TimeoutSec  *int              `yaml:"timeout_sec"`
	ReadOnly    *bool             `yaml:"read_only"`
}

type fileCommandTemplate struct {
	Enabled              *bool             `yaml:"enabled"`
	Command              []string          `yaml:"command"`
	Env                  map[string]string `yaml:"env"`
	AllowedWorkdirs      []string          `yaml:"allowed_workdirs"`
	Category             string            `yaml:"category"`
	Destructive          *bool             `yaml:"destructive"`
	RequiresConfirmation *bool             `yaml:"requires_confirmation"`
	TimeoutSec           *int              `yaml:"timeout_sec"`
	ReadOnly             *bool             `yaml:"read_only"`
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
	configPath := strings.TrimSpace(util.FirstNonEmpty(opts.ConfigPath, os.Getenv("MCP_CONFIG_FILE")))
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
		ListenAddr:            "0.0.0.0:8080",
		LogLevel:              "INFO",
		UnsafeAllowAll:        true,
		AllowedRoots:          defaultAllowedRoots(cwd),
		AuditLogPath:          defaultAuditLogPath(cwd),
		AuditRotateMaxMB:      10,
		AuditRotateMaxBackups: 5,
		CommandTimeout:        timeout,
		OutputMaxBytes:        65536,
		StreamQueueSize:       128,
		MaxRequestBytes:       1 << 20,
		ReadHeaderTimeout:     5 * time.Second,
		ReadTimeout:           15 * time.Second,
		WriteTimeout:          30 * time.Second,
		IdleTimeout:           60 * time.Second,
		GitAllowed:            defaultGitAllowed(),
		ExecPresets:           defaultExecPresets(timeout),
		CommandTemplates:      defaultCommandTemplates(timeout),
		StartupDirectory:      cwd,
		SessionTTL:            120 * time.Minute,
		ServerName:            "mcp-tools",
		ServerVersion:         "1.0.0",
		SupportedProtocols:    []string{ProtocolLatest, ProtocolCompat, ProtocolLegacy, ProtocolOld},
		PullFile: PullFileConfig{
			Enabled:    true,
			MaxBytes:   10 * 1024 * 1024,
			TTLSeconds: 300,
		},
	}
}

func applyYAMLFile(cfg *Config, path string) error {
	payload, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config file %q: %w", path, err)
	}
	if err := validateYAMLDocumentShape(payload, path); err != nil {
		return err
	}
	var fc fileConfig
	decoder := yaml.NewDecoder(bytes.NewReader(payload))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fc); err != nil {
		if err == io.EOF {
			return nil
		}
		return fmt.Errorf("parse config file %q: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return fmt.Errorf("parse config file %q: %w", path, err)
		}
		return fmt.Errorf("parse config file %q: multiple YAML documents are not supported", path)
	}
	baseDir := filepath.Dir(path)
	if fc.ListenAddr != "" {
		cfg.ListenAddr = fc.ListenAddr
	}
	if fc.BearerToken != "" {
		cfg.BearerToken = fc.BearerToken
	}
	if strings.TrimSpace(fc.LogLevel) != "" {
		cfg.LogLevel = normalizeLogLevel(fc.LogLevel)
	}
	if fc.UnsafeAllowAll != nil {
		cfg.UnsafeAllowAll = *fc.UnsafeAllowAll
	}
	if len(fc.AllowedRoots) > 0 {
		roots, err := resolvePathList(fc.AllowedRoots, baseDir)
		if err != nil {
			return fmt.Errorf("allowed_roots: %w", err)
		}
		cfg.AllowedRoots = mergeUniquePaths(cfg.AllowedRoots, roots)
	}
	if len(fc.AllowedOrigins) > 0 {
		origins, err := dedupeRequiredStrings(fc.AllowedOrigins, "allowed_origins")
		if err != nil {
			return err
		}
		cfg.AllowedOrigins = origins
	}
	if fc.AuditLogPath != "" {
		cfg.AuditLogPath = resolveMaybeRelative(baseDir, fc.AuditLogPath)
	}
	if fc.AuditRotateMaxMB != nil {
		cfg.AuditRotateMaxMB = *fc.AuditRotateMaxMB
	}
	if fc.AuditRotateMaxBackups != nil {
		cfg.AuditRotateMaxBackups = *fc.AuditRotateMaxBackups
	}
	if fc.CommandTimeoutSec != nil {
		timeout, err := durationFromUnits(*fc.CommandTimeoutSec, time.Second, "command_timeout_sec")
		if err != nil {
			return err
		}
		setCommandTimeout(cfg, timeout)
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
		timeout, err := durationFromUnits(*fc.ReadHeaderTimeoutSec, time.Second, "read_header_timeout_sec")
		if err != nil {
			return err
		}
		cfg.ReadHeaderTimeout = timeout
	}
	if fc.ReadTimeoutSec != nil {
		timeout, err := durationFromUnits(*fc.ReadTimeoutSec, time.Second, "read_timeout_sec")
		if err != nil {
			return err
		}
		cfg.ReadTimeout = timeout
	}
	if fc.WriteTimeoutSec != nil {
		timeout, err := durationFromUnits(*fc.WriteTimeoutSec, time.Second, "write_timeout_sec")
		if err != nil {
			return err
		}
		cfg.WriteTimeout = timeout
	}
	if fc.IdleTimeoutSec != nil {
		timeout, err := durationFromUnits(*fc.IdleTimeoutSec, time.Second, "idle_timeout_sec")
		if err != nil {
			return err
		}
		cfg.IdleTimeout = timeout
	}
	if fc.SessionTTLMin != nil {
		ttl, err := durationFromUnits(*fc.SessionTTLMin, time.Minute, "session_ttl_min")
		if err != nil {
			return err
		}
		cfg.SessionTTL = ttl
	}
	if fc.ServerName != "" {
		cfg.ServerName = fc.ServerName
	}
	if fc.ServerVersion != "" {
		cfg.ServerVersion = fc.ServerVersion
	}
	if fc.SupportedProtocols != nil {
		cfg.SupportedProtocols = util.CloneStrings(*fc.SupportedProtocols)
	}
	if fc.Git.AllowedSubcommands != nil {
		allowed, err := resolveGitAllowedSubcommands(*fc.Git.AllowedSubcommands)
		if err != nil {
			return fmt.Errorf("git.allowed_subcommands: %w", err)
		}
		cfg.GitAllowed = allowed
	}
	if len(fc.Exec.Presets) > 0 {
		merged, err := mergeExecPresets(cfg.ExecPresets, fc.Exec.Presets, baseDir, cfg.CommandTimeout)
		if err != nil {
			return err
		}
		cfg.ExecPresets = merged
	}
	if len(fc.Exec.CommandTemplates) > 0 {
		merged, err := mergeCommandTemplates(cfg.CommandTemplates, fc.Exec.CommandTemplates, baseDir, cfg.CommandTimeout)
		if err != nil {
			return err
		}
		cfg.CommandTemplates = merged
	}
	if fc.PullFile != nil {
		if fc.PullFile.Enabled != nil {
			cfg.PullFile.Enabled = *fc.PullFile.Enabled
		}
		if len(fc.PullFile.AllowedExtensions) > 0 {
			extensions, err := normalizePullFileExtensions(fc.PullFile.AllowedExtensions)
			if err != nil {
				return fmt.Errorf("pull_file.allowed_extensions: %w", err)
			}
			cfg.PullFile.AllowedExtensions = extensions
		}
		if fc.PullFile.MaxBytes != nil {
			cfg.PullFile.MaxBytes = *fc.PullFile.MaxBytes
		}
		if fc.PullFile.URL.TTLSeconds != nil {
			cfg.PullFile.TTLSeconds = *fc.PullFile.URL.TTLSeconds
		}
		if fc.PullFile.URL.MaxDownloads != nil {
			cfg.PullFile.MaxDownloads = *fc.PullFile.URL.MaxDownloads
		}
		if strings.TrimSpace(fc.PullFile.URL.PublicBaseURL) != "" {
			cfg.PullFile.PublicBaseURL = strings.TrimRight(strings.TrimSpace(fc.PullFile.URL.PublicBaseURL), "/")
		}
	}
	return nil
}

func validateYAMLDocumentShape(payload []byte, path string) error {
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&doc); err != nil {
		if err == io.EOF {
			return nil
		}
		return fmt.Errorf("parse config file %q: %w", path, err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return fmt.Errorf("parse config file %q: %w", path, err)
		}
		return fmt.Errorf("parse config file %q: multiple YAML documents are not supported", path)
	}
	if isExplicitNullDocument(doc) {
		return fmt.Errorf("parse config file %q: YAML document must be a mapping, not null", path)
	}
	if nullPath, ok := findNullYAMLValuePath(&doc, ""); ok {
		return fmt.Errorf("parse config file %q: YAML field %s must not be null", path, nullPath)
	}
	return nil
}

func isExplicitNullDocument(doc yaml.Node) bool {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return false
	}
	child := doc.Content[0]
	return child.Kind == yaml.ScalarNode && child.Tag == "!!null" && strings.TrimSpace(child.Value) != ""
}

func findNullYAMLValuePath(node *yaml.Node, nodePath string) (string, bool) {
	if node == nil {
		return "", false
	}
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) == 0 {
			return "", false
		}
		return findNullYAMLValuePath(node.Content[0], nodePath)
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			childPath := appendYAMLFieldPath(nodePath, node.Content[i], i/2)
			if foundPath, ok := findNullYAMLValuePath(node.Content[i+1], childPath); ok {
				return foundPath, true
			}
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			childPath := fmt.Sprintf("[%d]", i)
			if nodePath != "" {
				childPath = fmt.Sprintf("%s[%d]", nodePath, i)
			}
			if foundPath, ok := findNullYAMLValuePath(child, childPath); ok {
				return foundPath, true
			}
		}
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			return nodePath, true
		}
	}
	return "", false
}

func appendYAMLFieldPath(parent string, key *yaml.Node, index int) string {
	name := fmt.Sprintf("<field:%d>", index)
	if key != nil && strings.TrimSpace(key.Value) != "" {
		name = key.Value
	}
	if parent == "" {
		return name
	}
	return parent + "." + name
}

func applyEnv(cfg *Config, cwd string) error {
	if value := os.Getenv("MCP_BEARER_TOKEN"); value != "" {
		cfg.BearerToken = value
	}
	if value := os.Getenv("MCP_LISTEN_ADDR"); value != "" {
		cfg.ListenAddr = value
	}
	if value := os.Getenv("MCP_LOG_LEVEL"); value != "" {
		cfg.LogLevel = normalizeLogLevel(value)
	}
	if value := os.Getenv("MCP_UNSAFE_ALLOW_ALL"); value != "" {
		parsed, err := parseBool(value, "MCP_UNSAFE_ALLOW_ALL")
		if err != nil {
			return err
		}
		cfg.UnsafeAllowAll = parsed
	}
	if value := os.Getenv("MCP_ALLOWED_ROOTS"); value != "" {
		roots, err := resolvePathList(splitCSVStrict(value), cwd)
		if err != nil {
			return fmt.Errorf("MCP_ALLOWED_ROOTS: %w", err)
		}
		cfg.AllowedRoots = mergeUniquePaths(defaultAllowedRoots(cfg.StartupDirectory), roots)
	}
	if value := os.Getenv("MCP_ALLOWED_ORIGINS"); value != "" {
		origins, err := dedupeRequiredStrings(splitCSVStrict(value), "MCP_ALLOWED_ORIGINS")
		if err != nil {
			return err
		}
		cfg.AllowedOrigins = origins
	}
	if value := os.Getenv("MCP_AUDIT_LOG_PATH"); value != "" {
		cfg.AuditLogPath = resolveMaybeRelative(cwd, value)
	}
	if value := os.Getenv("MCP_AUDIT_ROTATE_MAX_MB"); value != "" {
		parsed, err := parseNonNegativeInt(value, "MCP_AUDIT_ROTATE_MAX_MB")
		if err != nil {
			return err
		}
		cfg.AuditRotateMaxMB = parsed
	}
	if value := os.Getenv("MCP_AUDIT_ROTATE_MAX_BACKUPS"); value != "" {
		parsed, err := parseNonNegativeInt(value, "MCP_AUDIT_ROTATE_MAX_BACKUPS")
		if err != nil {
			return err
		}
		cfg.AuditRotateMaxBackups = parsed
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
		timeout, err := durationFromUnits(parsed, time.Second, "MCP_COMMAND_TIMEOUT_SEC")
		if err != nil {
			return err
		}
		setCommandTimeout(cfg, timeout)
	}
	if value := os.Getenv("MCP_SESSION_TTL_MIN"); value != "" {
		parsed, err := parsePositiveInt(value, "MCP_SESSION_TTL_MIN")
		if err != nil {
			return err
		}
		ttl, err := durationFromUnits(parsed, time.Minute, "MCP_SESSION_TTL_MIN")
		if err != nil {
			return err
		}
		cfg.SessionTTL = ttl
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
		timeout, err := durationFromUnits(parsed, time.Second, "MCP_READ_HEADER_TIMEOUT_SEC")
		if err != nil {
			return err
		}
		cfg.ReadHeaderTimeout = timeout
	}
	if value := os.Getenv("MCP_READ_TIMEOUT_SEC"); value != "" {
		parsed, err := parsePositiveInt(value, "MCP_READ_TIMEOUT_SEC")
		if err != nil {
			return err
		}
		timeout, err := durationFromUnits(parsed, time.Second, "MCP_READ_TIMEOUT_SEC")
		if err != nil {
			return err
		}
		cfg.ReadTimeout = timeout
	}
	if value := os.Getenv("MCP_WRITE_TIMEOUT_SEC"); value != "" {
		parsed, err := parsePositiveInt(value, "MCP_WRITE_TIMEOUT_SEC")
		if err != nil {
			return err
		}
		timeout, err := durationFromUnits(parsed, time.Second, "MCP_WRITE_TIMEOUT_SEC")
		if err != nil {
			return err
		}
		cfg.WriteTimeout = timeout
	}
	if value := os.Getenv("MCP_IDLE_TIMEOUT_SEC"); value != "" {
		parsed, err := parsePositiveInt(value, "MCP_IDLE_TIMEOUT_SEC")
		if err != nil {
			return err
		}
		timeout, err := durationFromUnits(parsed, time.Second, "MCP_IDLE_TIMEOUT_SEC")
		if err != nil {
			return err
		}
		cfg.IdleTimeout = timeout
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
	goEnv := defaultManagedGoEnv()
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
			Env:         cloneMapStrings(goEnv),
			Timeout:     baseTimeout,
		},
		"go_test": {
			Command:     "go",
			FixedArgs:   []string{"test"},
			AllowedArgs: []string{"-run", "-count", "-timeout", "-v", "-race", "-cover", "-coverprofile"},
			Env:         cloneMapStrings(goEnv),
			Timeout:     baseTimeout,
			ReadOnly:    true,
		},
		"go_generate": {
			Command:     "go",
			FixedArgs:   []string{"generate"},
			AllowedArgs: []string{"-run", "-skip", "-v", "-x", "-n", "-tags"},
			Env:         cloneMapStrings(goEnv),
			Timeout:     baseTimeout,
		},
		"go_build": {
			Command:     "go",
			FixedArgs:   []string{"build"},
			AllowedArgs: []string{"-v", "-race", "-o", "-tags"},
			Env:         cloneMapStrings(goEnv),
			Timeout:     baseTimeout,
		},
		"go_vet": {
			Command:     "go",
			FixedArgs:   []string{"vet"},
			AllowedArgs: []string{"-tags"},
			Env:         cloneMapStrings(goEnv),
			Timeout:     baseTimeout,
			ReadOnly:    true,
		},
		"go_mod_tidy": {
			Command:     "go",
			FixedArgs:   []string{"mod", "tidy"},
			AllowedArgs: []string{"-v", "-e", "-diff", "-go", "-compat", "-x"},
			Env:         cloneMapStrings(goEnv),
			Timeout:     baseTimeout,
		},
		"go_get": {
			Command:     "go",
			FixedArgs:   []string{"get"},
			AllowedArgs: []string{"-t", "-u", "-x", "-tags"},
			Env:         cloneMapStrings(goEnv),
			Timeout:     baseTimeout,
		},
		"go_list": {
			Command:     "go",
			FixedArgs:   []string{"list"},
			AllowedArgs: []string{"-deps", "-json", "-test", "-tags"},
			Env:         cloneMapStrings(goEnv),
			Timeout:     baseTimeout,
			ReadOnly:    true,
		},
		"go_work_sync": {
			Command:   "go",
			FixedArgs: []string{"work", "sync"},
			Env:       cloneMapStrings(goEnv),
			Timeout:   baseTimeout,
		},
	}
}

func defaultManagedGoEnv() map[string]string {
	configDir, ok := defaultConfigDir()
	cacheRoot := filepath.Join(".", ".mcp-tools", "cache")
	if ok {
		cacheRoot = filepath.Join(configDir, "cache")
	}
	return map[string]string{
		"GOCACHE":    filepath.Join(cacheRoot, "go-build"),
		"GOMODCACHE": filepath.Join(cacheRoot, "gomod"),
		"GOTMPDIR":   filepath.Join(cacheRoot, "tmp"),
	}
}

func defaultCommandTemplates(baseTimeout time.Duration) map[string]CommandTemplate {
	return map[string]CommandTemplate{
		"make_test": {
			Command:              []string{"make", "test"},
			Category:             "test",
			Destructive:          false,
			RequiresConfirmation: false,
			Timeout:              baseTimeout,
			ReadOnly:             false,
		},
		"make_build": {
			Command:              []string{"make", "build"},
			Category:             "build",
			Destructive:          false,
			RequiresConfirmation: false,
			Timeout:              baseTimeout,
			ReadOnly:             false,
		},
		"go_clean_testcache": {
			Command:              []string{"go", "clean", "-testcache"},
			Category:             "cleanup",
			Destructive:          true,
			RequiresConfirmation: true,
			Timeout:              baseTimeout,
			ReadOnly:             false,
		},
	}
}

func setCommandTimeout(cfg *Config, timeout time.Duration) {
	previous := cfg.CommandTimeout
	cfg.CommandTimeout = timeout
	if previous <= 0 || previous == timeout {
		return
	}
	for name, preset := range cfg.ExecPresets {
		if preset.Timeout == previous {
			preset.Timeout = timeout
			cfg.ExecPresets[name] = preset
		}
	}
	for name, template := range cfg.CommandTemplates {
		if template.Timeout == previous {
			template.Timeout = timeout
			cfg.CommandTemplates[name] = template
		}
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
		if override.Command != nil {
			preset.Command = resolveCommandName(baseDir, *override.Command)
		}
		if override.FixedArgs != nil {
			preset.FixedArgs = util.CloneStrings(override.FixedArgs)
		}
		if override.AllowedArgs != nil {
			preset.AllowedArgs = util.CloneStrings(override.AllowedArgs)
		}
		if override.Env != nil {
			preset.Env = cloneMapStrings(override.Env)
		}
		if override.TimeoutSec != nil {
			timeout, err := durationFromUnits(*override.TimeoutSec, time.Second, fmt.Sprintf("exec preset %q timeout_sec", name))
			if err != nil {
				return nil, err
			}
			preset.Timeout = timeout
		}
		if override.ReadOnly != nil {
			preset.ReadOnly = *override.ReadOnly
		}
		merged[name] = preset
	}
	return merged, nil
}

func mergeCommandTemplates(base map[string]CommandTemplate, overrides map[string]fileCommandTemplate, baseDir string, defaultTimeout time.Duration) (map[string]CommandTemplate, error) {
	merged := make(map[string]CommandTemplate, len(base)+len(overrides))
	for name, template := range base {
		merged[name] = template
	}
	for name, override := range overrides {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("command template name cannot be empty")
		}
		if override.Enabled != nil && !*override.Enabled {
			delete(merged, name)
			continue
		}
		template, ok := merged[name]
		if !ok {
			template = CommandTemplate{Timeout: defaultTimeout}
		}
		if override.Command != nil {
			template.Command = resolveCommandArgv(baseDir, override.Command)
		}
		if override.Env != nil {
			template.Env = cloneMapStrings(override.Env)
		}
		if override.AllowedWorkdirs != nil {
			allowedWorkdirs, err := resolveRequiredPathList(override.AllowedWorkdirs, baseDir)
			if err != nil {
				return nil, fmt.Errorf("command template %q allowed_workdirs: %w", name, err)
			}
			template.AllowedWorkdirs = allowedWorkdirs
		}
		if strings.TrimSpace(override.Category) != "" {
			template.Category = strings.TrimSpace(override.Category)
		}
		if override.Destructive != nil {
			template.Destructive = *override.Destructive
		}
		if override.RequiresConfirmation != nil {
			template.RequiresConfirmation = *override.RequiresConfirmation
		}
		if override.TimeoutSec != nil {
			timeout, err := durationFromUnits(*override.TimeoutSec, time.Second, fmt.Sprintf("command template %q timeout_sec", name))
			if err != nil {
				return nil, err
			}
			template.Timeout = timeout
		}
		if override.ReadOnly != nil {
			template.ReadOnly = *override.ReadOnly
		}
		merged[name] = template
	}
	return merged, nil
}

func validateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.BearerToken) == "" {
		return errors.New("MCP_BEARER_TOKEN required (or bearer_token in YAML)")
	}
	if !isValidLogLevel(cfg.LogLevel) {
		return fmt.Errorf("log_level %q not supported", cfg.LogLevel)
	}
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		return errors.New("listen_addr required")
	}
	if cfg.AuditRotateMaxMB < 0 {
		return errors.New("audit_rotate_max_mb must be non-negative")
	}
	if int64(cfg.AuditRotateMaxMB) > (int64(1<<63-1) / (1024 * 1024)) {
		return errors.New("audit_rotate_max_mb is too large")
	}
	if cfg.AuditRotateMaxMB > 0 && cfg.AuditRotateMaxBackups <= 0 {
		return errors.New("audit_rotate_max_backups must be positive when audit rotation is enabled")
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
	for _, origin := range cfg.AllowedOrigins {
		if strings.TrimSpace(origin) == "" {
			return errors.New("allowed_origins cannot contain empty entries")
		}
	}
	if len(cfg.SupportedProtocols) == 0 {
		return errors.New("at least one supported protocol required")
	}
	for _, protocol := range cfg.SupportedProtocols {
		if strings.TrimSpace(protocol) == "" {
			return errors.New("supported_protocols cannot contain empty values")
		}
	}
	// Git subcommand validation is done in resolveGitAllowedSubcommands
	// (fail-fast at parse time). Here we only enforce the push ban.
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
		for _, arg := range preset.FixedArgs {
			if strings.TrimSpace(arg) == "" {
				return fmt.Errorf("exec preset %q cannot contain empty fixed_args entries", name)
			}
		}
		for _, arg := range preset.AllowedArgs {
			if strings.TrimSpace(arg) == "" {
				return fmt.Errorf("exec preset %q cannot contain empty allowed_args entries", name)
			}
			if strings.TrimSpace(arg) == "-vettool" {
				return fmt.Errorf("exec preset %q cannot allow -vettool", name)
			}
		}
		for key := range preset.Env {
			if err := util.ValidateEnvKey(key); err != nil {
				return fmt.Errorf("exec preset %q env: %w", name, err)
			}
		}
	}
	for name, template := range cfg.CommandTemplates {
		if strings.TrimSpace(name) == "" {
			return errors.New("command template name cannot be empty")
		}
		if len(template.Command) == 0 {
			return fmt.Errorf("command template %q command required", name)
		}
		for _, part := range template.Command {
			if strings.TrimSpace(part) == "" {
				return fmt.Errorf("command template %q cannot contain empty command parts", name)
			}
		}
		for _, workdir := range template.AllowedWorkdirs {
			if strings.TrimSpace(workdir) == "" {
				return fmt.Errorf("command template %q cannot contain empty allowed_workdirs entries", name)
			}
		}
		if template.Timeout <= 0 {
			return fmt.Errorf("command template %q timeout must be positive", name)
		}
		for key := range template.Env {
			if err := util.ValidateEnvKey(key); err != nil {
				return fmt.Errorf("command template %q env: %w", name, err)
			}
		}
	}
	if cfg.PullFile.MaxBytes <= 0 {
		return errors.New("pull_file.max_bytes must be positive")
	}
	if cfg.PullFile.TTLSeconds <= 0 {
		return errors.New("pull_file.url.ttl_sec must be positive")
	}
	if cfg.PullFile.MaxDownloads < 0 {
		return errors.New("pull_file.url.max_downloads must be non-negative")
	}
	if err := validatePullFilePublicBaseURL(cfg.PullFile.PublicBaseURL); err != nil {
		return err
	}
	return nil
}

func normalizePullFileExtensions(values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return nil, errors.New("cannot contain empty entries")
		}
		if !strings.HasPrefix(trimmed, ".") {
			return nil, fmt.Errorf("extension %q must start with a dot", trimmed)
		}
		if strings.ContainsAny(trimmed, "/\\ \t") {
			return nil, fmt.Errorf("extension %q is invalid", trimmed)
		}
		out = append(out, strings.ToLower(trimmed))
	}
	return out, nil
}

func validatePullFilePublicBaseURL(value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("pull_file.url.public_base_url: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("pull_file.url.public_base_url must be an absolute http(s) URL")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("pull_file.url.public_base_url must not contain query or fragment")
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
			return nil, errors.New("cannot contain empty entries")
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

func resolveRequiredPathList(values []string, baseDir string) ([]string, error) {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return nil, errors.New("cannot contain empty entries")
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

func resolveCommandArgv(baseDir string, values []string) []string {
	out := util.CloneStrings(values)
	if len(out) > 0 {
		out[0] = resolveCommandName(baseDir, out[0])
	}
	return out
}

func resolveCommandName(baseDir string, value string) string {
	if value == "" {
		return ""
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	if isPathLikeCommandName(value) {
		return resolveMaybeRelative(baseDir, value)
	}
	return value
}

func isPathLikeCommandName(value string) bool {
	return strings.ContainsAny(value, `/\`)
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
	for _, value := range append(util.CloneStrings(base), extra...) {
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

func dedupeRequiredStrings(values []string, name string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return nil, fmt.Errorf("%s cannot contain empty entries", name)
		}
		if seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out, nil
}

func resolveGitAllowedSubcommands(values []string) (map[string]bool, error) {
	allowed := make(map[string]bool, len(values))
	for _, value := range values {
		name := strings.TrimSpace(value)
		if name == "" {
			return nil, errors.New("cannot contain empty entries")
		}
		if !knownGitSubcommands[name] {
			return nil, fmt.Errorf("git subcommand %q not supported", name)
		}
		allowed[name] = true
	}
	return allowed, nil
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	return dedupeStrings(parts)
}

func splitCSVStrict(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, len(parts))
	for i, part := range parts {
		out[i] = strings.TrimSpace(part)
	}
	return out
}

func durationFromUnits(value int, unit time.Duration, name string) (time.Duration, error) {
	if value <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	maxValue := int64((time.Duration(1<<63 - 1)) / unit)
	if int64(value) > maxValue {
		return 0, fmt.Errorf("%s is too large", name)
	}
	return time.Duration(value) * unit, nil
}

func parsePositiveInt(raw, name string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return value, nil
}

func parseNonNegativeInt(raw, name string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return value, nil
}

func parseBool(raw, name string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid %s", name)
	}
}

func normalizeLogLevel(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func isValidLogLevel(value string) bool {
	switch normalizeLogLevel(value) {
	case "DEBUG", "INFO", "WARN", "ERROR":
		return true
	default:
		return false
	}
}

func cloneMapStrings(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v
	}
	return out
}
