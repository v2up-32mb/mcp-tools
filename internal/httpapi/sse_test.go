package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/mcp-tools/internal/config"
)

func parseSSEData(t *testing.T, body string) rpcResponse {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			var resp rpcResponse
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &resp); err != nil {
				t.Fatalf("unmarshal SSE payload: %v", err)
			}
			return resp
		}
	}
	t.Fatalf("missing SSE data line in body: %q", body)
	return rpcResponse{}
}

func initializeSessionSSE(t *testing.T, handler http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"tester","version":"1.0.0"}}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected SSE content type, got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "event: message") {
		t.Fatalf("expected response event type, got body=%q", rec.Body.String())
	}
	if rec.Header().Get(protocolHeader) != config.ProtocolLatest {
		t.Fatalf("expected protocol header %q, got %q", config.ProtocolLatest, rec.Header().Get(protocolHeader))
	}
	resp := parseSSEData(t, rec.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected initialize error: %#v", resp.Error)
	}
	sessionID := rec.Header().Get(sessionHeader)
	if sessionID == "" {
		t.Fatal("missing session header")
	}
	return sessionID
}

func initializeSessionHTTP(t *testing.T, baseURL string) string {
	t.Helper()
	body := bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"tester","version":"1.0.0"}}}`)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/mcp", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("initialize failed: code=%d body=%s", resp.StatusCode, payload)
	}
	sessionID := resp.Header.Get(sessionHeader)
	if sessionID == "" {
		t.Fatal("missing session header")
	}
	return sessionID
}

func startSSEStream(t *testing.T, baseURL, sessionID string) (chan rpcResponse, chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/mcp", nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	getReq.Header.Set("Authorization", "Bearer secret")
	getReq.Header.Set("Accept", "text/event-stream")
	getReq.Header.Set(sessionHeader, sessionID)

	ready := make(chan struct{})
	events := make(chan rpcResponse, 1)
	errs := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(getReq)
		if err != nil {
			errs <- err
			return
		}
		defer resp.Body.Close()
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					return
				}
				errs <- err
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == ": stream opened" {
				select {
				case <-ready:
				default:
					close(ready)
				}
				continue
			}
			if strings.HasPrefix(line, "data: ") {
				var resp rpcResponse
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &resp); err != nil {
					errs <- err
					return
				}
				events <- resp
				return
			}
		}
	}()

	select {
	case <-ready:
	case err := <-errs:
		cancel()
		t.Fatalf("stream open failed: %v", err)
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("timed out waiting for SSE stream to open")
	}
	return events, errs, cancel
}

func startRawSSEStream(t *testing.T, baseURL, sessionID string) (chan map[string]any, chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/mcp", nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	getReq.Header.Set("Authorization", "Bearer secret")
	getReq.Header.Set("Accept", "text/event-stream")
	getReq.Header.Set(sessionHeader, sessionID)

	ready := make(chan struct{})
	events := make(chan map[string]any, 2)
	errs := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(getReq)
		if err != nil {
			errs <- err
			return
		}
		defer resp.Body.Close()
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					return
				}
				errs <- err
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == ": stream opened" {
				select {
				case <-ready:
				default:
					close(ready)
				}
				continue
			}
			if strings.HasPrefix(line, "data: ") {
				var payload map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
					errs <- err
					return
				}
				payload["_event"] = "message"
				events <- payload
			}
			if strings.HasPrefix(line, "event: ") {
				eventType := strings.TrimPrefix(line, "event: ")
				nextLine, err := reader.ReadString('\n')
				if err != nil {
					if err == io.EOF {
						return
					}
					errs <- err
					return
				}
				nextLine = strings.TrimRight(nextLine, "\r\n")
				if !strings.HasPrefix(nextLine, "data: ") {
					continue
				}
				var payload map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(nextLine, "data: ")), &payload); err != nil {
					errs <- err
					return
				}
				payload["_event"] = eventType
				events <- payload
			}
		}
	}()

	select {
	case <-ready:
	case err := <-errs:
		cancel()
		t.Fatalf("raw stream open failed: %v", err)
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("timed out waiting for raw SSE stream to open")
	}
	return events, errs, cancel
}

func openSSEStreamResponse(t *testing.T, baseURL, sessionID string) (*http.Response, *bufio.Reader, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/mcp", nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(sessionHeader, sessionID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		resp.Body.Close()
		cancel()
		t.Fatal(err)
	}
	if strings.TrimRight(line, "\r\n") != ": stream opened" {
		resp.Body.Close()
		cancel()
		t.Fatalf("expected stream opened comment, got %q", line)
	}
	blank, err := reader.ReadString('\n')
	if err != nil {
		resp.Body.Close()
		cancel()
		t.Fatal(err)
	}
	if strings.TrimRight(blank, "\r\n") != "" {
		resp.Body.Close()
		cancel()
		t.Fatalf("expected blank line after stream opened, got %q", blank)
	}
	return resp, reader, cancel
}

func TestSSEInitialize(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSessionSSE(t, handler)
	if sessionID == "" {
		t.Fatal("empty session id")
	}
}

func TestSSEInitializeCapabilities(t *testing.T) {
	handler, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"tester","version":"1.0.0"}}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := parseSSEData(t, rec.Body.String())
	resultBytes, _ := json.Marshal(resp.Result)
	var result struct {
		Capabilities map[string]map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("decode initialize result: %v", err)
	}
	if result.Capabilities["resources"]["subscribe"] != true {
		t.Fatalf("expected resources.subscribe=true, got %#v", result.Capabilities)
	}
	if result.Capabilities["tools"]["listChanged"] != false {
		t.Fatalf("expected tools.listChanged=false, got %#v", result.Capabilities)
	}
	if result.Capabilities["resources"]["listChanged"] != false {
		t.Fatalf("expected resources.listChanged=false, got %#v", result.Capabilities)
	}
	if result.Capabilities["prompts"]["listChanged"] != false {
		t.Fatalf("expected prompts.listChanged=false, got %#v", result.Capabilities)
	}
	if _, ok := result.Capabilities["completions"]; !ok {
		t.Fatalf("expected completions capability, got %#v", result.Capabilities)
	}
}

func TestSSEToolsList(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSessionSSE(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := parseSSEData(t, rec.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected tools/list error: %#v", resp.Error)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var result struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(result.Tools) == 0 {
		t.Fatal("expected tools in SSE result")
	}
}

func TestSSEResourcesList(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSessionSSE(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":4,"method":"resources/list","params":{}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resources/list failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := parseSSEData(t, rec.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected resources/list error: %#v", resp.Error)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var result struct {
		Resources []map[string]any `json:"resources"`
	}
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("decode resources result: %v", err)
	}
	if len(result.Resources) == 0 {
		t.Fatal("expected resources in SSE result")
	}
}

func TestSSEResourceTemplatesList(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	sessionID := initializeSessionSSE(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":5,"method":"resources/templates/list","params":{}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resources/templates/list failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := parseSSEData(t, rec.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected resources/templates/list error: %#v", resp.Error)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var result struct {
		ResourceTemplates []map[string]any `json:"resourceTemplates"`
	}
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("decode resourceTemplates result: %v", err)
	}
	if len(result.ResourceTemplates) == 0 {
		t.Fatal("expected resource templates in SSE result")
	}
	first := result.ResourceTemplates[0]
	if first["uriTemplate"] != "file://"+dir+"/{path}" {
		t.Fatalf("unexpected SSE resource template: %#v", first)
	}
	meta := first["_meta"].(map[string]any)
	args := meta["templateArguments"].([]any)
	if len(args) == 0 || args[0].(map[string]any)["name"] != "path" {
		t.Fatalf("unexpected SSE template metadata: %#v", first)
	}
}

func TestSSEResourceRead(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	sessionID := initializeSessionSSE(t, handler)
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      5,
		"method":  "resources/read",
		"params": map[string]any{
			"uri": "file://" + dir,
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resources/read failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := parseSSEData(t, rec.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected resources/read error: %#v", resp.Error)
	}
}

func TestSSEResourceReadFile(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	sessionID := initializeSessionSSE(t, handler)
	target := filepath.Join(dir, "sse-note.txt")
	if err := os.WriteFile(target, []byte("hello sse resource\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      6,
		"method":  "resources/read",
		"params": map[string]any{
			"uri": "file://" + target,
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resources/read file failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := parseSSEData(t, rec.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected resources/read file error: %#v", resp.Error)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var result struct {
		Contents []map[string]any `json:"contents"`
	}
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("decode file resource result: %v", err)
	}
	if len(result.Contents) == 0 || result.Contents[0]["text"] != "hello sse resource\n" {
		t.Fatalf("unexpected SSE resource file contents: %#v", result.Contents)
	}
	metadata := result.Contents[0]["_meta"].(map[string]any)
	if metadata["path"] != target || metadata["is_dir"] != false {
		t.Fatalf("unexpected SSE resource metadata: %#v", metadata)
	}
}

func TestSSEPromptsList(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSessionSSE(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":6,"method":"prompts/list","params":{}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("prompts/list failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := parseSSEData(t, rec.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected prompts/list error: %#v", resp.Error)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var result struct {
		Prompts []map[string]any `json:"prompts"`
	}
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("decode prompts result: %v", err)
	}
	if len(result.Prompts) == 0 {
		t.Fatal("expected prompts in SSE result")
	}
	first := result.Prompts[0]
	meta := first["_meta"].(map[string]any)
	inputSchema := meta["inputSchema"].(map[string]any)
	properties := inputSchema["properties"].(map[string]any)
	if _, ok := properties["task"]; !ok {
		t.Fatalf("expected task property in SSE prompt schema, got %#v", first)
	}
}

func TestSSEPromptGet(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSessionSSE(t, handler)
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      7,
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
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("prompts/get failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := parseSSEData(t, rec.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected prompts/get error: %#v", resp.Error)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var result struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("decode prompt get result: %v", err)
	}
	text := result.Messages[0]["content"].(map[string]any)["text"].(string)
	if !strings.Contains(text, "./internal/httpapi") || strings.Contains(text, "go_vet") {
		t.Fatalf("unexpected SSE prompt text customization: %q", text)
	}
}

func TestSSECompletionComplete(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	sessionID := initializeSessionSSE(t, handler)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      8,
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
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("completion failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := parseSSEData(t, rec.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected completion error: %#v", resp.Error)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var result struct {
		Completion struct {
			Values []string `json:"values"`
		} `json:"completion"`
	}
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("decode completion result: %v", err)
	}
	if len(result.Completion.Values) == 0 || result.Completion.Values[0] != "README.md" {
		t.Fatalf("unexpected completion values: %#v", result.Completion.Values)
	}
}

func TestSSEStreamClosesAfterSessionDelete(t *testing.T) {
	handler, _, _ := newTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	sessionID := initializeSessionHTTP(t, ts.URL)
	events, errs, cancel := startSSEStream(t, ts.URL, sessionID)
	defer cancel()
	_ = events

	deleteReq, err := http.NewRequest(http.MethodDelete, ts.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	deleteReq.Header.Set("Authorization", "Bearer secret")
	deleteReq.Header.Set(sessionHeader, sessionID)
	deleteResp, err := http.DefaultClient.Do(deleteReq)
	if err != nil {
		t.Fatal(err)
	}
	defer deleteResp.Body.Close()
	if deleteResp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(deleteResp.Body)
		t.Fatalf("expected 204, got %d body=%s", deleteResp.StatusCode, body)
	}

	select {
	case err := <-errs:
		t.Fatalf("unexpected stream error after session delete: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	postBody := bytes.NewBufferString(`{"jsonrpc":"2.0","id":11,"method":"tools/list","params":{}}`)
	postReq, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", postBody)
	if err != nil {
		t.Fatal(err)
	}
	postReq.Header.Set("Authorization", "Bearer secret")
	postReq.Header.Set(sessionHeader, sessionID)
	postReq.Header.Set("Accept", "text/event-stream")
	postReq.Header.Set("Content-Type", "application/json")
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(postResp.Body)
		t.Fatalf("expected fallback single-shot SSE 200, got %d body=%s", postResp.StatusCode, body)
	}
	payload, err := io.ReadAll(postResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp := parseSSEData(t, string(payload))
	if resp.Error == nil || resp.Error.Code != -32003 {
		t.Fatalf("expected missing or invalid session error, got %#v body=%s", resp.Error, payload)
	}
}

func TestSSEStreamSetsProtocolHeader(t *testing.T) {
	handler, _, _ := newTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	sessionID := initializeSessionHTTP(t, ts.URL)
	resp, _, cancel := openSSEStreamResponse(t, ts.URL, sessionID)
	defer cancel()
	defer resp.Body.Close()
	if got := resp.Header.Get(protocolHeader); got != config.ProtocolLatest {
		t.Fatalf("expected stream protocol header %q, got %q", config.ProtocolLatest, got)
	}
}

func TestSSEEditLinesAndAudit(t *testing.T) {
	handler, dir, auditPath := newTestServer(t)
	sessionID := initializeSessionSSE(t, handler)
	target := filepath.Join(dir, "sse.txt")
	if err := os.WriteFile(target, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      12,
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
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/call failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := parseSSEData(t, rec.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected tools/call error: %#v", resp.Error)
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

func TestSSEStreamGETReceivesAsyncPost(t *testing.T) {
	handler, _, _ := newTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	sessionID := initializeSessionHTTP(t, ts.URL)
	events, errs, cancel := startSSEStream(t, ts.URL, sessionID)
	defer cancel()

	postBody := bytes.NewBufferString(`{"jsonrpc":"2.0","id":13,"method":"tools/list","params":{}}`)
	postReq, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", postBody)
	if err != nil {
		t.Fatal(err)
	}
	postReq.Header.Set("Authorization", "Bearer secret")
	postReq.Header.Set(sessionHeader, sessionID)
	postReq.Header.Set("Accept", "text/event-stream")
	postReq.Header.Set("Content-Type", "application/json")
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(postResp.Body)
		t.Fatalf("expected 202, got %d body=%s", postResp.StatusCode, body)
	}

	select {
	case event := <-events:
		if event.Error != nil {
			t.Fatalf("unexpected stream error: %#v", event.Error)
		}
		resultBytes, _ := json.Marshal(event.Result)
		var result struct {
			Tools []map[string]any `json:"tools"`
		}
		if err := json.Unmarshal(resultBytes, &result); err != nil {
			t.Fatalf("decode streamed result: %v", err)
		}
		if len(result.Tools) == 0 {
			t.Fatal("expected streamed tools result")
		}
	case err := <-errs:
		t.Fatalf("stream read failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for streamed event")
	}
}

func TestSSEAsyncResourceReadPublishesToStream(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	sessionID := initializeSessionHTTP(t, ts.URL)
	events, errs, cancel := startSSEStream(t, ts.URL, sessionID)
	defer cancel()

	target := filepath.Join(dir, "async-resource.txt")
	if err := os.WriteFile(target, []byte("async resource body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	postPayload := map[string]any{
		"jsonrpc": "2.0",
		"id":      15,
		"method":  "resources/read",
		"params": map[string]any{
			"uri": "file://" + target,
		},
	}
	body, _ := json.Marshal(postPayload)
	postReq, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	postReq.Header.Set("Authorization", "Bearer secret")
	postReq.Header.Set(sessionHeader, sessionID)
	postReq.Header.Set("Accept", "text/event-stream")
	postReq.Header.Set("Content-Type", "application/json")
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusAccepted {
		payload, _ := io.ReadAll(postResp.Body)
		t.Fatalf("expected 202, got %d body=%s", postResp.StatusCode, payload)
	}

	select {
	case event := <-events:
		if event.Error != nil {
			t.Fatalf("unexpected stream error: %#v", event.Error)
		}
		resultBytes, _ := json.Marshal(event.Result)
		var result struct {
			Contents []map[string]any `json:"contents"`
		}
		if err := json.Unmarshal(resultBytes, &result); err != nil {
			t.Fatalf("decode streamed resource result: %v", err)
		}
		if len(result.Contents) == 0 || result.Contents[0]["text"] != "async resource body\n" {
			t.Fatalf("unexpected streamed resource result: %#v", result.Contents)
		}
		metadata := result.Contents[0]["_meta"].(map[string]any)
		if metadata["path"] != target || metadata["is_dir"] != false {
			t.Fatalf("unexpected streamed resource metadata: %#v", metadata)
		}
	case err := <-errs:
		t.Fatalf("stream read failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for streamed resource read")
	}
}

func TestSSEAsyncPromptGetPublishesToStream(t *testing.T) {
	handler, _, _ := newTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	sessionID := initializeSessionHTTP(t, ts.URL)
	events, errs, cancel := startSSEStream(t, ts.URL, sessionID)
	defer cancel()

	postPayload := map[string]any{
		"jsonrpc": "2.0",
		"id":      16,
		"method":  "prompts/get",
		"params": map[string]any{
			"name": "safe_file_edit",
			"arguments": map[string]any{
				"task": "update readme",
				"path": "README.md",
			},
		},
	}
	body, _ := json.Marshal(postPayload)
	postReq, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	postReq.Header.Set("Authorization", "Bearer secret")
	postReq.Header.Set(sessionHeader, sessionID)
	postReq.Header.Set("Accept", "text/event-stream")
	postReq.Header.Set("Content-Type", "application/json")
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusAccepted {
		payload, _ := io.ReadAll(postResp.Body)
		t.Fatalf("expected 202, got %d body=%s", postResp.StatusCode, payload)
	}

	select {
	case event := <-events:
		if event.Error != nil {
			t.Fatalf("unexpected stream error: %#v", event.Error)
		}
		resultBytes, _ := json.Marshal(event.Result)
		var result struct {
			Description string           `json:"description"`
			Messages    []map[string]any `json:"messages"`
		}
		if err := json.Unmarshal(resultBytes, &result); err != nil {
			t.Fatalf("decode streamed prompt result: %v", err)
		}
		if result.Description == "" || len(result.Messages) == 0 {
			t.Fatalf("unexpected streamed prompt result: %#v", result)
		}
	case err := <-errs:
		t.Fatalf("stream read failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for streamed prompt get")
	}
}

func TestSSEAsyncToolErrorPublishesIsErrorResult(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	sessionID := initializeSessionHTTP(t, ts.URL)
	events, errs, cancel := startSSEStream(t, ts.URL, sessionID)
	defer cancel()

	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	postPayload := map[string]any{
		"jsonrpc": "2.0",
		"id":      14,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "fs.read_file",
			"arguments": map[string]any{
				"path": outside,
			},
		},
	}
	body, _ := json.Marshal(postPayload)
	postReq, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	postReq.Header.Set("Authorization", "Bearer secret")
	postReq.Header.Set(sessionHeader, sessionID)
	postReq.Header.Set("Accept", "text/event-stream")
	postReq.Header.Set("Content-Type", "application/json")
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusAccepted {
		payload, _ := io.ReadAll(postResp.Body)
		t.Fatalf("expected 202, got %d body=%s", postResp.StatusCode, payload)
	}

	select {
	case event := <-events:
		if event.Error != nil {
			t.Fatalf("unexpected rpc error: %#v", event.Error)
		}
		resultBytes, _ := json.Marshal(event.Result)
		var result struct {
			IsError           bool           `json:"isError"`
			StructuredContent map[string]any `json:"structuredContent"`
		}
		if err := json.Unmarshal(resultBytes, &result); err != nil {
			t.Fatalf("decode streamed tool error result: %v", err)
		}
		if !result.IsError {
			t.Fatalf("expected isError result, got %s", resultBytes)
		}
		if _, ok := result.StructuredContent["error"]; !ok {
			t.Fatalf("expected structured error content, got %v", result.StructuredContent)
		}
	case err := <-errs:
		t.Fatalf("stream read failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for streamed tool error")
	}
}

func TestSSESubscribedResourceReceivesUpdatedNotification(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	sessionID := initializeSessionHTTP(t, ts.URL)
	events, errs, cancel := startRawSSEStream(t, ts.URL, sessionID)
	defer cancel()

	target := filepath.Join(dir, "watched.txt")
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	subscribePayload := map[string]any{
		"jsonrpc": "2.0",
		"id":      20,
		"method":  "resources/subscribe",
		"params": map[string]any{
			"uri": "file://" + target,
		},
	}
	body, _ := json.Marshal(subscribePayload)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 subscribe async, got %d", resp.StatusCode)
	}
	select {
	case evt := <-events:
		if evt["id"].(float64) != 20 {
			t.Fatalf("unexpected subscribe response: %#v", evt)
		}
	case err := <-errs:
		t.Fatalf("stream read failed after subscribe: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for subscribe response")
	}

	writePayload := map[string]any{
		"jsonrpc": "2.0",
		"id":      21,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "fs.write_file",
			"arguments": map[string]any{
				"path": target,
				"text": "new\n",
			},
		},
	}
	body, _ = json.Marshal(writePayload)
	req, err = http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 write async, got %d", resp.StatusCode)
	}

	gotResponse := false
	gotNotification := false
	timeout := time.After(3 * time.Second)
	for !(gotResponse && gotNotification) {
		select {
		case evt := <-events:
			if method, _ := evt["method"].(string); method == "notifications/resources/updated" {
				params := evt["params"].(map[string]any)
				if params["uri"] != "file://"+target {
					t.Fatalf("unexpected notification params: %#v", evt)
				}
				gotNotification = true
				continue
			}
			if id, ok := evt["id"].(float64); ok && int(id) == 21 {
				gotResponse = true
				continue
			}
		case err := <-errs:
			t.Fatalf("stream read failed: %v", err)
		case <-timeout:
			t.Fatalf("timed out waiting for response+notification, response=%v notification=%v", gotResponse, gotNotification)
		}
	}
}

func TestSSESubscribedDirectoryReceivesChildUpdateNotification(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	sessionID := initializeSessionHTTP(t, ts.URL)
	events, errs, cancel := startRawSSEStream(t, ts.URL, sessionID)
	defer cancel()

	targetDir := filepath.Join(dir, "watched-dir")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(targetDir, "child.txt")

	subscribePayload := map[string]any{
		"jsonrpc": "2.0",
		"id":      22,
		"method":  "resources/subscribe",
		"params": map[string]any{
			"uri": "file://" + targetDir,
		},
	}
	body, _ := json.Marshal(subscribePayload)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case evt := <-events:
		if evt["_event"] != "message" {
			t.Fatalf("expected subscribe ack, got %#v", evt)
		}
	case err := <-errs:
		t.Fatalf("stream read failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for subscribe ack")
	}

	writePayload := map[string]any{
		"jsonrpc": "2.0",
		"id":      23,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "fs.write_file",
			"arguments": map[string]any{
				"path": child,
				"text": "child\n",
			},
		},
	}
	body, _ = json.Marshal(writePayload)
	req, err = http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	gotResponse := false
	gotNotification := false
	timeout := time.After(3 * time.Second)
	for !(gotResponse && gotNotification) {
		select {
		case evt := <-events:
			if method, _ := evt["method"].(string); method == "notifications/resources/updated" {
				params := evt["params"].(map[string]any)
				if params["uri"] == "file://"+targetDir {
					gotNotification = true
				}
				continue
			}
			if id, ok := evt["id"].(float64); ok && int(id) == 23 {
				gotResponse = true
			}
		case err := <-errs:
			t.Fatalf("stream read failed: %v", err)
		case <-timeout:
			t.Fatalf("timed out waiting for response+directory notification, response=%v notification=%v", gotResponse, gotNotification)
		}
	}
}

func TestSSEMoveAndDeletePublishResourceNotifications(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	sessionID := initializeSessionHTTP(t, ts.URL)
	events, errs, cancel := startRawSSEStream(t, ts.URL, sessionID)
	defer cancel()

	src := filepath.Join(dir, "move-src.txt")
	dst := filepath.Join(dir, "move-dst.txt")
	if err := os.WriteFile(src, []byte("move\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	subscribe := func(id int, uri string) {
		payload := map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  "resources/subscribe",
			"params": map[string]any{
				"uri": uri,
			},
		}
		body, _ := json.Marshal(payload)
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set(sessionHeader, sessionID)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		select {
		case <-events:
		case err := <-errs:
			t.Fatalf("stream read failed: %v", err)
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for subscribe ack")
		}
	}

	subscribe(30, "file://"+src)

	movePayload := map[string]any{
		"jsonrpc": "2.0",
		"id":      31,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "fs.move_path",
			"arguments": map[string]any{
				"src": src,
				"dst": dst,
			},
		},
	}
	body, _ := json.Marshal(movePayload)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	seen := map[string]bool{}
	timeout := time.After(3 * time.Second)
	for len(seen) < 1 {
		select {
		case evt := <-events:
			if evt["method"] == "notifications/resources/updated" {
				uri := evt["params"].(map[string]any)["uri"].(string)
				seen[uri] = true
			}
		case err := <-errs:
			t.Fatalf("stream read failed: %v", err)
		case <-timeout:
			t.Fatalf("timed out waiting for move notifications: %#v", seen)
		}
	}
	if !seen["file://"+src] {
		t.Fatalf("expected src notification, got %#v", seen)
	}

	subscribe(31, "file://"+dst)

	deletePayload := map[string]any{
		"jsonrpc": "2.0",
		"id":      32,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "fs.delete_path",
			"arguments": map[string]any{
				"path": dst,
			},
		},
	}
	body, _ = json.Marshal(deletePayload)
	req, err = http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	timeout = time.After(3 * time.Second)
	for {
		select {
		case evt := <-events:
			if evt["method"] == "notifications/resources/updated" {
				uri := evt["params"].(map[string]any)["uri"].(string)
				if uri == "file://"+dst {
					return
				}
			}
		case err := <-errs:
			t.Fatalf("stream read failed: %v", err)
		case <-timeout:
			t.Fatal("timed out waiting for delete notification")
		}
	}
}

func TestSSEUnsubscribeStopsResourceNotifications(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	sessionID := initializeSessionHTTP(t, ts.URL)
	events, errs, cancel := startRawSSEStream(t, ts.URL, sessionID)
	defer cancel()

	target := filepath.Join(dir, "unsubscribe.txt")
	if err := os.WriteFile(target, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for id, method := range []string{"resources/subscribe", "resources/unsubscribe"} {
		payload := map[string]any{
			"jsonrpc": "2.0",
			"id":      40 + id,
			"method":  method,
			"params": map[string]any{
				"uri": "file://" + target,
			},
		}
		body, _ := json.Marshal(payload)
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set(sessionHeader, sessionID)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		select {
		case evt := <-events:
			if _, ok := evt["id"].(float64); !ok {
				t.Fatalf("expected response ack for %s, got %#v", method, evt)
			}
		case err := <-errs:
			t.Fatalf("stream read failed: %v", err)
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for subscribe/unsubscribe ack")
		}
	}

	writePayload := map[string]any{
		"jsonrpc": "2.0",
		"id":      50,
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
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	gotResponse := false
	timeout := time.After(1500 * time.Millisecond)
	for !gotResponse {
		select {
		case evt := <-events:
			if method, _ := evt["method"].(string); method == "notifications/resources/updated" {
				t.Fatalf("unexpected notification after unsubscribe: %#v", evt)
			}
			if id, ok := evt["id"].(float64); ok && int(id) == 50 {
				gotResponse = true
			}
		case err := <-errs:
			t.Fatalf("stream read failed: %v", err)
		case <-timeout:
			t.Fatal("timed out waiting for write response")
		}
	}

	select {
	case evt := <-events:
		if method, _ := evt["method"].(string); method == "notifications/resources/updated" {
			t.Fatalf("unexpected late notification after unsubscribe: %#v", evt)
		}
	case err := <-errs:
		t.Fatalf("stream read failed: %v", err)
	case <-time.After(400 * time.Millisecond):
	}
}

func TestAsyncAcceptedResponseSetsProtocolHeader(t *testing.T) {
	handler, _, _ := newTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	sessionID := initializeSessionHTTP(t, ts.URL)
	resp, _, cancel := openSSEStreamResponse(t, ts.URL, sessionID)
	defer cancel()
	defer resp.Body.Close()

	postBody := bytes.NewBufferString(`{"jsonrpc":"2.0","id":90,"method":"tools/list","params":{}}`)
	postReq, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", postBody)
	if err != nil {
		t.Fatal(err)
	}
	postReq.Header.Set("Authorization", "Bearer secret")
	postReq.Header.Set(sessionHeader, sessionID)
	postReq.Header.Set("Accept", "text/event-stream")
	postReq.Header.Set("Content-Type", "application/json")
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusAccepted {
		payload, _ := io.ReadAll(postResp.Body)
		t.Fatalf("expected 202, got %d body=%s", postResp.StatusCode, payload)
	}
	if got := postResp.Header.Get(protocolHeader); got != config.ProtocolLatest {
		t.Fatalf("expected async response protocol header %q, got %q", config.ProtocolLatest, got)
	}
}

func TestJSONModeUnaffectedWithoutSSEAccept(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)
	if sessionID == "" {
		t.Fatal("expected JSON initialize to keep working")
	}
}

func TestSSEGenericNotificationAcceptedWithoutBodyAndProtocolHeader(t *testing.T) {
	handler, _, _ := newTestServer(t)
	sessionID := initializeSession(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","method":"notifications/custom","params":{"ok":true}}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(protocolHeader); got != config.ProtocolLatest {
		t.Fatalf("expected protocol header %q, got %q", config.ProtocolLatest, got)
	}
	if strings.TrimSpace(rec.Body.String()) != "" {
		t.Fatalf("expected empty body, got %q", rec.Body.String())
	}
}

func TestSSEStreamProtocolVersionMismatchRejected(t *testing.T) {
	handler, _, _ := newTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	sessionID := initializeSessionHTTP(t, ts.URL)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set(protocolHeader, config.ProtocolLegacy)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("protocol version does not match session")) {
		t.Fatalf("unexpected body: %s", body)
	}
}
