package httpapi

// modern_test.go tests the 2026-07-28 modern-era protocol implementation:
// _meta per-request metadata, server/discover, resultType injection,
// -32022 error code, Mcp-Method/Mcp-Name header validation, and
// subscriptions/listen. Legacy tests in server_test.go are unaffected.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/v2up-32mb/mcp-tools/internal/config"
)

// modernRequest builds a JSON-RPC request with _meta protocol version.
func modernRequest(method string, id any, params map[string]any) []byte {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion": config.ProtocolLatest,
		"io.modelcontextprotocol/clientInfo":       map[string]any{"name": "modern-test", "version": "1.0.0"},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	body := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}
	if id != nil {
		body["id"] = id
	}
	b, _ := json.Marshal(body)
	return b
}

func doModernRequest(t *testing.T, handler http.Handler, method string, id any, params map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(modernRequest(method, id, params)))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(protocolHeader, config.ProtocolLatest)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeRPCResult(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("failed to decode response: %v\nbody: %s", err, body)
	}
	if errMsg, ok := resp["error"]; ok {
		t.Fatalf("unexpected error response: %v", errMsg)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %v", resp["result"])
	}
	return result
}

func decodeRPCErrorObj(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("failed to decode error response: %v\nbody: %s", err, body)
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("error is not an object: %v", resp["error"])
	}
	return errObj
}

// --- server/discover ---

func TestModernServerDiscoverReturnsSupportedVersions(t *testing.T) {
	handler, _, _ := newTestServer(t)
	rec := doModernRequest(t, handler, "server/discover", 1, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	result := decodeRPCResult(t, rec.Body.Bytes())
	versions, ok := result["supportedVersions"].([]any)
	if !ok {
		t.Fatalf("supportedVersions missing or wrong type: %v", result["supportedVersions"])
	}
	if len(versions) == 0 {
		t.Fatal("supportedVersions should not be empty")
	}
	// The latest version must be in the list.
	found := false
	for _, v := range versions {
		if v == config.ProtocolLatest {
			found = true
		}
	}
	if !found {
		t.Fatalf("supportedVersions does not include %s: %v", config.ProtocolLatest, versions)
	}
	// Capabilities must be present.
	caps, ok := result["capabilities"].(map[string]any)
	if !ok {
		t.Fatal("capabilities missing")
	}
	if _, ok := caps["tools"]; !ok {
		t.Fatal("tools capability missing")
	}
	// _meta.serverInfo must be present.
	meta, ok := result["_meta"].(map[string]any)
	if !ok {
		t.Fatal("_meta missing in discover result")
	}
	serverInfo, ok := meta["io.modelcontextprotocol/serverInfo"].(map[string]any)
	if !ok {
		t.Fatal("serverInfo missing in _meta")
	}
	if serverInfo["name"] != "mcp-tools-test" {
		t.Fatalf("unexpected server name: %v", serverInfo["name"])
	}
}

// --- resultType injection ---

func TestModernToolsListHasResultType(t *testing.T) {
	handler, _, _ := newTestServer(t)
	rec := doModernRequest(t, handler, "tools/list", 1, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	result := decodeRPCResult(t, rec.Body.Bytes())
	if result["resultType"] != "complete" {
		t.Fatalf("resultType should be 'complete', got %v", result["resultType"])
	}
}

func TestModernPingReturnsResultType(t *testing.T) {
	handler, _, _ := newTestServer(t)
	rec := doModernRequest(t, handler, "ping", 1, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	result := decodeRPCResult(t, rec.Body.Bytes())
	if result["resultType"] != "complete" {
		t.Fatalf("resultType should be 'complete', got %v", result["resultType"])
	}
}

// --- UnsupportedProtocolVersion error code -32022 ---

func TestModernUnsupportedProtocolVersionReturns32022(t *testing.T) {
	handler, _, _ := newTestServer(t)
	params := map[string]any{
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":   "1900-01-01",
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		},
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
		"params":  params,
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(protocolHeader, "1900-01-01")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	errObj := decodeRPCErrorObj(t, rec.Body.Bytes())
	code, ok := errObj["code"].(float64)
	if !ok {
		t.Fatalf("code is not a number: %v", errObj["code"])
	}
	if int(code) != -32022 {
		t.Fatalf("expected error code -32022, got %d", int(code))
	}
	data, ok := errObj["data"].(map[string]any)
	if !ok {
		t.Fatal("data field missing in error")
	}
	if data["requested"] != "1900-01-01" {
		t.Fatalf("expected requested '1900-01-01', got %v", data["requested"])
	}
	supported, ok := data["supported"].([]any)
	if !ok || len(supported) == 0 {
		t.Fatalf("supported list missing or empty: %v", data["supported"])
	}
}

// --- Modern tools/call works statelessly (no session) ---

func TestModernToolsCallNoSessionRequired(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	// Create a test file to read.
	testFile := dir + "/test.txt"
	if err := writeFile(testFile, "hello world"); err != nil {
		t.Fatal(err)
	}
	rec := doModernRequest(t, handler, "tools/call", 1, map[string]any{
		"name":      "fs_read_file",
		"arguments": map[string]any{"path": testFile},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	result := decodeRPCResult(t, rec.Body.Bytes())
	if result["resultType"] != "complete" {
		t.Fatalf("resultType should be 'complete', got %v", result["resultType"])
	}
}

// --- Mcp-Method header validation ---

func TestModernMcpMethodHeaderMismatchReturns32020(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	testFile := dir + "/test.txt"
	if err := writeFile(testFile, "hello"); err != nil {
		t.Fatal(err)
	}
	body := modernRequest("tools/call", 1, map[string]any{
		"name":      "fs_read_file",
		"arguments": map[string]any{"path": testFile},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(protocolHeader, config.ProtocolLatest)
	req.Header.Set("Mcp-Method", "resources/read") // mismatch!
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	errObj := decodeRPCErrorObj(t, rec.Body.Bytes())
	code, ok := errObj["code"].(float64)
	if !ok {
		t.Fatalf("code is not a number: %v", errObj["code"])
	}
	if int(code) != -32020 {
		t.Fatalf("expected error code -32020 (HeaderMismatch), got %d", int(code))
	}
}

func TestModernMcpMethodHeaderMatchSucceeds(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	testFile := dir + "/test.txt"
	if err := writeFile(testFile, "hello"); err != nil {
		t.Fatal(err)
	}
	body := modernRequest("tools/call", 1, map[string]any{
		"name":      "fs_read_file",
		"arguments": map[string]any{"path": testFile},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(protocolHeader, config.ProtocolLatest)
	req.Header.Set("Mcp-Method", "tools/call") // matches
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with matching header, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// --- Mcp-Name header validation ---

func TestModernMcpNameHeaderMismatchReturns32020(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	testFile := dir + "/test.txt"
	if err := writeFile(testFile, "hello"); err != nil {
		t.Fatal(err)
	}
	body := modernRequest("tools/call", 1, map[string]any{
		"name":      "fs_read_file",
		"arguments": map[string]any{"path": testFile},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(protocolHeader, config.ProtocolLatest)
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "wrong_tool_name") // mismatch!
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	errObj := decodeRPCErrorObj(t, rec.Body.Bytes())
	code, ok := errObj["code"].(float64)
	if !ok {
		t.Fatalf("code is not a number: %v", errObj["code"])
	}
	if int(code) != -32020 {
		t.Fatalf("expected error code -32020 (HeaderMismatch), got %d", int(code))
	}
}

// --- GET /mcp returns 405 for modern clients ---

func TestModernGetReturns405(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(protocolHeader, config.ProtocolLatest)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for modern GET, got %d", rec.Code)
	}
}

// --- DELETE /mcp returns 405 for modern clients ---

func TestModernDeleteReturns405(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(protocolHeader, config.ProtocolLatest)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for modern DELETE, got %d", rec.Code)
	}
}

// --- Legacy still works (no _meta) ---

func TestLegacyInitializeStillWorks(t *testing.T) {
	handler, _, _ := newTestServer(t)
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": config.ProtocolCompat,
			"clientInfo":      map[string]any{"name": "legacy-test", "version": "1.0.0"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for legacy initialize, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(sessionHeader) == "" {
		t.Fatal("legacy initialize should return Mcp-Session-Id")
	}
}

// --- Modern resources/list works ---

func TestModernResourcesList(t *testing.T) {
	handler, _, _ := newTestServer(t)
	rec := doModernRequest(t, handler, "resources/list", 1, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	result := decodeRPCResult(t, rec.Body.Bytes())
	if result["resultType"] != "complete" {
		t.Fatalf("resultType should be 'complete', got %v", result["resultType"])
	}
	resources, ok := result["resources"].([]any)
	if !ok {
		t.Fatal("resources should be an array")
	}
	if len(resources) == 0 {
		t.Fatal("resources should not be empty")
	}
}

// --- Modern resources/subscribe returns deprecated error ---

func TestModernResourcesSubscribeDeprecated(t *testing.T) {
	handler, _, _ := newTestServer(t)
	rec := doModernRequest(t, handler, "resources/subscribe", 1, map[string]any{
		"uri": "file:///test",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	errObj := decodeRPCErrorObj(t, rec.Body.Bytes())
	if !strings.Contains(errObj["message"].(string), "deprecated") {
		t.Fatalf("expected deprecation message, got %v", errObj["message"])
	}
}

// --- Modern unknown method returns 404 ---

func TestModernUnknownMethodReturns404(t *testing.T) {
	handler, _, _ := newTestServer(t)
	rec := doModernRequest(t, handler, "nonexistent/method", 1, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rec.Code, rec.Body.String())
	}
	errObj := decodeRPCErrorObj(t, rec.Body.Bytes())
	code, ok := errObj["code"].(float64)
	if !ok {
		t.Fatalf("code is not a number: %v", errObj["code"])
	}
	if int(code) != -32601 {
		t.Fatalf("expected -32601 (method not found), got %d", int(code))
	}
}

// --- Modern notifications return 202 ---

func TestModernNotificationReturns202(t *testing.T) {
	handler, _, _ := newTestServer(t)
	body := modernRequest("notifications/initialized", nil, nil)
	// notifications have no id
	raw := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
		"params": map[string]any{
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":   config.ProtocolLatest,
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		},
	}
	body, _ = json.Marshal(raw)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(protocolHeader, config.ProtocolLatest)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 for notification, got %d", rec.Code)
	}
}

// --- subscriptions/listen opens SSE stream ---

func TestModernSubscriptionsListenOpensStream(t *testing.T) {
	handler, _, _ := newTestServer(t)
	body := modernRequest("subscriptions/listen", 1, map[string]any{
		"notifications": map[string]any{
			"toolsListChanged": true,
		},
	})
	// Use a cancelable context so the SSE stream terminates immediately
	// after the server writes the acknowledgment.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(protocolHeader, config.ProtocolLatest)

	// Cancel after a short delay to let the server write the ack.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream content type, got %s", ct)
	}

	// Read enough of the body to find the acknowledgment event.
	bodyStr := rec.Body.String()
	if !strings.Contains(bodyStr, "notifications/subscriptions/acknowledged") {
		t.Fatalf("expected acknowledgment event in SSE stream, got: %s", bodyStr[:min(len(bodyStr), 500)])
	}
}

// --- Helper: writeFile writes a file for test fixtures ---

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// min returns the smaller of a or b.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
