package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/mcp-tools/internal/applog"
	"github.com/example/mcp-tools/internal/audit"
	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
	"github.com/example/mcp-tools/internal/session"
	execx "github.com/example/mcp-tools/internal/tools/execx"
	fstools "github.com/example/mcp-tools/internal/tools/fs"
	gittools "github.com/example/mcp-tools/internal/tools/git"
	golangx "github.com/example/mcp-tools/internal/tools/golangx"
)

func newTestServer(t *testing.T) (http.Handler, string, string) {
	return newTestServerWithConfig(t, nil)
}

func newTestServerWithConfig(t *testing.T, mutate func(*config.Config)) (http.Handler, string, string) {
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
	}
	if mutate != nil {
		mutate(&cfg)
	}
	logger, err := audit.NewJSONLWriter(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	registry := mcp.NewRegistry(logger)
	for _, tool := range fstools.NewTools(cfg) {
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
	return NewServer(cfg, registry), dir, auditPath
}

func captureDebugLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := applog.Default()
	applog.SetDefault(applog.New(&buf, "DEBUG"))
	t.Cleanup(func() {
		applog.SetDefault(old)
	})
	return &buf
}
func TestUnauthorized(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestStatezRequiresAuth(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/debug/statez", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSSEAsyncQueueFullFallbackDoesNotExecuteToolTwice(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	cfg := config.Config{
		BearerToken:        "secret",
		AllowedRoots:       []string{dir},
		AuditLogPath:       auditPath,
		CommandTimeout:     5 * time.Second,
		OutputMaxBytes:     4096,
		StreamQueueSize:    1,
		MaxRequestBytes:    1 << 20,
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
	for _, tool := range fstools.NewTools(cfg) {
		registry.Register(tool)
	}
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
	stream := srv.streams.register(sess.ID)
	defer srv.streams.unregister(sess.ID, stream)
	if !srv.streams.publish(sess.ID, rpcResponse{JSONRPC: "2.0", ID: 1, Result: map[string]any{"queued": true}}) {
		t.Fatal("failed to pre-fill stream queue")
	}

	target := filepath.Join(dir, "line.txt")
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      77,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "fs.edit_lines",
			"arguments": map[string]any{
				"path":              target,
				"start_line":        1,
				"end_line":          1,
				"expected_old_text": "old\n",
				"new_text":          "new\n",
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sess.ID)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected single-shot fallback 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	resp := parseSSEData(t, rec.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected SSE RPC error: %#v", resp.Error)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected tool result map, got %#v", resp.Result)
	}
	if result["isError"] != false {
		t.Fatalf("expected fallback to return the first successful tool result without re-executing, got %#v", result)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new\n" {
		t.Fatalf("unexpected file content %q", got)
	}
}

func TestStatezShowsRuntimeCountsAndCounters(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	sessionID := initializeSessionHTTP(t, ts.URL)
	events, errs, cancel := startRawSSEStreamBuffered(t, ts.URL, sessionID, 32)
	defer cancel()

	target := filepath.Join(dir, "statez.txt")
	if err := os.WriteFile(target, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	subResp := asyncSSEPost(t, ts.URL, sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      901,
		"method":  "resources/subscribe",
		"params": map[string]any{
			"uri": "file://" + target,
		},
	})
	subResp.Body.Close()
	if subResp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 subscribe, got %d", subResp.StatusCode)
	}
	select {
	case evt := <-events:
		if id, ok := evt["id"].(float64); !ok || int(id) != 901 {
			t.Fatalf("unexpected subscribe ack: %#v", evt)
		}
	case err := <-errs:
		t.Fatalf("stream error after subscribe: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for subscribe ack")
	}

	pingResp := asyncSSEPost(t, ts.URL, sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      902,
		"method":  "ping",
		"params":  map[string]any{},
	})
	pingResp.Body.Close()
	if pingResp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 ping, got %d", pingResp.StatusCode)
	}

	writePayload := map[string]any{
		"jsonrpc": "2.0",
		"id":      903,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "fs.write_file",
			"arguments": map[string]any{
				"path": target,
				"text": "after\n",
			},
		},
	}
	body, _ := json.Marshal(writePayload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("writer request failed: %d body=%s", rec.Code, rec.Body.String())
	}

	gotPing := false
	gotNotification := false
	timeout := time.After(3 * time.Second)
	for !(gotPing && gotNotification) {
		select {
		case evt := <-events:
			if method, _ := evt["method"].(string); method == "notifications/resources/updated" {
				params := evt["params"].(map[string]any)
				if params["uri"] == "file://"+target {
					gotNotification = true
				}
				continue
			}
			if id, ok := evt["id"].(float64); ok && int(id) == 902 {
				gotPing = true
			}
		case err := <-errs:
			t.Fatalf("stream read failed: %v", err)
		case <-timeout:
			t.Fatalf("timed out waiting for ping+notification, ping=%v notification=%v", gotPing, gotNotification)
		}
	}

	stateReq, err := http.NewRequest(http.MethodGet, ts.URL+"/debug/statez", nil)
	if err != nil {
		t.Fatal(err)
	}
	stateReq.Header.Set("Authorization", "Bearer secret")
	stateResp, err := http.DefaultClient.Do(stateReq)
	if err != nil {
		t.Fatal(err)
	}
	defer stateResp.Body.Close()
	if stateResp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(stateResp.Body)
		t.Fatalf("expected 200, got %d body=%s", stateResp.StatusCode, payload)
	}
	var snapshot struct {
		Config struct {
			StreamQueueSize int      `json:"stream_queue_size"`
			AllowedRoots    []string `json:"allowed_roots"`
		} `json:"config"`
		Runtime struct {
			ActiveSessions               int `json:"active_sessions"`
			ActiveStreamSessions         int `json:"active_stream_sessions"`
			ActiveStreamConnections      int `json:"active_stream_connections"`
			ResourceSubscriptionTotal    int `json:"resource_subscription_total"`
			ResourceSubscriptionSessions int `json:"resource_subscription_sessions"`
		} `json:"runtime"`
		Counters map[string]int64 `json:"counters"`
	}
	if err := json.NewDecoder(stateResp.Body).Decode(&snapshot); err != nil {
		t.Fatalf("decode statez: %v", err)
	}
	if snapshot.Config.StreamQueueSize != 128 {
		t.Fatalf("expected stream queue size 128, got %d", snapshot.Config.StreamQueueSize)
	}
	if len(snapshot.Config.AllowedRoots) == 0 {
		t.Fatal("expected allowed roots in statez")
	}
	if snapshot.Runtime.ActiveSessions < 1 {
		t.Fatalf("expected active sessions >= 1, got %d", snapshot.Runtime.ActiveSessions)
	}
	if snapshot.Runtime.ActiveStreamSessions < 1 || snapshot.Runtime.ActiveStreamConnections < 1 {
		t.Fatalf("expected active streams/connections >= 1, got %+v", snapshot.Runtime)
	}
	if snapshot.Runtime.ResourceSubscriptionSessions < 1 || snapshot.Runtime.ResourceSubscriptionTotal < 1 {
		t.Fatalf("expected resource subscriptions >= 1, got %+v", snapshot.Runtime)
	}
	if snapshot.Counters["async_stream_attempts"] < 2 {
		t.Fatalf("expected async attempts >= 2, got %+v", snapshot.Counters)
	}
	if snapshot.Counters["async_stream_published"] < 2 {
		t.Fatalf("expected async published >= 2, got %+v", snapshot.Counters)
	}
	if snapshot.Counters["resource_notification_attempts"] < 1 || snapshot.Counters["resource_notification_sent"] < 1 {
		t.Fatalf("expected resource notification counters >= 1, got %+v", snapshot.Counters)
	}
}

func TestStatezPrunesExpiredResourceSubscriptions(t *testing.T) {
	manager := session.NewManager(time.Millisecond)
	sess, err := manager.Create(config.ProtocolLatest, nil)
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	srv := &Server{
		cfg: config.Config{
			SessionTTL:         time.Millisecond,
			SupportedProtocols: []string{config.ProtocolLatest},
		},
		sessions: manager,
		streams:  newStreamHub(1),
		resSubs:  newResourceSubscriptions(),
		metrics:  newServerMetrics(),
	}
	srv.resSubs.subscribe(sess.ID, "file:///tmp/demo.txt")
	time.Sleep(5 * time.Millisecond)

	snapshot := srv.stateSnapshot()
	runtime := snapshot["runtime"].(map[string]any)
	if runtime["active_sessions"].(int) != 0 {
		t.Fatalf("expected expired session to be pruned, got %#v", runtime)
	}
	if runtime["resource_subscription_total"].(int) != 0 {
		t.Fatalf("expected expired subscriptions to be pruned, got %#v", runtime)
	}
}

func TestChooseProtocolRejectsEmptySupportedProtocols(t *testing.T) {
	if protocol, ok := chooseProtocol("", nil); ok || protocol != "" {
		t.Fatalf("expected empty supported protocols to be rejected, got protocol=%q ok=%t", protocol, ok)
	}
}

func TestListToolsAfterInitialize(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)

	payload := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(protocolHeader) == "" {
		t.Fatal("expected protocol header on session-bound response")
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	result := body["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("expected tools")
	}
}

func TestInitializeWithCombinedAcceptPrefersJSON(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"tester","version":"1.0.0"}}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("expected JSON content type, got %q", ct)
	}
	if strings.Contains(rec.Body.String(), "event: message") {
		t.Fatalf("expected JSON body, got SSE body %q", rec.Body.String())
	}
}

func TestInitializeAcceptJSONQZeroPrefersSSE(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"tester","version":"1.0.0"}}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Accept", "application/json;q=0, text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected SSE content type, got %q body=%s", ct, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "event: message") {
		t.Fatalf("expected SSE body, got %q", rec.Body.String())
	}
}

func TestInitializeRejectsNonStringProtocolVersion(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":20251125,"clientInfo":{"name":"tester","version":"1.0.0"}}}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-string protocolVersion, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(sessionHeader) != "" {
		t.Fatalf("initialize should not create a session on invalid protocolVersion, got %q", rec.Header().Get(sessionHeader))
	}
	if !strings.Contains(rec.Body.String(), "protocolVersion must be a string") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestInitializeRejectsNonObjectClientInfo(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":["tester"]}}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-object clientInfo, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(sessionHeader) != "" {
		t.Fatalf("initialize should not create a session on invalid clientInfo, got %q", rec.Header().Get(sessionHeader))
	}
	if !strings.Contains(rec.Body.String(), "clientInfo must be an object") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestInitializeSupportsCompatProtocol(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"tester","version":"1.0.0"}}}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(protocolHeader) != config.ProtocolCompat {
		t.Fatalf("expected protocol header %q, got %q", config.ProtocolCompat, rec.Header().Get(protocolHeader))
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	result := decoded["result"].(map[string]any)
	if got, _ := result["protocolVersion"].(string); got != config.ProtocolCompat {
		t.Fatalf("expected negotiated protocol %q, got %q", config.ProtocolCompat, got)
	}
}

func TestInitializeSetsProtocolHeader(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"tester","version":"1.0.0"}}}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(protocolHeader) != config.ProtocolLatest {
		t.Fatalf("expected protocol header %q, got %q", config.ProtocolLatest, rec.Header().Get(protocolHeader))
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	result := decoded["result"].(map[string]any)
	capabilities := result["capabilities"].(map[string]any)
	resources := capabilities["resources"].(map[string]any)
	if resources["subscribe"] != true {
		t.Fatalf("expected resources.subscribe=true, got %#v", resources)
	}
	tools := capabilities["tools"].(map[string]any)
	if tools["listChanged"] != false {
		t.Fatalf("expected tools.listChanged=false, got %#v", tools)
	}
	if resources["listChanged"] != false {
		t.Fatalf("expected resources.listChanged=false, got %#v", resources)
	}
	prompts := capabilities["prompts"].(map[string]any)
	if prompts["listChanged"] != false {
		t.Fatalf("expected prompts.listChanged=false, got %#v", prompts)
	}
	if _, ok := capabilities["completions"].(map[string]any); !ok {
		t.Fatalf("expected completions capability, got %#v", capabilities)
	}
}

func TestEditLinesAndAudit(t *testing.T) {
	handler, dir, auditPath := newTestServer(t)
	sessionID := initializeSession(t, handler)
	target := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(target, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "fs.edit_lines",
			"arguments": map[string]any{
				"path":       target,
				"start_line": 2,
				"end_line":   2,
				"new_text":   "TWO\n",
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one\nTWO\nthree\n" {
		t.Fatalf("unexpected file contents: %q", string(got))
	}

	auditPayload, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(auditPayload, []byte("fs.edit_lines")) {
		t.Fatalf("missing audit entry: %s", auditPayload)
	}
}

func TestApplyUnifiedDiffAndAudit(t *testing.T) {
	handler, dir, auditPath := newTestServer(t)
	sessionID := initializeSession(t, handler)
	target := filepath.Join(dir, "apply.txt")
	if err := os.WriteFile(target, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := strings.Join([]string{
		"--- a/apply.txt",
		"+++ b/apply.txt",
		"@@ -1,3 +1,3 @@",
		" one",
		"-two",
		"+TWO",
		" three",
		"",
	}, "\n")
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      301,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "fs.apply_unified_diff",
			"arguments": map[string]any{
				"path": "apply.txt",
				"diff": diff,
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one\nTWO\nthree\n" {
		t.Fatalf("unexpected file contents: %q", string(got))
	}

	auditPayload, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(auditPayload, []byte("fs.apply_unified_diff")) {
		t.Fatalf("missing audit entry: %s", auditPayload)
	}
}

func TestApplyUnifiedDiffReturnsStructuredConflictResult(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)
	target := filepath.Join(dir, "apply-conflict.txt")
	if err := os.WriteFile(target, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := strings.Join([]string{
		"--- a/apply-conflict.txt",
		"+++ b/apply-conflict.txt",
		"@@ -1,3 +1,3 @@",
		" one",
		"-TWO",
		"+two2",
		" three",
		"",
	}, "\n")
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      302,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "fs.apply_unified_diff",
			"arguments": map[string]any{
				"path": "apply-conflict.txt",
				"diff": diff,
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	result := decoded["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("expected tool error result, got %#v", result)
	}
	structured := result["structuredContent"].(map[string]any)
	if structured["reason"] != "delete_mismatch" {
		t.Fatalf("unexpected structured conflict: %#v", structured)
	}
	if structured["hunk_index"].(float64) != 1 {
		t.Fatalf("expected hunk_index=1, got %#v", structured)
	}
	if _, ok := structured["expected_lines"]; !ok {
		t.Fatalf("expected structured expected_lines, got %#v", structured)
	}
}

func TestResourcesListAfterInitialize(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)

	payload := []byte(`{"jsonrpc":"2.0","id":7,"method":"resources/list","params":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	result := body["result"].(map[string]any)
	resources := result["resources"].([]any)
	if len(resources) == 0 {
		t.Fatal("expected resources")
	}
}

func TestResourceTemplatesListAfterInitialize(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)

	payload := []byte(`{"jsonrpc":"2.0","id":8,"method":"resources/templates/list","params":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	result := body["result"].(map[string]any)
	templates := result["resourceTemplates"].([]any)
	if len(templates) == 0 {
		t.Fatal("expected resource templates")
	}
	first := templates[0].(map[string]any)
	expectedPrefix := "file://" + dir + "/{path}"
	if first["uriTemplate"] != expectedPrefix {
		t.Fatalf("expected placeholder template %q, got %#v", expectedPrefix, first)
	}
	meta := first["_meta"].(map[string]any)
	args := meta["templateArguments"].([]any)
	if len(args) == 0 || args[0].(map[string]any)["type"] != "path" {
		t.Fatalf("expected typed template arguments, got %#v", first)
	}
}

func TestResourcesReadAfterInitialize(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      8,
		"method":  "resources/read",
		"params": map[string]any{
			"uri": "file://" + dir,
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	result := decoded["result"].(map[string]any)
	contents := result["contents"].([]any)
	if len(contents) == 0 {
		t.Fatal("expected resource contents")
	}
}

func TestResourcesReadFileAfterInitialize(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)
	target := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(target, []byte("hello resource\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      9,
		"method":  "resources/read",
		"params": map[string]any{
			"uri": "file://" + target,
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	result := decoded["result"].(map[string]any)
	contents := result["contents"].([]any)
	item := contents[0].(map[string]any)
	if item["text"] != "hello resource\n" {
		t.Fatalf("unexpected resource file contents: %#v", item)
	}
	metadata := item["_meta"].(map[string]any)
	if metadata["path"] != target {
		t.Fatalf("unexpected metadata path: %#v", metadata)
	}
	if metadata["is_dir"] != false {
		t.Fatalf("expected file metadata, got %#v", metadata)
	}
	if metadata["name"] != "note.txt" {
		t.Fatalf("unexpected metadata name: %#v", metadata)
	}
	if metadata["size"].(float64) <= 0 {
		t.Fatalf("expected positive size metadata: %#v", metadata)
	}
	if metadata["mod_time"] == "" {
		t.Fatalf("expected mod_time metadata: %#v", metadata)
	}
}

func TestResourcesRejectNonStringURI(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)

	for _, method := range []string{"resources/read", "resources/subscribe", "resources/unsubscribe"} {
		t.Run(method, func(t *testing.T) {
			payload := map[string]any{
				"jsonrpc": "2.0",
				"id":      901,
				"method":  method,
				"params": map[string]any{
					"uri": 123,
				},
			}
			body, _ := json.Marshal(payload)
			req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer secret")
			req.Header.Set(sessionHeader, sessionID)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
			}
			var decoded rpcResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Error == nil || decoded.Error.Code != -32602 {
				t.Fatalf("expected invalid params error, got %#v", decoded)
			}
			if !strings.Contains(decoded.Error.Message, "must be a string") {
				t.Fatalf("expected type error, got %#v", decoded.Error)
			}
		})
	}
}

func TestResourcesReadParsesStandardFileURIPath(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)
	target := filepath.Join(dir, "space name.txt")
	if err := os.WriteFile(target, []byte("encoded resource\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, uri := range []string{
		"file://" + strings.ReplaceAll(target, " ", "%20"),
		"file://localhost" + strings.ReplaceAll(target, " ", "%20"),
	} {
		t.Run(uri, func(t *testing.T) {
			payload := map[string]any{
				"jsonrpc": "2.0",
				"id":      92,
				"method":  "resources/read",
				"params": map[string]any{
					"uri": uri,
				},
			}
			body, _ := json.Marshal(payload)
			req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer secret")
			req.Header.Set(sessionHeader, sessionID)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
			}
			var decoded map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			result := decoded["result"].(map[string]any)
			contents := result["contents"].([]any)
			item := contents[0].(map[string]any)
			if item["uri"] != localFileURI(target) {
				t.Fatalf("expected canonical content uri, got %#v", item)
			}
			if item["text"] != "encoded resource\n" {
				t.Fatalf("unexpected resource file contents: %#v", item)
			}
		})
	}
}

func TestResourcesReadReturnsEscapedRoundTrippableURI(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)
	target := filepath.Join(dir, "frag#query?.txt")
	if err := os.WriteFile(target, []byte("roundtrip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inputURI := (&url.URL{Scheme: "file", Path: target}).String()

	readResource := func(uri string) map[string]any {
		t.Helper()
		payload := map[string]any{
			"jsonrpc": "2.0",
			"id":      93,
			"method":  "resources/read",
			"params": map[string]any{
				"uri": uri,
			},
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set(sessionHeader, sessionID)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("read %q: expected 200, got %d body=%s", uri, rec.Code, rec.Body.String())
		}
		var decoded map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		result := decoded["result"].(map[string]any)
		contents := result["contents"].([]any)
		return contents[0].(map[string]any)
	}

	item := readResource(inputURI)
	returnedURI, _ := item["uri"].(string)
	if returnedURI != inputURI {
		t.Fatalf("expected escaped canonical content uri %q, got %#v", inputURI, item)
	}
	item = readResource(returnedURI)
	if item["text"] != "roundtrip\n" {
		t.Fatalf("unexpected round-tripped resource contents: %#v", item)
	}
}

func TestParseFileResourcePathRejectsNonLocalURIParts(t *testing.T) {
	for _, uri := range []string{
		"file://example.com/tmp/resource.txt",
		"file:///tmp/resource.txt?version=1",
		"file:///tmp/resource.txt#section",
	} {
		t.Run(uri, func(t *testing.T) {
			if _, err := parseFileResourcePath(uri); err == nil {
				t.Fatal("expected invalid file resource URI to be rejected")
			}
		})
	}
}

func TestResourcesReadOutsideAllowedRootsWhenUnsafeAllowAllEnabled(t *testing.T) {
	handler, _, _ := newTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.UnsafeAllowAll = true
	})
	sessionID := initializeSession(t, handler)
	outsideDir := t.TempDir()
	target := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(target, []byte("outside resource\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      91,
		"method":  "resources/read",
		"params": map[string]any{
			"uri": "file://" + target,
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	result := decoded["result"].(map[string]any)
	contents := result["contents"].([]any)
	item := contents[0].(map[string]any)
	if item["text"] != "outside resource\n" {
		t.Fatalf("unexpected resource file contents: %#v", item)
	}
}

func TestResourcesReadDirectoryMetadataAfterInitialize(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)
	if err := os.WriteFile(filepath.Join(dir, "alpha.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      10,
		"method":  "resources/read",
		"params": map[string]any{
			"uri": "file://" + dir,
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	result := decoded["result"].(map[string]any)
	contents := result["contents"].([]any)
	item := contents[0].(map[string]any)
	metadata := item["_meta"].(map[string]any)
	if metadata["is_dir"] != true {
		t.Fatalf("expected dir metadata, got %#v", metadata)
	}
	if metadata["child_count"].(float64) < 2 {
		t.Fatalf("expected child_count metadata, got %#v", metadata)
	}
	entries := metadata["entries"].([]any)
	if len(entries) < 2 {
		t.Fatalf("expected entries metadata, got %#v", metadata)
	}
}

func TestResourcesSubscribeAndUnsubscribe(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)
	target := filepath.Join(dir, "watch.txt")
	if err := os.WriteFile(target, []byte("watch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	subscribe := map[string]any{
		"jsonrpc": "2.0",
		"id":      13,
		"method":  "resources/subscribe",
		"params": map[string]any{
			"uri": "file://" + target,
		},
	}
	body, _ := json.Marshal(subscribe)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("subscribe failed: %d body=%s", rec.Code, rec.Body.String())
	}

	unsubscribe := map[string]any{
		"jsonrpc": "2.0",
		"id":      14,
		"method":  "resources/unsubscribe",
		"params": map[string]any{
			"uri": "file://" + target,
		},
	}
	body, _ = json.Marshal(unsubscribe)
	req = httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unsubscribe failed: %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResourcesUnsubscribeRejectsInvalidURI(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)

	for _, uri := range []string{
		"http://example.com/resource.txt",
		"file://example.com/tmp/resource.txt",
		"file:///tmp/resource.txt#section",
	} {
		t.Run(uri, func(t *testing.T) {
			payload := map[string]any{
				"jsonrpc": "2.0",
				"id":      141,
				"method":  "resources/unsubscribe",
				"params": map[string]any{
					"uri": uri,
				},
			}
			body, _ := json.Marshal(payload)
			req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer secret")
			req.Header.Set(sessionHeader, sessionID)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected invalid unsubscribe URI to fail, got %d body=%s", rec.Code, rec.Body.String())
			}
			var decoded rpcResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Error == nil || decoded.Error.Code != -32602 {
				t.Fatalf("expected invalid params error, got %#v", decoded)
			}
		})
	}
}

func TestResourcesSubscribeCanonicalizesEquivalentURIForUnsubscribe(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)
	target := filepath.Join(dir, "watch-canonical.txt")
	if err := os.WriteFile(target, []byte("watch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nonCanonicalTarget := dir + string(filepath.Separator) + "." + string(filepath.Separator) + filepath.Base(target)

	postRPC := func(payload map[string]any) {
		t.Helper()
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set(sessionHeader, sessionID)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s failed: %d body=%s", payload["method"], rec.Code, rec.Body.String())
		}
	}

	postRPC(map[string]any{
		"jsonrpc": "2.0",
		"id":      131,
		"method":  "resources/subscribe",
		"params": map[string]any{
			"uri": "file://" + nonCanonicalTarget,
		},
	})
	postRPC(map[string]any{
		"jsonrpc": "2.0",
		"id":      132,
		"method":  "resources/unsubscribe",
		"params": map[string]any{
			"uri": "file://" + target,
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/debug/statez", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("statez failed: %d body=%s", rec.Code, rec.Body.String())
	}
	var snapshot struct {
		Runtime struct {
			ResourceSubscriptionTotal int `json:"resource_subscription_total"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode statez: %v", err)
	}
	if snapshot.Runtime.ResourceSubscriptionTotal != 0 {
		t.Fatalf("expected canonical unsubscribe to remove subscription, got %d", snapshot.Runtime.ResourceSubscriptionTotal)
	}
}

func TestPromptsListAfterInitialize(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)

	payload := []byte(`{"jsonrpc":"2.0","id":9,"method":"prompts/list","params":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	result := body["result"].(map[string]any)
	prompts := result["prompts"].([]any)
	if len(prompts) == 0 {
		t.Fatal("expected prompts")
	}
	first := prompts[0].(map[string]any)
	meta := first["_meta"].(map[string]any)
	inputSchema := meta["inputSchema"].(map[string]any)
	properties := inputSchema["properties"].(map[string]any)
	if _, ok := properties["task"]; !ok {
		t.Fatalf("expected task property in prompt schema, got %#v", first)
	}
	args := first["arguments"].([]any)
	if len(args) < 2 || args[0].(map[string]any)["name"] != "task" || args[1].(map[string]any)["name"] != "path" {
		t.Fatalf("expected typed prompt arguments, got %#v", first)
	}
}

func TestPromptSchemasDeclareRequiredStringMinLength(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)

	payload := []byte(`{"jsonrpc":"2.0","id":91,"method":"prompts/list","params":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	result := body["result"].(map[string]any)
	prompts := result["prompts"].([]any)
	byName := make(map[string]map[string]any, len(prompts))
	for _, raw := range prompts {
		prompt := raw.(map[string]any)
		byName[prompt["name"].(string)] = prompt
	}
	tests := []struct {
		prompt string
		prop   string
	}{
		{prompt: "safe_file_edit", prop: "task"},
		{prompt: "safe_file_edit", prop: "path"},
		{prompt: "go_dev_loop", prop: "goal"},
		{prompt: "go_dev_loop", prop: "workdir"},
	}
	for _, tt := range tests {
		t.Run(tt.prompt+"."+tt.prop, func(t *testing.T) {
			prompt, ok := byName[tt.prompt]
			if !ok {
				t.Fatalf("prompt %q not found in %#v", tt.prompt, byName)
			}
			meta := prompt["_meta"].(map[string]any)
			inputSchema := meta["inputSchema"].(map[string]any)
			properties := inputSchema["properties"].(map[string]any)
			prop := properties[tt.prop].(map[string]any)
			if got, ok := prop["minLength"].(float64); !ok || got != 1 {
				t.Fatalf("%s schema should declare %s minLength=1, got %#v", tt.prompt, tt.prop, prop)
			}
		})
	}
}

func TestPromptGetAfterInitialize(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      10,
		"method":  "prompts/get",
		"params": map[string]any{
			"name": "go_dev_loop",
			"arguments": map[string]any{
				"goal":        "fix tests",
				"workdir":     ".",
				"test_target": "./internal/httpapi",
				"run_vet":     false,
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	result := decoded["result"].(map[string]any)
	messages := result["messages"].([]any)
	if len(messages) == 0 {
		t.Fatal("expected prompt messages")
	}
	content := messages[0].(map[string]any)["content"].(map[string]any)
	text := content["text"].(string)
	if !strings.Contains(text, "./internal/httpapi") || strings.Contains(text, "go_vet") {
		t.Fatalf("unexpected prompt text customization: %q", text)
	}
}

func TestPromptGetRejectsNonStringExpectedOldText(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      10,
		"method":  "prompts/get",
		"params": map[string]any{
			"name": "safe_file_edit",
			"arguments": map[string]any{
				"task":              "update readme",
				"path":              "README.md",
				"expected_old_text": 123,
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	msg := decoded["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "expected_old_text must be a string") {
		t.Fatalf("unexpected error message: %q", msg)
	}
}

func TestPromptGetRejectsNonBooleanRunVet(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      11,
		"method":  "prompts/get",
		"params": map[string]any{
			"name": "go_dev_loop",
			"arguments": map[string]any{
				"goal":        "fix tests",
				"workdir":     ".",
				"test_target": "./internal/httpapi",
				"run_vet":     "false",
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	msg := decoded["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "run_vet must be a boolean") {
		t.Fatalf("unexpected error message: %q", msg)
	}
}

func TestPromptGetRejectsNonObjectArguments(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      13,
		"method":  "prompts/get",
		"params": map[string]any{
			"name":      "go_dev_loop",
			"arguments": []any{"not", "an", "object"},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	msg := decoded["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "arguments must be an object") {
		t.Fatalf("unexpected error message: %q", msg)
	}
}

func TestPromptGetRejectsNonStringName(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      13,
		"method":  "prompts/get",
		"params": map[string]any{
			"name": []any{"go_dev_loop"},
			"arguments": map[string]any{
				"goal":    "fix tests",
				"workdir": "/tmp/work",
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	msg := decoded["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "prompt name must be a string") {
		t.Fatalf("unexpected error message: %q", msg)
	}
}

func TestCompletionCompleteForPromptPath(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      11,
		"method":  "completion/complete",
		"params": map[string]any{
			"ref": map[string]any{
				"type": "ref/prompt",
				"name": "safe_file_edit",
			},
			"argument": map[string]any{
				"name":  "path",
				"value": "REA",
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	values := decoded["result"].(map[string]any)["completion"].(map[string]any)["values"].([]any)
	if len(values) == 0 || values[0].(string) != "README.md" {
		t.Fatalf("unexpected completion values: %#v", values)
	}
}

func TestCompletionCompleteRejectsNonStringArgumentValue(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      12,
		"method":  "completion/complete",
		"params": map[string]any{
			"ref": map[string]any{
				"type": "ref/prompt",
				"name": "safe_file_edit",
			},
			"argument": map[string]any{
				"name":  "path",
				"value": 123,
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	msg := decoded["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "argument.value must be a string") {
		t.Fatalf("unexpected error message: %q", msg)
	}
}

func TestCompletionCompleteRejectsNonStringContextWorkdir(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      13,
		"method":  "completion/complete",
		"params": map[string]any{
			"ref": map[string]any{
				"type": "ref/prompt",
				"name": "go_dev_loop",
			},
			"argument": map[string]any{
				"name":  "test_target",
				"value": "./",
			},
			"context": map[string]any{
				"arguments": map[string]any{
					"workdir": 123,
				},
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	msg := decoded["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "context.arguments.workdir must be a string") {
		t.Fatalf("unexpected error message: %q", msg)
	}
}

func TestCompletionCompleteAllowsNullContextWorkdir(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      14,
		"method":  "completion/complete",
		"params": map[string]any{
			"ref": map[string]any{
				"type": "ref/prompt",
				"name": "go_dev_loop",
			},
			"argument": map[string]any{
				"name":  "test_target",
				"value": "./",
			},
			"context": map[string]any{
				"arguments": map[string]any{
					"workdir": nil,
				},
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["error"] != nil {
		t.Fatalf("expected success, got %#v", decoded)
	}
}

func TestCompletionCompleteRejectsNonObjectContainers(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]any
		wantMsg string
	}{
		{
			name: "ref",
			params: map[string]any{
				"ref": []any{"not", "an", "object"},
				"argument": map[string]any{
					"name":  "path",
					"value": "REA",
				},
			},
			wantMsg: "ref must be an object",
		},
		{
			name: "argument",
			params: map[string]any{
				"ref": map[string]any{
					"type": "ref/prompt",
					"name": "safe_file_edit",
				},
				"argument": []any{"not", "an", "object"},
			},
			wantMsg: "argument must be an object",
		},
		{
			name: "context",
			params: map[string]any{
				"ref": map[string]any{
					"type": "ref/prompt",
					"name": "go_dev_loop",
				},
				"argument": map[string]any{
					"name":  "test_target",
					"value": "./",
				},
				"context": []any{"not", "an", "object"},
			},
			wantMsg: "context must be an object",
		},
		{
			name: "context arguments",
			params: map[string]any{
				"ref": map[string]any{
					"type": "ref/prompt",
					"name": "go_dev_loop",
				},
				"argument": map[string]any{
					"name":  "test_target",
					"value": "./",
				},
				"context": map[string]any{
					"arguments": []any{"not", "an", "object"},
				},
			},
			wantMsg: "context.arguments must be an object",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, _, _ := newTestServer(t)
			sessionID := initializeSession(t, handler)

			payload := map[string]any{
				"jsonrpc": "2.0",
				"id":      14,
				"method":  "completion/complete",
				"params":  tt.params,
			}
			body, _ := json.Marshal(payload)
			req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer secret")
			req.Header.Set(sessionHeader, sessionID)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
			}
			var decoded map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			msg := decoded["error"].(map[string]any)["message"].(string)
			if !strings.Contains(msg, tt.wantMsg) {
				t.Fatalf("expected %q, got %q", tt.wantMsg, msg)
			}
		})
	}
}

func TestCompletionCompleteForResourceTemplate(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)
	if err := os.WriteFile(filepath.Join(dir, "alpha.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      12,
		"method":  "completion/complete",
		"params": map[string]any{
			"ref": map[string]any{
				"type": "ref/resource",
				"uri":  "file://" + dir + "/{path}",
			},
			"argument": map[string]any{
				"name":  "path",
				"value": "al",
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	values := decoded["result"].(map[string]any)["completion"].(map[string]any)["values"].([]any)
	if len(values) == 0 || values[0].(string) != "alpha.txt" {
		t.Fatalf("unexpected resource completion values: %#v", values)
	}
}

func TestCompleteRelativePathRejectsTraversalPrefix(t *testing.T) {
	root := t.TempDir()
	outsideName := filepath.Base(root) + "-outside"
	outside := filepath.Join(filepath.Dir(root), outsideName)
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := completeRelativePath(root, "../"+outsideName+"/se", false)
	if len(got) != 0 {
		t.Fatalf("expected traversal completion to be rejected, got %#v", got)
	}
}

func TestCompleteRelativePathListsDirectoryPrefixChildren(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "guide.md"), []byte("guide\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := completeRelativePath(root, "docs/", false)
	if len(got) != 1 || got[0] != "docs/guide.md" {
		t.Fatalf("unexpected directory completion values: %#v", got)
	}
}

func TestCompleteRelativePathPreservesWhitespacePrefix(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, " spaced.txt"), []byte("space\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "alpha.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := completeRelativePath(root, " ", false)
	if len(got) != 1 || got[0] != " spaced.txt" {
		t.Fatalf("unexpected whitespace prefix completion values: %#v", got)
	}
}

func TestCompleteGoTestTargetResolvesWorkdirAgainstStartupDirectory(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{
		filepath.Join(root, "pkg", "sub"),
		filepath.Join(root, "other"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	srv := &Server{cfg: config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
	}}

	got := srv.completeGoTestTarget("pkg", "./")
	if len(got) != 2 || got[0] != "./..." || got[1] != "./sub/..." {
		t.Fatalf("unexpected go test target completions: %#v", got)
	}
}

func TestPathEscapeRejectedAsToolError(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      11,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "fs.read_file",
			"arguments": map[string]any{
				"path": outside,
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	result := decoded["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("expected tool error result, got %#v", result)
	}
}

func TestMissingSessionRejected(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":12,"method":"tools/list","params":{}}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestInvalidSessionReturns404(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, "missing-session")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestProtocolVersionMismatchRejected(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set("MCP-Protocol-Version", config.ProtocolLegacy)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRequestBodyTooLargeRejected(t *testing.T) {
	handler, _, _ := newTestServer(t)
	oversized := bytes.Repeat([]byte("a"), 2<<20)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(oversized))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRequestWithTrailingJSONRejected(t *testing.T) {
	handler, _, _ := newTestServer(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"ping"} {"jsonrpc":"2.0","id":2,"method":"ping"}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	errObj := decoded["error"].(map[string]any)
	if errObj["code"].(float64) != -32700 {
		t.Fatalf("expected parse error code, got %#v", errObj)
	}
}

func TestParseErrorResponseIncludesNullID(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	id, ok := decoded["id"]
	if !ok {
		t.Fatalf("expected parse error response to include id:null, got %#v", decoded)
	}
	if id != nil {
		t.Fatalf("expected id:null, got %#v", id)
	}
}

func TestRequestRejectsTopLevelArrayAsInvalidRequest(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`[{"jsonrpc":"2.0","id":56,"method":"ping"}]`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["id"] != nil {
		t.Fatalf("top-level array should produce id:null, got %#v", decoded["id"])
	}
	errObj := decoded["error"].(map[string]any)
	if errObj["code"].(float64) != -32600 {
		t.Fatalf("expected invalid request code, got %#v", errObj)
	}
}

func TestRequestMissingMethodRejectedAsInvalidRequest(t *testing.T) {
	handler, _, _ := newTestServer(t)
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":55,"params":{}}`,
		`{"jsonrpc":"2.0","id":55,"method":"","params":{}}`,
	} {
		t.Run(body, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
			req.Header.Set("Authorization", "Bearer secret")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
			}
			var decoded map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded["id"].(float64) != 55 {
				t.Fatalf("expected id to be preserved, got %#v", decoded)
			}
			errObj := decoded["error"].(map[string]any)
			if errObj["code"].(float64) != -32600 {
				t.Fatalf("expected invalid request code, got %#v", errObj)
			}
		})
	}
}

func TestRequestRejectsMissingIDForNonNotification(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","method":"ping","params":{}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["id"] != nil {
		t.Fatalf("missing id should produce id:null, got %#v", decoded["id"])
	}
	errObj := decoded["error"].(map[string]any)
	if errObj["code"].(float64) != -32600 {
		t.Fatalf("expected invalid request code, got %#v", errObj)
	}
}

func TestRequestRejectsInvalidIDType(t *testing.T) {
	handler, _, _ := newTestServer(t)
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":{},"method":"ping","params":{}}`,
		`{"jsonrpc":"2.0","id":[],"method":"ping","params":{}}`,
	} {
		t.Run(body, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
			req.Header.Set("Authorization", "Bearer secret")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
			}
			var decoded map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded["id"] != nil {
				t.Fatalf("invalid id type should produce id:null, got %#v", decoded["id"])
			}
			errObj := decoded["error"].(map[string]any)
			if errObj["code"].(float64) != -32600 {
				t.Fatalf("expected invalid request code, got %#v", errObj)
			}
		})
	}
}

func TestRequestRejectsUnsupportedJSONRPCVersionAsInvalidRequest(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"1.0","id":77,"method":"ping","params":{}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	id, ok := decoded["id"].(float64)
	if !ok || id != 77 {
		t.Fatalf("expected id to be preserved, got %#v", decoded)
	}
	errObj := decoded["error"].(map[string]any)
	if errObj["code"].(float64) != -32600 {
		t.Fatalf("expected invalid request code, got %#v", errObj)
	}
}

func TestRequestRejectsMissingJSONRPCAsInvalidRequest(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"id":76,"method":"ping","params":{}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	id, ok := decoded["id"].(float64)
	if !ok || id != 76 {
		t.Fatalf("expected id to be preserved, got %#v", decoded)
	}
	errObj := decoded["error"].(map[string]any)
	if errObj["code"].(float64) != -32600 {
		t.Fatalf("expected invalid request code, got %#v", errObj)
	}
}

func TestRequestPreservesLargeNumericID(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":9007199254740993,"method":"ping","params":{}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"id":9007199254740993`) {
		t.Fatalf("expected large numeric id to be preserved exactly, got %s", body)
	}
}

func TestDecodeRequestPreservesParamNumbersAsJSONNumber(t *testing.T) {
	req, err := decodeRequest(bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"limit":9007199254740992.5}}`))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := req.Params["limit"].(json.Number)
	if !ok {
		t.Fatalf("expected params number to decode as json.Number, got %T %#v", req.Params["limit"], req.Params["limit"])
	}
	if got.String() != "9007199254740992.5" {
		t.Fatalf("expected exact numeric token, got %q", got.String())
	}
}

func TestRequestRejectsNonStringJSONRPCAsInvalidRequest(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":2,"id":80,"method":"ping","params":{}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	id, ok := decoded["id"].(float64)
	if !ok || id != 80 {
		t.Fatalf("expected id to be preserved, got %#v", decoded)
	}
	errObj := decoded["error"].(map[string]any)
	if errObj["code"].(float64) != -32600 {
		t.Fatalf("expected invalid request code, got %#v", errObj)
	}
}

func TestRequestRejectsNonStringMethodAsInvalidRequest(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":79,"method":123,"params":{}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	id, ok := decoded["id"].(float64)
	if !ok || id != 79 {
		t.Fatalf("expected id to be preserved, got %#v", decoded)
	}
	errObj := decoded["error"].(map[string]any)
	if errObj["code"].(float64) != -32600 {
		t.Fatalf("expected invalid request code, got %#v", errObj)
	}
}

func TestRequestRejectsNonObjectParamsAsInvalidParams(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":78,"method":"ping","params":[]}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	id, ok := decoded["id"].(float64)
	if !ok || id != 78 {
		t.Fatalf("expected id to be preserved, got %#v", decoded)
	}
	errObj := decoded["error"].(map[string]any)
	if errObj["code"].(float64) != -32602 {
		t.Fatalf("expected invalid params code, got %#v", errObj)
	}
}

func TestGenericNotificationAcceptedWithoutBody(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","method":"notifications/custom","params":{"ok":true}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("expected empty body for notification, got %q", rec.Body.String())
	}
}

func TestGenericNotificationSetsProtocolHeader(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","method":"notifications/custom","params":{"ok":true}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(protocolHeader); got != config.ProtocolLatest {
		t.Fatalf("expected protocol header %q, got %q", config.ProtocolLatest, got)
	}
}

func TestNotificationProtocolVersionMismatchRejected(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","method":"notifications/custom","params":{"ok":true}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set(protocolHeader, config.ProtocolLegacy)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	errObj := decoded["error"].(map[string]any)
	if errObj["code"].(float64) != -32002 {
		t.Fatalf("expected protocol mismatch code, got %#v", errObj)
	}
}

func TestSessionDeleteSetsProtocolHeader(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)
	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(protocolHeader); got != config.ProtocolLatest {
		t.Fatalf("expected protocol header %q, got %q", config.ProtocolLatest, got)
	}
}

func TestSessionDeleteProtocolVersionMismatchRejected(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)
	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set(protocolHeader, config.ProtocolLegacy)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["error"] != "protocol version does not match session" {
		t.Fatalf("unexpected delete mismatch body: %#v", decoded)
	}
}

func TestGetStreamWithoutSessionReturns405(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "initialized session") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestGetStreamWithStaleSessionReturns405(t *testing.T) {
	handler1, _, _ := newTestServer(t)
	staleSessionID := initializeSession(t, handler1)

	handler2, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(sessionHeader, staleSessionID)
	rec := httptest.NewRecorder()
	handler2.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for stale session, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "initialized session") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestInitializeIgnoresStaleSessionHeader(t *testing.T) {
	handler1, _, _ := newTestServer(t)
	staleSessionID := initializeSession(t, handler1)

	handler2, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"tester","version":"1.0.0"}}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, staleSessionID)
	rec := httptest.NewRecorder()
	handler2.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	newSessionID := rec.Header().Get(sessionHeader)
	if newSessionID == "" {
		t.Fatal("missing new session header")
	}
	if newSessionID == staleSessionID {
		t.Fatalf("expected new session id, got stale one %q", newSessionID)
	}
}

func TestOriginRejectedWhenNotAllowed(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":13,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCORSAllowsProtocolVersionHeader(t *testing.T) {
	handler, _, _ := newTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.AllowedOrigins = []string{"https://ui.example"}
	})
	req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	req.Header.Set("Origin", "https://ui.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", rec.Code, rec.Body.String())
	}
	allowedHeaders := rec.Header().Get("Access-Control-Allow-Headers")
	for _, header := range []string{sessionHeader, protocolHeader} {
		if !strings.Contains(allowedHeaders, header) {
			t.Fatalf("expected CORS headers to allow %s, got %q", header, allowedHeaders)
		}
	}
	exposedHeaders := rec.Header().Get("Access-Control-Expose-Headers")
	for _, header := range []string{sessionHeader, protocolHeader} {
		if !strings.Contains(exposedHeaders, header) {
			t.Fatalf("expected CORS headers to expose %s, got %q", header, exposedHeaders)
		}
	}
}

func initializeSession(t *testing.T, handler http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"tester","version":"1.0.0"}}}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	sessionID := rec.Header().Get(sessionHeader)
	if sessionID == "" {
		t.Fatal("missing session header")
	}
	return sessionID
}

func TestDebugLoggingUnsupportedProtocolIncludesRequestedVersion(t *testing.T) {
	buf := captureDebugLogs(t)
	handler, _, _ := newTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.LogLevel = "DEBUG"
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2099-01-01","clientInfo":{"name":"tester","version":"1.0.0"}}}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	logs := buf.String()
	if !strings.Contains(logs, "initialize_requested_protocol: 2099-01-01") {
		t.Fatalf("expected requested protocol in logs, got %s", logs)
	}
	if !strings.Contains(logs, "rpc_error_code: -32002") {
		t.Fatalf("expected rpc error code in logs, got %s", logs)
	}
	if !strings.Contains(logs, "authorization_scheme: Bearer") {
		t.Fatalf("expected auth scheme in logs, got %s", logs)
	}
}

func TestDebugLoggingDoesNotLeakBearerToken(t *testing.T) {
	buf := captureDebugLogs(t)
	handler, _, _ := newTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.LogLevel = "DEBUG"
		cfg.BearerToken = "super-secret-token"
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{`))
	req.Header.Set("Authorization", "Bearer super-secret-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	logs := buf.String()
	if strings.Contains(logs, "super-secret-token") {
		t.Fatalf("expected logs to redact token, got %s", logs)
	}
	if !strings.Contains(logs, "authorization_present: true") {
		t.Fatalf("expected authorization presence in logs, got %s", logs)
	}
	if !strings.Contains(logs, "authorization_scheme: Bearer") {
		t.Fatalf("expected bearer scheme in logs, got %s", logs)
	}
}

func TestStatezIncludesTemplateMetadataAndCounters(t *testing.T) {
	handler, _, _ := newTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.CommandTemplates = map[string]config.CommandTemplate{
			"gomod": {
				Command:              []string{"go", "env", "GOMOD"},
				Env:                  map[string]string{"MCP_TEMPLATE_TEST": "hello"},
				AllowedWorkdirs:      []string{"."},
				Category:             "build",
				Destructive:          false,
				RequiresConfirmation: false,
				Timeout:              5 * time.Second,
				ReadOnly:             true,
			},
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	sessionID := initializeSessionHTTP(t, ts.URL)
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      999,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "exec.run_template",
			"arguments": map[string]any{
				"template": "gomod",
				"workdir":  ".",
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("template request failed: %d body=%s", rec.Code, rec.Body.String())
	}

	stateReq, err := http.NewRequest(http.MethodGet, ts.URL+"/debug/statez", nil)
	if err != nil {
		t.Fatal(err)
	}
	stateReq.Header.Set("Authorization", "Bearer secret")
	stateResp, err := http.DefaultClient.Do(stateReq)
	if err != nil {
		t.Fatal(err)
	}
	defer stateResp.Body.Close()
	if stateResp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(stateResp.Body)
		t.Fatalf("expected 200, got %d body=%s", stateResp.StatusCode, payload)
	}
	var snapshot map[string]any
	if err := json.NewDecoder(stateResp.Body).Decode(&snapshot); err != nil {
		t.Fatalf("decode statez: %v", err)
	}
	configSection := snapshot["config"].(map[string]any)
	templates := configSection["command_templates"].(map[string]any)
	gomod := templates["gomod"].(map[string]any)
	if gomod["read_only"] != true || gomod["category"] != "build" || gomod["destructive"] != false || gomod["requires_confirmation"] != false {
		t.Fatalf("expected risk metadata on template, got %#v", gomod)
	}
	envKeys := gomod["env_keys"].([]any)
	if len(envKeys) != 1 || envKeys[0].(string) != "MCP_TEMPLATE_TEST" {
		t.Fatalf("unexpected env keys: %#v", envKeys)
	}
	metrics := snapshot["template_metrics"].(map[string]any)
	if metrics["attempts"].(float64) < 1 || metrics["success"].(float64) < 1 || metrics["confirmation_blocked"].(float64) != 0 {
		t.Fatalf("unexpected template metrics: %#v", metrics)
	}
	perTemplate := metrics["per_template"].(map[string]any)
	if perTemplate["gomod"].(float64) < 1 {
		t.Fatalf("expected per-template metrics for gomod, got %#v", perTemplate)
	}
}

func TestStatezCountsTemplateConfirmationBlocked(t *testing.T) {
	handler, _, _ := newTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.CommandTemplates = map[string]config.CommandTemplate{
			"cleanup": {
				Command:              []string{"pwd"},
				Category:             "cleanup",
				Destructive:          true,
				RequiresConfirmation: true,
				Timeout:              5 * time.Second,
			},
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	sessionID := initializeSessionHTTP(t, ts.URL)
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1001,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "exec.run_template",
			"arguments": map[string]any{
				"template": "cleanup",
				"workdir":  ".",
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("template request failed: %d body=%s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	result := decoded["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("expected tool error result, got %#v", result)
	}

	stateReq, err := http.NewRequest(http.MethodGet, ts.URL+"/debug/statez", nil)
	if err != nil {
		t.Fatal(err)
	}
	stateReq.Header.Set("Authorization", "Bearer secret")
	stateResp, err := http.DefaultClient.Do(stateReq)
	if err != nil {
		t.Fatal(err)
	}
	defer stateResp.Body.Close()
	var snapshot map[string]any
	if err := json.NewDecoder(stateResp.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	metrics := snapshot["template_metrics"].(map[string]any)
	if metrics["confirmation_blocked"].(float64) < 1 {
		t.Fatalf("expected confirmation_blocked >= 1, got %#v", metrics)
	}
	blockedPerTemplate := metrics["blocked_per_template"].(map[string]any)
	if blockedPerTemplate["cleanup"].(float64) < 1 {
		t.Fatalf("expected cleanup blocked counter, got %#v", blockedPerTemplate)
	}
}

func TestTemplateMetricsDoNotCountUnrelatedErrorsAsConfirmationBlocked(t *testing.T) {
	srv := &Server{
		cfg: config.Config{
			CommandTemplates: map[string]config.CommandTemplate{
				"cleanup": {RequiresConfirmation: true},
			},
		},
		metrics: newServerMetrics(),
	}

	srv.trackTemplateCall("exec.run_template", map[string]any{
		"template": "cleanup",
	}, mcp.ErrorResult("workdir not allowed for template", mcp.AuditData{}))

	metrics := srv.metrics.templateSnapshot()
	if metrics["confirmation_blocked"].(int64) != 0 {
		t.Fatalf("unexpected confirmation_blocked for unrelated error: %#v", metrics)
	}
	blockedPerTemplate := metrics["blocked_per_template"].(map[string]int64)
	if blockedPerTemplate["cleanup"] != 0 {
		t.Fatalf("unexpected cleanup blocked counter: %#v", blockedPerTemplate)
	}
}

func TestAsyncSSETemplateCallUpdatesTemplateMetrics(t *testing.T) {
	handler, _, _ := newTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.CommandTemplates = map[string]config.CommandTemplate{
			"cleanup": {
				Command:              []string{"pwd"},
				Category:             "cleanup",
				Destructive:          true,
				RequiresConfirmation: true,
				Timeout:              5 * time.Second,
			},
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	sessionID := initializeSessionHTTP(t, ts.URL)
	events, errs, cancel := startRawSSEStreamBuffered(t, ts.URL, sessionID, 4)
	defer cancel()

	resp := asyncSSEPost(t, ts.URL, sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      2002,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "exec.run_template",
			"arguments": map[string]any{
				"template": "cleanup",
				"workdir":  ".",
			},
		},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected async 202, got %d", resp.StatusCode)
	}

	select {
	case evt := <-events:
		if id, ok := evt["id"].(float64); !ok || int(id) != 2002 {
			t.Fatalf("unexpected async response: %#v", evt)
		}
		result := evt["result"].(map[string]any)
		if result["isError"] != true {
			t.Fatalf("expected template call error result, got %#v", result)
		}
	case err := <-errs:
		t.Fatalf("stream read failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for async template response")
	}

	stateReq, err := http.NewRequest(http.MethodGet, ts.URL+"/debug/statez", nil)
	if err != nil {
		t.Fatal(err)
	}
	stateReq.Header.Set("Authorization", "Bearer secret")
	stateResp, err := http.DefaultClient.Do(stateReq)
	if err != nil {
		t.Fatal(err)
	}
	defer stateResp.Body.Close()
	var snapshot map[string]any
	if err := json.NewDecoder(stateResp.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	metrics := snapshot["template_metrics"].(map[string]any)
	if metrics["confirmation_blocked"].(float64) < 1 {
		t.Fatalf("expected async confirmation_blocked >= 1, got %#v", metrics)
	}
	blockedPerTemplate := metrics["blocked_per_template"].(map[string]any)
	if blockedPerTemplate["cleanup"].(float64) < 1 {
		t.Fatalf("expected async cleanup blocked counter, got %#v", blockedPerTemplate)
	}
}

func TestStatezIncludesExecPresetMetadata(t *testing.T) {
	handler, _, _ := newTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.ExecPresets = map[string]config.ExecPreset{
			"go_get": {
				Command:     "go",
				FixedArgs:   []string{"get"},
				AllowedArgs: []string{"-u", "-t", "-x", "-tags"},
				Env: map[string]string{
					"GOCACHE":    filepath.Join(cfg.StartupDirectory, ".mcp-tools", "cache", "go-build"),
					"GOMODCACHE": filepath.Join(cfg.StartupDirectory, ".mcp-tools", "cache", "gomod"),
					"GOTMPDIR":   filepath.Join(cfg.StartupDirectory, ".mcp-tools", "cache", "tmp"),
				},
				Timeout:  5 * time.Second,
				ReadOnly: false,
			},
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	stateReq, err := http.NewRequest(http.MethodGet, ts.URL+"/debug/statez", nil)
	if err != nil {
		t.Fatal(err)
	}
	stateReq.Header.Set("Authorization", "Bearer secret")
	stateResp, err := http.DefaultClient.Do(stateReq)
	if err != nil {
		t.Fatal(err)
	}
	defer stateResp.Body.Close()
	if stateResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(stateResp.Body)
		t.Fatalf("statez status=%d body=%s", stateResp.StatusCode, string(body))
	}
	var snapshot map[string]any
	if err := json.NewDecoder(stateResp.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	configSection := snapshot["config"].(map[string]any)
	presets := configSection["exec_presets"].(map[string]any)
	goGet := presets["go_get"].(map[string]any)
	if goGet["command"] != "go" || goGet["read_only"] != false {
		t.Fatalf("unexpected preset metadata: %#v", goGet)
	}
	envKeys := goGet["env_keys"].([]any)
	if len(envKeys) != 3 {
		t.Fatalf("expected env keys in preset metadata, got %#v", goGet)
	}
}
