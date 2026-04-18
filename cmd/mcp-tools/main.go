package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/example/mcp-tools/internal/audit"
	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/httpapi"
	"github.com/example/mcp-tools/internal/mcp"
	execx "github.com/example/mcp-tools/internal/tools/execx"
	fstools "github.com/example/mcp-tools/internal/tools/fs"
	gittools "github.com/example/mcp-tools/internal/tools/git"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func run() error {
	configPath := flag.String("config", "", "path to YAML config file")
	validateConfigOnly := flag.Bool("validate-config", false, "load config, print effective config summary, and exit")
	flag.Parse()

	cfg, err := config.LoadWithOptions(config.LoadOptions{ConfigPath: *configPath})
	if err != nil {
		return err
	}
	if *validateConfigOnly {
		return printConfigSummary(cfg)
	}

	auditor, err := audit.NewJSONLWriter(cfg.AuditLogPath)
	if err != nil {
		return err
	}
	defer auditor.Close()

	registry := mcp.NewRegistry(auditor)
	for _, t := range fstools.NewTools(cfg) {
		registry.Register(t)
	}
	for _, t := range gittools.NewTools(cfg) {
		registry.Register(t)
	}
	for _, t := range execx.NewTools(cfg) {
		registry.Register(t)
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           httpapi.NewServer(cfg, registry),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	go func() {
		<-shutdownSignal()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Printf("listening on %s", cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func printConfigSummary(cfg config.Config) error {
	summary := map[string]any{
		"listen_addr":          cfg.ListenAddr,
		"debug_http_log":       cfg.DebugHTTPLog,
		"allowed_roots":        cfg.AllowedRoots,
		"allowed_origins":      cfg.AllowedOrigins,
		"audit_log_path":       cfg.AuditLogPath,
		"command_timeout_sec":  int(cfg.CommandTimeout / time.Second),
		"output_max_bytes":     cfg.OutputMaxBytes,
		"stream_queue_size":    cfg.StreamQueueSize,
		"max_request_bytes":    cfg.MaxRequestBytes,
		"read_header_timeout":  int(cfg.ReadHeaderTimeout / time.Second),
		"read_timeout":         int(cfg.ReadTimeout / time.Second),
		"write_timeout":        int(cfg.WriteTimeout / time.Second),
		"idle_timeout":         int(cfg.IdleTimeout / time.Second),
		"session_ttl_min":      int(cfg.SessionTTL / time.Minute),
		"server_name":          cfg.ServerName,
		"server_version":       cfg.ServerVersion,
		"supported_protocols":  cfg.SupportedProtocols,
		"git_allowed":          sortedTrueKeys(cfg.GitAllowed),
		"exec_presets":         sortedPresetNames(cfg.ExecPresets),
		"command_templates":    sortedTemplateNames(cfg.CommandTemplates),
		"bearer_token_present": cfg.BearerToken != "",
	}
	encoded, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config summary: %w", err)
	}
	fmt.Println(string(encoded))
	return nil
}

func sortedTrueKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for key, ok := range values {
		if ok {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

func sortedPresetNames(values map[string]config.ExecPreset) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func shutdownSignal() <-chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	return ch
}

func sortedTemplateNames(values map[string]config.CommandTemplate) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
