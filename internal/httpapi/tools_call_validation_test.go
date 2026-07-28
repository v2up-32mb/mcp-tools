package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example/mcp-tools/internal/audit"
	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
	"github.com/example/mcp-tools/internal/session"
)

type markerTool struct {
	marker string
}

func (t markerTool) Name() string        { return "test.marker" }
func (t markerTool) Description() string { return "writes a marker file" }
func (t markerTool) Schema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false}
}
func (t markerTool) ReadOnly() bool { return false }
func (t markerTool) Call(_ context.Context, _ mcp.CallContext, _ map[string]any) (mcp.Result, error) {
	if err := os.WriteFile(t.marker, []byte("ran\n"), 0o644); err != nil {
		return mcp.Result{}, err
	}
	return mcp.TextResult("marker written", map[string]any{"summary": "marker written"}, mcp.AuditData{Allowed: true, ResultDigest: "marker written"}), nil
}

func TestToolsCallRejectsNonObjectArgumentsBeforeExecutingTool(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	marker := filepath.Join(dir, "marker.txt")
	cfg := config.Config{
		BearerToken:        "secret",
		AllowedRoots:       []string{dir},
		AuditLogPath:       auditPath,
		OutputMaxBytes:     4096,
		StreamQueueSize:    128,
		MaxRequestBytes:    1 << 20,
		CommandTimeout:     5 * time.Second,
		StartupDirectory:   dir,
		SessionTTL:         time.Hour,
		ServerName:         "mcp-tools-test",
		ServerVersion:      "test",
		SupportedProtocols: []string{config.ProtocolLatest, config.ProtocolCompat, config.ProtocolLegacy},
	}
	logger, err := audit.NewJSONLWriter(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	registry := mcp.NewRegistry(logger)
	registry.Register(markerTool{marker: marker})
	srv := &Server{
		cfg:      cfg,
		registry: registry,
		sessions: session.NewManager(cfg.SessionTTL),
		streams:  newStreamHub(cfg.StreamQueueSize),
		resSubs:  newResourceSubscriptions(),
		metrics:  newServerMetrics(),
	}
	sess, err := srv.sessions.Create(config.ProtocolLatest, map[string]any{"name": "tester"})
	if err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "test.marker",
			"arguments": []any{"not", "an", "object"},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sess.ID)
	rec := httptest.NewRecorder()
	srv.handleMCP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-object arguments, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("tool should not execute on invalid arguments, stat err=%v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	errObj := decoded["error"].(map[string]any)
	if errObj["code"].(float64) != -32602 {
		t.Fatalf("expected invalid params error, got %#v", decoded)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("arguments must be an object")) {
		t.Fatalf("expected arguments type error, got %s", rec.Body.String())
	}
}

func TestToolsCallRejectsNonStringName(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	marker := filepath.Join(dir, "marker.txt")
	cfg := config.Config{
		BearerToken:        "secret",
		AllowedRoots:       []string{dir},
		AuditLogPath:       auditPath,
		OutputMaxBytes:     4096,
		StreamQueueSize:    128,
		MaxRequestBytes:    1 << 20,
		CommandTimeout:     5 * time.Second,
		StartupDirectory:   dir,
		SessionTTL:         time.Hour,
		ServerName:         "mcp-tools-test",
		ServerVersion:      "test",
		SupportedProtocols: []string{config.ProtocolLatest, config.ProtocolCompat, config.ProtocolLegacy},
	}
	logger, err := audit.NewJSONLWriter(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	registry := mcp.NewRegistry(logger)
	registry.Register(markerTool{marker: marker})
	srv := &Server{
		cfg:      cfg,
		registry: registry,
		sessions: session.NewManager(cfg.SessionTTL),
		streams:  newStreamHub(cfg.StreamQueueSize),
		resSubs:  newResourceSubscriptions(),
		metrics:  newServerMetrics(),
	}
	sess, err := srv.sessions.Create(config.ProtocolLatest, map[string]any{"name": "tester"})
	if err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      []any{"test.marker"},
			"arguments": map[string]any{},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sess.ID)
	rec := httptest.NewRecorder()
	srv.handleMCP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-string name, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("tool should not execute on invalid name, stat err=%v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	errObj := decoded["error"].(map[string]any)
	if errObj["code"].(float64) != -32602 {
		t.Fatalf("expected invalid params error, got %#v", decoded)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("tool name must be a string")) {
		t.Fatalf("expected name type error, got %s", rec.Body.String())
	}
}
