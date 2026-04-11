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
	"runtime"
	"sync"
	"testing"
	"time"
)

func startRawSSEStreamBuffered(t *testing.T, baseURL, sessionID string, capacity int) (chan map[string]any, chan error, context.CancelFunc) {
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
	events := make(chan map[string]any, capacity)
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
			line = stringsTrimRightCRLF(line)
			if line == ": stream opened" {
				select {
				case <-ready:
				default:
					close(ready)
				}
				continue
			}
			if line == "" || line[0] == ':' {
				continue
			}
			if hasPrefix(line, "event: ") {
				eventType := line[len("event: "):]
				nextLine, err := reader.ReadString('\n')
				if err != nil {
					if err == io.EOF {
						return
					}
					errs <- err
					return
				}
				nextLine = stringsTrimRightCRLF(nextLine)
				if !hasPrefix(nextLine, "data: ") {
					continue
				}
				var payload map[string]any
				if err := json.Unmarshal([]byte(nextLine[len("data: "):]), &payload); err != nil {
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
		t.Fatalf("buffered stream open failed: %v", err)
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("timed out waiting for buffered SSE stream to open")
	}
	return events, errs, cancel
}

func stringsTrimRightCRLF(s string) string {
	for len(s) > 0 {
		last := s[len(s)-1]
		if last != '\n' && last != '\r' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func asyncSSEPost(t *testing.T, baseURL, sessionID string, payload map[string]any) *http.Response {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/mcp", bytes.NewReader(body))
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
	return resp
}

func TestStreamHubConcurrentPublishAndUnregisterDoesNotPanic(t *testing.T) {
	for i := 0; i < 200; i++ {
		h := newStreamHub(32)
		ch := h.register("s")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h.publish("s", rpcResponse{JSONRPC: "2.0", ID: j})
				runtime.Gosched()
			}
		}()
		go func() {
			defer wg.Done()
			runtime.Gosched()
			h.unregister("s", ch)
		}()
		wg.Wait()
	}
}

func TestStreamHubConcurrentPublishNotificationAndCloseSessionDoesNotPanic(t *testing.T) {
	for i := 0; i < 200; i++ {
		h := newStreamHub(32)
		h.register("s")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h.publishNotification("s", "notifications/resources/updated", map[string]any{"uri": "file:///tmp/test"})
				runtime.Gosched()
			}
		}()
		go func() {
			defer wg.Done()
			runtime.Gosched()
			h.closeSession("s")
		}()
		wg.Wait()
	}
}

func TestSSEConcurrentAsyncPostsDeliverAllResponses(t *testing.T) {
	handler, _, _ := newTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	sessionID := initializeSessionHTTP(t, ts.URL)
	events, errs, cancel := startRawSSEStreamBuffered(t, ts.URL, sessionID, 128)
	defer cancel()

	const requestCount = 24
	var wg sync.WaitGroup
	wg.Add(requestCount)
	for i := 0; i < requestCount; i++ {
		id := 1000 + i
		go func() {
			defer wg.Done()
			resp := asyncSSEPost(t, ts.URL, sessionID, map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"method":  "ping",
				"params":  map[string]any{},
			})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusAccepted {
				payload, _ := io.ReadAll(resp.Body)
				t.Errorf("request %d expected 202, got %d body=%s", id, resp.StatusCode, payload)
			}
		}()
	}
	wg.Wait()

	seen := make(map[int]bool, requestCount)
	timeout := time.After(5 * time.Second)
	for len(seen) < requestCount {
		select {
		case evt := <-events:
			if idValue, ok := evt["id"].(float64); ok {
				seen[int(idValue)] = true
			}
		case err := <-errs:
			t.Fatalf("stream read failed: %v", err)
		case <-timeout:
			t.Fatalf("timed out waiting for %d responses, got %d", requestCount, len(seen))
		}
	}
}

func TestSSEConcurrentSubscribersAllReceiveNotification(t *testing.T) {
	handler, dir, _ := newTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	target := filepath.Join(dir, "fanout.txt")
	if err := os.WriteFile(target, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const subscriberCount = 4
	type subscriber struct {
		sessionID string
		events    chan map[string]any
		errs      chan error
		cancel    context.CancelFunc
	}
	subscribers := make([]subscriber, 0, subscriberCount)
	for i := 0; i < subscriberCount; i++ {
		sessionID := initializeSessionHTTP(t, ts.URL)
		events, errs, cancel := startRawSSEStreamBuffered(t, ts.URL, sessionID, 32)
		subscribers = append(subscribers, subscriber{sessionID: sessionID, events: events, errs: errs, cancel: cancel})
		resp := asyncSSEPost(t, ts.URL, sessionID, map[string]any{
			"jsonrpc": "2.0",
			"id":      2000 + i,
			"method":  "resources/subscribe",
			"params": map[string]any{
				"uri": "file://" + target,
			},
		})
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("subscriber %d expected 202 subscribe, got %d", i, resp.StatusCode)
		}
		select {
		case evt := <-events:
			if idValue, ok := evt["id"].(float64); !ok || int(idValue) != 2000+i {
				t.Fatalf("subscriber %d unexpected subscribe ack: %#v", i, evt)
			}
		case err := <-errs:
			t.Fatalf("subscriber %d stream error: %v", i, err)
		case <-time.After(3 * time.Second):
			t.Fatalf("subscriber %d timed out waiting for subscribe ack", i)
		}
	}
	defer func() {
		for _, sub := range subscribers {
			sub.cancel()
		}
	}()

	writerSession := initializeSessionHTTP(t, ts.URL)
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      3000,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "fs.write_file",
			"arguments": map[string]any{
				"path": target,
				"text": "after\n",
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set(sessionHeader, writerSession)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("writer request failed: %d body=%s", rec.Code, rec.Body.String())
	}

	for i, sub := range subscribers {
		timeout := time.After(3 * time.Second)
		for {
			select {
			case evt := <-sub.events:
				if method, _ := evt["method"].(string); method == "notifications/resources/updated" {
					params := evt["params"].(map[string]any)
					if params["uri"] == "file://"+target {
						goto nextSubscriber
					}
				}
			case err := <-sub.errs:
				t.Fatalf("subscriber %d stream error: %v", i, err)
			case <-timeout:
				t.Fatalf("subscriber %d timed out waiting for notification", i)
			}
		}
	nextSubscriber:
	}
}
