package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/v2up-32mb/mcp-tools/internal/audit"
	"github.com/v2up-32mb/mcp-tools/internal/config"
	"github.com/v2up-32mb/mcp-tools/internal/mcp"
	"github.com/v2up-32mb/mcp-tools/internal/pullfile"
	execx "github.com/v2up-32mb/mcp-tools/internal/tools/execx"
	fstools "github.com/v2up-32mb/mcp-tools/internal/tools/fs"
	gittools "github.com/v2up-32mb/mcp-tools/internal/tools/git"
	golangx "github.com/v2up-32mb/mcp-tools/internal/tools/golangx"
)

// newPullTestServer builds a server where fs tools and the /file endpoint
// share the same pullfile.Manager (as production main.go does).
func newPullTestServer(t *testing.T, mutate func(*config.Config)) (http.Handler, string, string) {
	t.Helper()
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	cfg := config.Config{
		ListenAddr:            "127.0.0.1:0",
		BearerToken:           "secret",
		LogLevel:              "INFO",
		AllowedRoots:          []string{dir},
		AuditLogPath:          auditPath,
		AuditRotateMaxMB:      10,
		AuditRotateMaxBackups: 5,
		CommandTimeout:        5 * time.Second,
		OutputMaxBytes:        4096,
		StreamQueueSize:       128,
		MaxRequestBytes:       1 << 20,
		ReadHeaderTimeout:     5 * time.Second,
		ReadTimeout:           15 * time.Second,
		WriteTimeout:          30 * time.Second,
		IdleTimeout:           60 * time.Second,
		GitAllowed:            map[string]bool{"status": true, "diff": true, "log": true, "add": true, "restore": true, "commit": true, "branch": true, "switch": true, "pull": true},
		ExecPresets:           map[string]config.ExecPreset{"go_test": {Command: "go", FixedArgs: []string{"test"}, AllowedArgs: []string{"-v"}, Timeout: 5 * time.Second, ReadOnly: true}},
		StartupDirectory:      dir,
		SessionTTL:            time.Hour,
		ServerName:            "mcp-tools-test",
		ServerVersion:         "test",
		SupportedProtocols:    []string{config.ProtocolLatest, config.ProtocolCompat, config.ProtocolLegacy},
		PullFile: config.PullFileConfig{
			Enabled:    true,
			MaxBytes:   1 << 20,
			TTLSeconds: 300,
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	logger, err := audit.NewJSONLWriter(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	mgr := pullfile.NewManager(cfg, logger)
	registry := mcp.NewRegistry(logger)
	for _, tool := range fstools.NewTools(cfg, mgr) {
		registry.Register(tool)
	}
	for _, tool := range gittools.NewTools(cfg) {
		registry.Register(tool)
	}
	for _, tool := range execx.NewTools(cfg) {
		registry.Register(tool)
	}
	for _, tool := range golangx.NewTools(cfg) {
		registry.Register(tool)
	}
	return NewServer(cfg, registry, mgr), dir, auditPath
}

func pullFileURL(t *testing.T, handler http.Handler, path string) string {
	t.Helper()
	sessionID := initializeSession(t, handler)
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      99,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "fs_pull_file",
			"arguments": map[string]any{"path": path},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/call returned %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Result struct {
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode tools/call response: %v", err)
	}
	url, _ := resp.Result.StructuredContent["url"].(string)
	if url == "" {
		t.Fatalf("no url in response: %s", rec.Body.String())
	}
	return url
}

func TestPullFileDownloadRoundtrip(t *testing.T) {
	handler, dir, auditPath := newPullTestServer(t, nil)
	raw := bytes.Repeat([]byte{0x89, 0x50, 0x4e, 0x47}, 16)
	if err := os.WriteFile(filepath.Join(dir, "progress.png"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	url := pullFileURL(t, handler, "progress.png")

	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("download returned %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Fatalf("content-type=%q", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), raw) {
		t.Fatalf("downloaded bytes mismatch: %d vs %d", len(rec.Body.Bytes()), len(raw))
	}

	// Audit must contain one fs_pull_file.download event.
	payload, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte(`"fs_pull_file.download"`)) {
		t.Fatalf("audit missing download event: %s", payload)
	}
}

func TestDownloadRejectsInvalidToken(t *testing.T) {
	handler, _, _ := newPullTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/file/not-a-valid-token", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
}

func TestDownloadRejectsMethodNotAllowed(t *testing.T) {
	handler, _, _ := newPullTestServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/file/whatever", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", rec.Code)
	}
}

func TestDownloadConsumesTokenOnce(t *testing.T) {
	handler, dir, _ := newPullTestServer(t, func(cfg *config.Config) {
		cfg.PullFile.MaxDownloads = 1
	})
	raw := []byte("once")
	if err := os.WriteFile(filepath.Join(dir, "a.png"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	url := pullFileURL(t, handler, "a.png")
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if i == 0 && rec.Code != http.StatusOK {
			t.Fatalf("first download got %d: %s", rec.Code, rec.Body.String())
		}
		if i == 1 && rec.Code != http.StatusForbidden {
			t.Fatalf("second download got %d, want 403", rec.Code)
		}
	}
}

func TestDownloadDisabled(t *testing.T) {
	handler, _, _ := newPullTestServer(t, func(cfg *config.Config) {
		cfg.PullFile.Enabled = false
	})
	req := httptest.NewRequest(http.MethodGet, "/file/sometoken", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 when disabled", rec.Code)
	}
}

func TestDownloadMissingFileNotFound(t *testing.T) {
	handler, dir, _ := newPullTestServer(t, func(cfg *config.Config) {
		cfg.PullFile.MaxDownloads = 1
	})
	path := filepath.Join(dir, "a.png")
	if err := os.WriteFile(path, []byte("gone"), 0o644); err != nil {
		t.Fatal(err)
	}
	url := pullFileURL(t, handler, "a.png")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestPullFileRejectsPathOutsideAllowedRoots(t *testing.T) {
	handler, _, _ := newPullTestServer(t, nil)
	outside := t.TempDir()
	path := filepath.Join(outside, "secret.png")
	if err := os.WriteFile(path, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionID := initializeSession(t, handler)
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      100,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "fs_pull_file",
			"arguments": map[string]any{"path": path},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/call returned %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("path rejected")) && !bytes.Contains(rec.Body.Bytes(), []byte("outside allowed roots")) {
		t.Fatalf("expected path rejection, got %s", rec.Body.String())
	}
}
