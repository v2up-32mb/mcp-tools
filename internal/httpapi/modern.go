package httpapi

// modern.go implements the 2026-07-28 "modern" MCP protocol era:
// stateless per-request metadata via _meta, server/discover, resultType
// injection, Mcp-Method/Mcp-Name header validation, and
// subscriptions/listen. Legacy (2025-11-25 and earlier) request handling
// in server.go and sse.go is untouched; this file adds the dual-era layer.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/v2up-32mb/mcp-tools/internal/mcp"
)

// metaKeyProtocolVersion is the _meta key for per-request protocol version.
const metaKeyProtocolVersion = "io.modelcontextprotocol/protocolVersion"
const metaKeyClientInfo = "io.modelcontextprotocol/clientInfo"
const metaKeyClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
const metaKeyServerInfo = "io.modelcontextprotocol/serverInfo"
const metaKeySubscriptionId = "io.modelcontextprotocol/subscriptionId"

// Modern error codes defined by the 2026-07-28 specification.
const (
	errCodeUnsupportedProtocolVersion = -32022
	errCodeHeaderMismatch              = -32020
)

// isModernRequest returns true when the request carries per-request
// protocol-version metadata in _meta, indicating a modern-era client.
func isModernRequest(req rpcRequest) bool {
	return metaProtocolVersion(req) != ""
}

// metaProtocolVersion extracts io.modelcontextprotocol/protocolVersion
// from params._meta. Returns "" when absent or not a string.
func metaProtocolVersion(req rpcRequest) string {
	if req.Params == nil {
		return ""
	}
	meta, ok := req.Params["_meta"].(map[string]any)
	if !ok {
		return ""
	}
	pv, ok := meta[metaKeyProtocolVersion].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(pv)
}

// metaClientInfo extracts io.modelcontextprotocol/clientInfo from
// params._meta. Returns nil when absent.
func metaClientInfo(req rpcRequest) map[string]any {
	if req.Params == nil {
		return nil
	}
	meta, ok := req.Params["_meta"].(map[string]any)
	if !ok {
		return nil
	}
	info, _ := meta[metaKeyClientInfo].(map[string]any)
	return info
}

// writeModernResult writes a JSON-RPC result response with the required
// resultType field. Handles both map[string]any and struct results by
// round-tripping through JSON to inject resultType when absent.
func writeModernResult(w http.ResponseWriter, id any, result any) {
	if m, ok := result.(map[string]any); ok {
		if _, exists := m["resultType"]; !exists {
			copied := make(map[string]any, len(m)+1)
			for k, v := range m {
				copied[k] = v
			}
			copied["resultType"] = "complete"
			result = copied
		}
	} else {
		// For struct results (e.g. mcp.Result), marshal to map to inject resultType.
			b, err := json.Marshal(result)
			if err == nil {
				var m map[string]any
				if json.Unmarshal(b, &m) == nil {
					if _, exists := m["resultType"]; !exists {
						m["resultType"] = "complete"
						result = m
					}
				}
		}
	}
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

// writeModernError writes a JSON-RPC error response with a modern error
// code and the requested version echoed in data.requested.
func writeModernError(w http.ResponseWriter, id any, status, code int, message string, data any) {
	writeJSON(w, status, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message, Data: data}})
}

// writeUnsupportedProtocolVersion responds with -32022 and the server's
// supported version list, as required by the 2026-07-28 spec.
func (s *Server) writeUnsupportedProtocolVersion(w http.ResponseWriter, id any, requested string) {
	writeModernError(w, id, http.StatusBadRequest, errCodeUnsupportedProtocolVersion,
		"Unsupported protocol version",
		map[string]any{
			"supported": s.cfg.SupportedProtocols,
			"requested": requested,
		})
}

// handleModernDispatch routes a modern-era request (carrying _meta
// protocol version) through the stateless path. No session is required.
func (s *Server) handleModernDispatch(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	// Validate the requested protocol version is supported.
	requested := metaProtocolVersion(req)
	if _, ok := chooseProtocol(requested, s.cfg.SupportedProtocols); !ok {
		s.writeUnsupportedProtocolVersion(w, req.ID, requested)
		return
	}

	// Set MCP-Protocol-Version response header.
	w.Header().Set(protocolHeader, requested)

	// Notifications: return 202 Accepted with no body.
	if strings.HasPrefix(req.Method, "notifications/") {
		writeAccepted(w)
		return
	}

	switch req.Method {
	case "server/discover":
		s.handleServerDiscover(w, r, req)
	case "initialize":
		// A modern client normally does not call initialize, but a dual-era
		// server SHOULD respond with supported versions so legacy clients
		// that mistakenly send initialize to a modern server get an error
		// naming the supported versions.
		s.writeUnsupportedProtocolVersion(w, req.ID, requested)
	case "tools/list":
		s.handleModernToolsList(w, r, req)
	case "tools/call":
		s.handleModernToolsCall(w, r, req)
	case "resources/list":
		s.handleModernResourcesList(w, r, req)
	case "resources/templates/list":
		s.handleModernResourceTemplatesList(w, r, req)
	case "resources/read":
		s.handleModernResourcesRead(w, r, req)
	case "resources/subscribe":
		// Deprecated in 2026-07-28; replaced by subscriptions/listen.
		writeModernError(w, req.ID, http.StatusBadRequest, -32601,
			"resources/subscribe is deprecated; use subscriptions/listen", nil)
	case "resources/unsubscribe":
		writeModernError(w, req.ID, http.StatusBadRequest, -32601,
			"resources/unsubscribe is deprecated; use subscriptions/listen", nil)
	case "prompts/list":
		s.handleModernPromptsList(w, r, req)
	case "prompts/get":
		s.handleModernPromptGet(w, r, req)
	case "completion/complete":
		s.handleModernCompletion(w, r, req)
	case "subscriptions/listen":
		s.handleSubscriptionsListen(w, r, req)
	case "ping":
		writeModernResult(w, req.ID, map[string]any{})
	default:
		writeModernError(w, req.ID, http.StatusNotFound, -32601,
			fmt.Sprintf("unknown method %q", req.Method), nil)
	}
}

// handleServerDiscover implements the server/discover method (MUST per spec).
func (s *Server) handleServerDiscover(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	result := map[string]any{
		"resultType":       "complete",
		"supportedVersions": s.cfg.SupportedProtocols,
		"capabilities": map[string]any{
			"tools": map[string]any{
				"listChanged": false,
			},
			"resources": map[string]any{
				"subscribe":   true,
				"listChanged": false,
			},
			"prompts": map[string]any{
				"listChanged": false,
			},
			"completions": map[string]any{},
		},
		"_meta": map[string]any{
			metaKeyServerInfo: map[string]any{
				"name":    s.cfg.ServerName,
				"version": s.cfg.ServerVersion,
			},
		},
	}
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}

// --- Modern stateless handlers ---

func (s *Server) handleModernToolsList(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	if err := s.validateStandardHeaders(w, r, req); err != nil {
		return
	}
	writeModernResult(w, req.ID, map[string]any{"tools": s.registry.List()})
}

func (s *Server) handleModernToolsCall(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	if err := s.validateStandardHeaders(w, r, req); err != nil {
		return
	}
	toolName, err := requiredStringParam(req.Params, "name", "tool name")
	if err != nil {
		writeModernError(w, req.ID, http.StatusBadRequest, -32602, err.Error(), nil)
		return
	}
	args, err := optionalObjectParam(req.Params, "arguments")
	if err != nil {
		writeModernError(w, req.ID, http.StatusBadRequest, -32602, err.Error(), nil)
		return
	}
	protocolVersion := metaProtocolVersion(req)
	result, err := s.registry.Call(r.Context(), mcp.CallContext{
		RemoteAddr:      r.RemoteAddr,
		Host:            r.Host,
		RequestID:       requestID(r),
		SessionID:       "",
		ProtocolVersion: protocolVersion,
	}, toolName, args)
	if err != nil {
		writeModernError(w, req.ID, http.StatusBadRequest, -32010, err.Error(), nil)
		return
	}
	s.trackTemplateCall(toolName, args, result)
	s.maybeNotifyResourceUpdated(toolName, args, result)
	writeModernResult(w, req.ID, result)
}

func (s *Server) handleModernResourcesList(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	writeModernResult(w, req.ID, map[string]any{"resources": s.listResources()})
}

func (s *Server) handleModernResourceTemplatesList(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	writeModernResult(w, req.ID, map[string]any{"resourceTemplates": s.listResourceTemplates()})
}

func (s *Server) handleModernResourcesRead(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	if err := s.validateStandardHeaders(w, r, req); err != nil {
		return
	}
	uri, err := resourceURIParam(req.Params)
	if err != nil {
		writeModernError(w, req.ID, http.StatusBadRequest, -32602, err.Error(), nil)
		return
	}
	result, err := s.readResource(uri)
	if err != nil {
		writeModernError(w, req.ID, http.StatusBadRequest, -32602, err.Error(), nil)
		return
	}
	writeModernResult(w, req.ID, result)
}

func (s *Server) handleModernPromptsList(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	writeModernResult(w, req.ID, map[string]any{"prompts": s.listPrompts()})
}

func (s *Server) handleModernPromptGet(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	if err := s.validateStandardHeaders(w, r, req); err != nil {
		return
	}
	result, rpcErr := s.promptGetResult(req)
	if rpcErr != nil {
		writeModernError(w, req.ID, http.StatusBadRequest, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		return
	}
	writeModernResult(w, req.ID, result)
}

func (s *Server) handleModernCompletion(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	result, err := s.complete(req.Params)
	if err != nil {
		writeModernError(w, req.ID, http.StatusBadRequest, -32602, err.Error(), nil)
		return
	}
	writeModernResult(w, req.ID, result)
}

// validateStandardHeaders validates Mcp-Method and Mcp-Name headers
// against the request body for tools/call, resources/read, and prompts/get.
// On mismatch, it writes a -32020 HeaderMismatch error and returns an error.
func (s *Server) validateStandardHeaders(w http.ResponseWriter, r *http.Request, req rpcRequest) error {
	// Mcp-Method: must match req.Method if the header is present.
	headerMethod := r.Header.Get("Mcp-Method")
	if headerMethod != "" && headerMethod != req.Method {
		writeModernError(w, req.ID, http.StatusBadRequest, errCodeHeaderMismatch,
			fmt.Sprintf("Header mismatch: Mcp-Method header value %q does not match body value %q", headerMethod, req.Method), nil)
		return fmt.Errorf("header mismatch")
	}

	// Mcp-Name: required for tools/call, resources/read, prompts/get.
	var bodyName string
	switch req.Method {
	case "tools/call":
		bodyName, _ = req.Params["name"].(string)
	case "resources/read":
		bodyName, _ = req.Params["uri"].(string)
	case "prompts/get":
		bodyName, _ = req.Params["name"].(string)
	}
	if bodyName != "" {
		headerName := decodeHeaderValue(r.Header.Get("Mcp-Name"))
		if headerName != "" && headerName != bodyName {
			writeModernError(w, req.ID, http.StatusBadRequest, errCodeHeaderMismatch,
				fmt.Sprintf("Header mismatch: Mcp-Name header value %q does not match body value %q", headerName, bodyName), nil)
			return fmt.Errorf("header mismatch")
		}
	}
	return nil
}

// decodeHeaderValue decodes a Base64-sentinel-encoded header value per the
// 2026-07-28 spec. If the value starts with =?base64? and ends with ?=,
// it is Base64-decoded; otherwise it is returned as-is.
func decodeHeaderValue(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=") {
		encoded := value[len("=?base64?") : len(value)-len("?=")]
		decoded, err := base64Decode(encoded)
		if err != nil {
			return value
		}
		return decoded
	}
	return value
}

func base64Decode(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		// Try RawStdEncoding (no padding) as fallback
		b, err = base64.RawStdEncoding.DecodeString(s)
		if err != nil {
			return "", err
		}
	}
	return string(b), nil
}

// handleSubscriptionsListen implements subscriptions/listen for the modern
// era. It opens a long-lived SSE stream that delivers change notifications.
func (s *Server) handleSubscriptionsListen(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	// Parse the notifications filter from params.
	params := req.Params
	var toolsListChanged, promptsListChanged, resourcesListChanged bool
	var resourceSubscriptions []string
	if notif, ok := params["notifications"].(map[string]any); ok {
		if v, ok := notif["toolsListChanged"].(bool); ok {
			toolsListChanged = v
		}
		if v, ok := notif["promptsListChanged"].(bool); ok {
			promptsListChanged = v
		}
		if v, ok := notif["resourcesListChanged"].(bool); ok {
			resourcesListChanged = v
		}
		if arr, ok := notif["resourceSubscriptions"].([]any); ok {
			for _, item := range arr {
				if str, ok := item.(string); ok {
					resourceSubscriptions = append(resourceSubscriptions, str)
				}
			}
		}
	}

	protocolVersion := metaProtocolVersion(req)
	w.Header().Set(protocolHeader, protocolVersion)

	sw, ok := newSSEWriter(w)
	if !ok {
		writeModernError(w, req.ID, http.StatusInternalServerError, -32005, "streaming not supported", nil)
		return
	}

	// Register subscription and get a notification channel.
	subID := fmt.Sprintf("%v", req.ID)
	ch := s.streams.register(subID)
	defer s.streams.unregister(subID, ch)

	// Register resource subscriptions.
	for _, uri := range resourceSubscriptions {
		s.resSubs.subscribe(subID, uri)
	}
	defer s.resSubs.clearSession(subID)

	// Send acknowledgment as first message.
	ackNotif := map[string]any{}
	if toolsListChanged {
		ackNotif["toolsListChanged"] = true
	}
	if promptsListChanged {
		ackNotif["promptsListChanged"] = true
	}
	if resourcesListChanged {
		ackNotif["resourcesListChanged"] = true
	}
	if len(resourceSubscriptions) > 0 {
		ackNotif["resourceSubscriptions"] = resourceSubscriptions
	}
	ackPayload := map[string]any{
		"_meta": map[string]any{
			metaKeySubscriptionId: req.ID,
		},
		"notifications": ackNotif,
	}
	_ = sw.writeEvent("message", map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/subscriptions/acknowledged",
		"params":  ackPayload,
	})

	// SSE keep-alive header for reverse proxies.
	w.Header().Set("X-Accel-Buffering", "no")

	ticker := time.NewTicker(defaultHeartbeatGap)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case resp, ok := <-ch:
			if !ok {
				return
			}
			// Inject subscriptionId into the notification _meta.
			if payload, ok := resp.payload.(map[string]any); ok {
				if _, hasMeta := payload["_meta"]; !hasMeta {
					payload["_meta"] = map[string]any{metaKeySubscriptionId: req.ID}
				} else if meta, ok := payload["_meta"].(map[string]any); ok {
					if _, has := meta[metaKeySubscriptionId]; !has {
						meta[metaKeySubscriptionId] = req.ID
					}
				}
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
