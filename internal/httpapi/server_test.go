package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/mcp-tools/internal/audit"
	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
	execx "github.com/example/mcp-tools/internal/tools/execx"
	fstools "github.com/example/mcp-tools/internal/tools/fs"
	gittools "github.com/example/mcp-tools/internal/tools/git"
)

func newTestServer(t *testing.T) (http.Handler, string, string) {
	return newTestServerWithConfig(t, nil)
}

func newTestServerWithConfig(t *testing.T, mutate func(*config.Config)) (http.Handler, string, string) {
	t.Helper()
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	cfg := config.Config{
		ListenAddr:         "127.0.0.1:0",
		BearerToken:        "secret",
		AllowedRoots:       []string{dir},
		AuditLogPath:       auditPath,
		CommandTimeout:     5 * time.Second,
		OutputMaxBytes:     4096,
		StreamQueueSize:    128,
		MaxRequestBytes:    1 << 20,
		ReadHeaderTimeout:  5 * time.Second,
		ReadTimeout:        15 * time.Second,
		WriteTimeout:       30 * time.Second,
		IdleTimeout:        60 * time.Second,
		GitAllowed:         map[string]bool{"status": true, "diff": true, "log": true, "add": true, "restore": true, "commit": true, "branch": true, "switch": true, "pull": true},
		ExecPresets:        map[string]config.ExecPreset{"go_test": {Command: "go", FixedArgs: []string{"test"}, AllowedArgs: []string{"-v"}, Timeout: 5 * time.Second, ReadOnly: true}},
		StartupDirectory:   dir,
		SessionTTL:         time.Hour,
		ServerName:         "mcp-tools-test",
		ServerVersion:      "test",
		SupportedProtocols: []string{config.ProtocolLatest, config.ProtocolCompat, config.ProtocolLegacy},
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
	return NewServer(cfg, registry), dir, auditPath
}

func captureDebugHTTPLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	oldWriter := debugHTTPLogger.Writer()
	oldFlags := debugHTTPLogger.Flags()
	oldPrefix := debugHTTPLogger.Prefix()
	var buf bytes.Buffer
	debugHTTPLogger.SetOutput(&buf)
	debugHTTPLogger.SetFlags(0)
	debugHTTPLogger.SetPrefix("")
	t.Cleanup(func() {
		debugHTTPLogger.SetOutput(oldWriter)
		debugHTTPLogger.SetFlags(oldFlags)
		debugHTTPLogger.SetPrefix(oldPrefix)
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

func TestDebugHTTPLogUnsupportedProtocolIncludesRequestedVersion(t *testing.T) {
	buf := captureDebugHTTPLogs(t)
	handler, _, _ := newTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.DebugHTTPLog = true
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2099-01-01","clientInfo":{"name":"tester","version":"1.0.0"}}}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	logs := buf.String()
	if !strings.Contains(logs, `"initialize_requested_protocol":"2099-01-01"`) {
		t.Fatalf("expected requested protocol in logs, got %s", logs)
	}
	if !strings.Contains(logs, `"rpc_error_code":-32002`) {
		t.Fatalf("expected rpc error code in logs, got %s", logs)
	}
	if !strings.Contains(logs, `"authorization_scheme":"Bearer"`) {
		t.Fatalf("expected auth scheme in logs, got %s", logs)
	}
}

func TestDebugHTTPLogDoesNotLeakBearerToken(t *testing.T) {
	buf := captureDebugHTTPLogs(t)
	handler, _, _ := newTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.DebugHTTPLog = true
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
	if !strings.Contains(logs, `"authorization_present":true`) {
		t.Fatalf("expected authorization presence in logs, got %s", logs)
	}
	if !strings.Contains(logs, `"authorization_scheme":"Bearer"`) {
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
