package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/example/mcp-tools/internal/mcp"
)

const (
	contentTypeSSE      = "text/event-stream"
	defaultHeartbeatGap = 15 * time.Second
	sseEventMessage     = "message"
)

type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

type sseEnvelope struct {
	event   string
	payload any
}

type streamHub struct {
	mu        sync.RWMutex
	streams   map[string]map[chan sseEnvelope]struct{}
	queueSize int
}

func newStreamHub(queueSize int) *streamHub {
	if queueSize <= 0 {
		queueSize = 128
	}
	return &streamHub{streams: make(map[string]map[chan sseEnvelope]struct{}), queueSize: queueSize}
}

func (h *streamHub) register(sessionID string) chan sseEnvelope {
	ch := make(chan sseEnvelope, h.queueSize)
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.streams[sessionID]; !ok {
		h.streams[sessionID] = make(map[chan sseEnvelope]struct{})
	}
	h.streams[sessionID][ch] = struct{}{}
	return ch
}

func (h *streamHub) unregister(sessionID string, ch chan sseEnvelope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	clients, ok := h.streams[sessionID]
	if !ok {
		return
	}
	if _, ok := clients[ch]; !ok {
		return
	}
	delete(clients, ch)
	close(ch)
	if len(clients) == 0 {
		delete(h.streams, sessionID)
	}
}

func (h *streamHub) closeSession(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	clients, ok := h.streams[sessionID]
	if !ok {
		return
	}
	for ch := range clients {
		close(ch)
	}
	delete(h.streams, sessionID)
}

func (h *streamHub) has(sessionID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.streams[sessionID]) > 0
}

func (h *streamHub) activeSessionCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.streams)
}

func (h *streamHub) activeConnectionCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	total := 0
	for _, clients := range h.streams {
		total += len(clients)
	}
	return total
}

func (h *streamHub) publish(sessionID string, resp rpcResponse) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	clients := h.streams[sessionID]
	if len(clients) == 0 {
		return false
	}
	delivered := false
	for ch := range clients {
		select {
		case ch <- sseEnvelope{event: sseEventMessage, payload: resp}:
			delivered = true
		default:
		}
	}
	return delivered
}

func (h *streamHub) publishNotification(sessionID string, method string, params map[string]any) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	clients := h.streams[sessionID]
	if len(clients) == 0 {
		return false
	}
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}
	delivered := false
	for ch := range clients {
		select {
		case ch <- sseEnvelope{event: sseEventMessage, payload: payload}:
			delivered = true
		default:
		}
	}
	return delivered
}

func acceptsSSE(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	for _, part := range strings.Split(accept, ",") {
		part = strings.TrimSpace(part)
		if part == contentTypeSSE || strings.HasPrefix(part, contentTypeSSE+";") {
			return true
		}
	}
	return false
}

func acceptsJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if strings.TrimSpace(accept) == "" {
		return false
	}
	for _, part := range strings.Split(accept, ",") {
		part = strings.TrimSpace(part)
		if part == "application/json" || strings.HasPrefix(part, "application/json;") {
			return true
		}
	}
	return false
}

func prefersSingleShotSSE(r *http.Request) bool {
	return acceptsSSE(r) && !acceptsJSON(r)
}

func newSSEWriter(w http.ResponseWriter) (*sseWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", contentTypeSSE)
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	return &sseWriter{w: w, flusher: flusher}, true
}

func (sw *sseWriter) writeRPC(id any, result any) error {
	return sw.writeEvent(sseEventMessage, rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (sw *sseWriter) writeRPCError(id any, code int, message string, data any) error {
	return sw.writeEvent(sseEventMessage, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message, Data: data}})
}

func (sw *sseWriter) writeEvent(eventType string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(sw.w, "event: %s\ndata: %s\n\n", eventType, body); err != nil {
		return err
	}
	sw.flusher.Flush()
	return nil
}

func (sw *sseWriter) writeComment(text string) error {
	if _, err := fmt.Fprintf(sw.w, ": %s\n\n", text); err != nil {
		return err
	}
	sw.flusher.Flush()
	return nil
}

func (s *Server) handleMCPSSE(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	if req.Method != "initialize" {
		sessionID := strings.TrimSpace(r.Header.Get(sessionHeader))
		if sessionID != "" && s.streams.has(sessionID) {
			trackAsyncAttempt := !strings.HasPrefix(req.Method, "notifications/")
			if trackAsyncAttempt {
				s.metrics.asyncStreamAttempts.Add(1)
			}
			if sess, ok := s.lookupSession(sessionID); ok {
				w.Header().Set(protocolHeader, sess.ProtocolVersion)
			}
			if s.tryPublishToStream(r, req, sessionID) {
				if trackAsyncAttempt {
					s.metrics.asyncStreamPublished.Add(1)
				}
				writeNoContent(w)
				return
			}
			if trackAsyncAttempt {
				s.metrics.asyncStreamFallbacks.Add(1)
			}
		}
	}
	s.handleSingleShotSSE(w, r, req)
}

func (s *Server) handleSingleShotSSE(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	if strings.HasPrefix(req.Method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	sw, ok := newSSEWriter(w)
	if !ok {
		writeRPCError(w, req.ID, http.StatusInternalServerError, -32005, "streaming not supported", nil)
		return
	}

	switch req.Method {
	case "initialize":
		requested := nestedString(req.Params, "protocolVersion")
		protocol, ok := chooseProtocol(requested, s.cfg.SupportedProtocols)
		if !ok {
			s.debugLogRejected(r, &req, http.StatusOK, -32002, "unsupported protocol version", map[string]any{"supported_protocols": s.cfg.SupportedProtocols, "transport": "single_shot_sse"})
			_ = sw.writeRPCError(req.ID, -32002, "unsupported protocol version", map[string]any{"supported": s.cfg.SupportedProtocols})
			return
		}
		sess, err := s.sessions.Create(protocol, nestedMap(req.Params, "clientInfo"))
		if err != nil {
			_ = sw.writeRPCError(req.ID, -32603, "create session failed", nil)
			return
		}
		w.Header().Set(sessionHeader, sess.ID)
		w.Header().Set(protocolHeader, protocol)
		_ = sw.writeRPC(req.ID, s.initializeResult(protocol))
	case "tools/list":
		if _, ok := s.requireSessionForMode(w, r, req.ID, true); !ok {
			_ = sw.writeRPCError(req.ID, -32003, "missing or invalid session", nil)
			return
		}
		_ = sw.writeRPC(req.ID, map[string]any{"tools": s.registry.List()})
	case "resources/list":
		if _, ok := s.requireSessionForMode(w, r, req.ID, true); !ok {
			_ = sw.writeRPCError(req.ID, -32003, "missing or invalid session", nil)
			return
		}
		_ = sw.writeRPC(req.ID, map[string]any{"resources": s.listResources()})
	case "resources/templates/list":
		if _, ok := s.requireSessionForMode(w, r, req.ID, true); !ok {
			_ = sw.writeRPCError(req.ID, -32003, "missing or invalid session", nil)
			return
		}
		_ = sw.writeRPC(req.ID, map[string]any{"resourceTemplates": s.listResourceTemplates()})
	case "resources/read":
		if _, ok := s.requireSessionForMode(w, r, req.ID, true); !ok {
			_ = sw.writeRPCError(req.ID, -32003, "missing or invalid session", nil)
			return
		}
		uri, _ := req.Params["uri"].(string)
		result, err := s.readResource(uri)
		if err != nil {
			_ = sw.writeRPCError(req.ID, -32602, err.Error(), nil)
			return
		}
		_ = sw.writeRPC(req.ID, result)
	case "resources/subscribe":
		if _, ok := s.requireSessionForMode(w, r, req.ID, true); !ok {
			_ = sw.writeRPCError(req.ID, -32003, "missing or invalid session", nil)
			return
		}
		uri, _ := req.Params["uri"].(string)
		if _, err := s.readResource(uri); err != nil {
			_ = sw.writeRPCError(req.ID, -32602, err.Error(), nil)
			return
		}
		s.resSubs.subscribe(r.Header.Get(sessionHeader), uri)
		_ = sw.writeRPC(req.ID, map[string]any{})
	case "resources/unsubscribe":
		if _, ok := s.requireSessionForMode(w, r, req.ID, true); !ok {
			_ = sw.writeRPCError(req.ID, -32003, "missing or invalid session", nil)
			return
		}
		uri, _ := req.Params["uri"].(string)
		if strings.TrimSpace(uri) == "" {
			_ = sw.writeRPCError(req.ID, -32602, "resource uri required", nil)
			return
		}
		s.resSubs.unsubscribe(r.Header.Get(sessionHeader), uri)
		_ = sw.writeRPC(req.ID, map[string]any{})
	case "prompts/list":
		if _, ok := s.requireSessionForMode(w, r, req.ID, true); !ok {
			_ = sw.writeRPCError(req.ID, -32003, "missing or invalid session", nil)
			return
		}
		_ = sw.writeRPC(req.ID, map[string]any{"prompts": s.listPrompts()})
	case "prompts/get":
		if _, ok := s.requireSessionForMode(w, r, req.ID, true); !ok {
			_ = sw.writeRPCError(req.ID, -32003, "missing or invalid session", nil)
			return
		}
		name, _ := req.Params["name"].(string)
		result, err := s.getPrompt(name, normalizeMap(req.Params["arguments"]))
		if err != nil {
			_ = sw.writeRPCError(req.ID, -32602, err.Error(), nil)
			return
		}
		_ = sw.writeRPC(req.ID, result)
	case "completion/complete":
		if _, ok := s.requireSessionForMode(w, r, req.ID, true); !ok {
			_ = sw.writeRPCError(req.ID, -32003, "missing or invalid session", nil)
			return
		}
		result, err := s.complete(req.Params)
		if err != nil {
			_ = sw.writeRPCError(req.ID, -32602, err.Error(), nil)
			return
		}
		_ = sw.writeRPC(req.ID, result)
	case "tools/call":
		result, ok := s.callTool(r, req, w, true)
		if !ok {
			_ = sw.writeRPCError(req.ID, -32003, "missing or invalid session", nil)
			return
		}
		_ = sw.writeRPC(req.ID, result)
	case "ping":
		_ = sw.writeRPC(req.ID, map[string]any{})
	default:
		_ = sw.writeRPCError(req.ID, -32601, fmt.Sprintf("unknown method %q", req.Method), nil)
	}
}

func (s *Server) tryPublishToStream(r *http.Request, req rpcRequest, sessionID string) bool {
	resp, ok := s.buildStreamResponse(r, req, sessionID)
	if !ok {
		return false
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		return true
	}
	return s.streams.publish(sessionID, resp)
}

func (s *Server) buildStreamResponse(r *http.Request, req rpcRequest, sessionID string) (rpcResponse, bool) {
	sess, _ := s.lookupSession(sessionID)
	switch req.Method {
	case "notifications/initialized":
		return rpcResponse{}, true
	default:
		if strings.HasPrefix(req.Method, "notifications/") {
			return rpcResponse{}, true
		}
	}
	switch req.Method {
	case "notifications/initialized":
		return rpcResponse{}, true
	case "tools/list":
		if sess.ID == "" {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32004, Message: "invalid or expired session"}}, true
		}
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": s.registry.List()}}, true
	case "resources/list":
		if sess.ID == "" {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32004, Message: "invalid or expired session"}}, true
		}
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"resources": s.listResources()}}, true
	case "resources/templates/list":
		if sess.ID == "" {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32004, Message: "invalid or expired session"}}, true
		}
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"resourceTemplates": s.listResourceTemplates()}}, true
	case "resources/read":
		if sess.ID == "" {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32004, Message: "invalid or expired session"}}, true
		}
		uri, _ := req.Params["uri"].(string)
		result, err := s.readResource(uri)
		if err != nil {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: err.Error()}}, true
		}
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}, true
	case "resources/subscribe":
		if sess.ID == "" {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32004, Message: "invalid or expired session"}}, true
		}
		uri, _ := req.Params["uri"].(string)
		if _, err := s.readResource(uri); err != nil {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: err.Error()}}, true
		}
		s.resSubs.subscribe(sessionID, uri)
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}, true
	case "resources/unsubscribe":
		if sess.ID == "" {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32004, Message: "invalid or expired session"}}, true
		}
		uri, _ := req.Params["uri"].(string)
		if strings.TrimSpace(uri) == "" {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: "resource uri required"}}, true
		}
		s.resSubs.unsubscribe(sessionID, uri)
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}, true
	case "prompts/list":
		if sess.ID == "" {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32004, Message: "invalid or expired session"}}, true
		}
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"prompts": s.listPrompts()}}, true
	case "prompts/get":
		if sess.ID == "" {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32004, Message: "invalid or expired session"}}, true
		}
		name, _ := req.Params["name"].(string)
		result, err := s.getPrompt(name, normalizeMap(req.Params["arguments"]))
		if err != nil {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: err.Error()}}, true
		}
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}, true
	case "completion/complete":
		if sess.ID == "" {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32004, Message: "invalid or expired session"}}, true
		}
		result, err := s.complete(req.Params)
		if err != nil {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: err.Error()}}, true
		}
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}, true
	case "tools/call":
		if sess.ID == "" {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32004, Message: "invalid or expired session"}}, true
		}
		toolName, _ := req.Params["name"].(string)
		if strings.TrimSpace(toolName) == "" {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: mcpErrorResult("tool name required")}, true
		}
		args := normalizeMap(req.Params["arguments"])
		result, err := s.registry.Call(r.Context(), mcp.CallContext{
			RemoteAddr:      r.RemoteAddr,
			RequestID:       requestID(r),
			SessionID:       sess.ID,
			ProtocolVersion: sess.ProtocolVersion,
		}, toolName, args)
		if err != nil {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: mcpErrorResult(err.Error())}, true
		}
		s.maybeNotifyResourceUpdated(toolName, args, result)
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}, true
	case "ping":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}, true
	default:
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: fmt.Sprintf("unknown method %q", req.Method)}}, true
	}
}

func (s *Server) handleSSEStream(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.Header.Get(sessionHeader))
	if sessionID == "" {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET stream requires an initialized session"})
		return
	}
	sess, ok := s.lookupSession(sessionID)
	if !ok {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET stream requires an initialized session"})
		return
	}
	if err := validateRequestedProtocolAgainstSession(r, sess); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "protocol version does not match session", "expected": sess.ProtocolVersion})
		return
	}
	sw, ok := newSSEWriter(w)
	if !ok {
		writeRPCError(w, nil, http.StatusInternalServerError, -32005, "streaming not supported", nil)
		return
	}

	stream := s.streams.register(sess.ID)
	defer s.streams.unregister(sess.ID, stream)
	w.Header().Set(protocolHeader, sess.ProtocolVersion)
	_ = sw.writeComment("stream opened")

	ticker := time.NewTicker(defaultHeartbeatGap)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case resp, ok := <-stream:
			if !ok {
				return
			}
			if err := sw.writeEvent(resp.event, resp.payload); err != nil {
				return
			}
		case <-ticker.C:
			if err := sw.writeComment("heartbeat"); err != nil {
				return
			}
		}
	}
}

type resourceSubscriptions struct {
	mu   sync.RWMutex
	subs map[string]map[string]struct{}
}

func newResourceSubscriptions() *resourceSubscriptions {
	return &resourceSubscriptions{subs: make(map[string]map[string]struct{})}
}

func (r *resourceSubscriptions) subscribe(sessionID, uri string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.subs[sessionID]; !ok {
		r.subs[sessionID] = make(map[string]struct{})
	}
	r.subs[sessionID][uri] = struct{}{}
}

func (r *resourceSubscriptions) unsubscribe(sessionID, uri string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.subs[sessionID]; !ok {
		return
	}
	delete(r.subs[sessionID], uri)
	if len(r.subs[sessionID]) == 0 {
		delete(r.subs, sessionID)
	}
}

func (r *resourceSubscriptions) clearSession(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.subs, sessionID)
}

func (r *resourceSubscriptions) sessionsFor(uri string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for sessionID, uris := range r.subs {
		if _, ok := uris[uri]; ok {
			out = append(out, sessionID)
		}
	}
	return out
}

func (r *resourceSubscriptions) sessionCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.subs)
}

func (r *resourceSubscriptions) totalCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	total := 0
	for _, uris := range r.subs {
		total += len(uris)
	}
	return total
}

func mcpErrorResult(message string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": message}},
		"structuredContent": map[string]any{
			"error": message,
		},
		"isError": true,
	}
}
