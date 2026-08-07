package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/example/mcp-tools/internal/util"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
	"github.com/example/mcp-tools/internal/pullfile"
	"github.com/example/mcp-tools/internal/security"
	"github.com/example/mcp-tools/internal/session"
)

const sessionHeader = "Mcp-Session-Id"
const protocolHeader = "MCP-Protocol-Version"

type Server struct {
	cfg      config.Config
	registry *mcp.Registry
	sessions *session.Manager
	streams  *streamHub
	resSubs  *resourceSubscriptions
	metrics  *serverMetrics
	pull     *pullfile.Manager
}

type rpcRequest struct {
	JSONRPC        string         `json:"jsonrpc"`
	ID             any            `json:"id,omitempty"`
	Method         string         `json:"method"`
	Params         map[string]any `json:"params,omitempty"`
	idPresent      bool
	requestInvalid bool
	jsonrpcInvalid bool
	methodInvalid  bool
	paramsInvalid  bool
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

func NewServer(cfg config.Config, registry *mcp.Registry, pull ...*pullfile.Manager) http.Handler {
	srv := &Server{
		cfg:      cfg,
		registry: registry,
		sessions: session.NewManager(cfg.SessionTTL),
		streams:  newStreamHub(cfg.StreamQueueSize),
		resSubs:  newResourceSubscriptions(),
		metrics:  newServerMetrics(),
	}
	if len(pull) > 0 && pull[0] != nil {
		srv.pull = pull[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.handleHealthz)
	mux.HandleFunc("/readyz", srv.handleReadyz)
	mux.HandleFunc("/debug/statez", srv.handleStatez)
	mux.HandleFunc("/file/", srv.handleFileDownload)
	mux.HandleFunc("/mcp", srv.handleMCP)
	return withCORS(cfg, mux)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "name": s.cfg.ServerName, "version": s.cfg.ServerVersion})
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if strings.TrimSpace(s.cfg.BearerToken) == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not-ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "name": s.cfg.ServerName, "version": s.cfg.ServerVersion})
}

func (s *Server) handleStatez(w http.ResponseWriter, r *http.Request) {
	// Method check first: reject non-GET before any auth to avoid leaking
	// endpoint existence to unauthenticated callers.
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !originAllowed(origin, s.cfg.AllowedOrigins) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "origin not allowed"})
		return
	}
	if err := authorizeRequest(r, s.cfg.BearerToken); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.stateSnapshot())
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	debugFields := s.debugRequestFields(r)
	debugFields["handler"] = "mcp"
	recorder := s.wrapDebugResponseWriter(w)
	if s.debugEnabled() {
		w = recorder
		s.debugLog("http_request_received", debugFields)
		defer s.debugLogCompleted(time.Now(), recorder, debugFields)
	}
	if origin := r.Header.Get("Origin"); origin != "" && !originAllowed(origin, s.cfg.AllowedOrigins) {
		rejected := copyDebugFields(debugFields)
		rejected["http_status"] = http.StatusForbidden
		rejected["error"] = "origin not allowed"
		s.debugLog("http_request_rejected", rejected)
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "origin not allowed"})
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := authorizeRequest(r, s.cfg.BearerToken); err != nil {
		s.debugLogRejected(r, nil, http.StatusUnauthorized, -32001, err.Error(), nil)
		writeRPCError(w, nil, http.StatusUnauthorized, -32001, err.Error(), nil)
		return
	}

	switch r.Method {
	case http.MethodGet:
		if !acceptsSSE(r) {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET requires Accept: text/event-stream"})
			return
		}
		if strings.TrimSpace(r.Header.Get(sessionHeader)) == "" {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET stream requires an initialized session"})
			return
		}
		s.handleSSEStream(w, r)
		return
	case http.MethodDelete:
		s.handleSessionDelete(w, r)
		return
	case http.MethodPost:
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes)
	req, err := decodeRequest(r.Body)
	if err != nil {
		writeRPCError(w, nil, http.StatusBadRequest, -32700, err.Error(), nil)
		return
	}
	if !validRPCID(req.ID) {
		writeRPCError(w, nil, http.StatusBadRequest, -32600, "invalid id", nil)
		return
	}
	if req.requestInvalid {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32600, "request must be an object", nil)
		return
	}
	if req.jsonrpcInvalid {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32600, "jsonrpc must be a string", nil)
		return
	}
	if req.JSONRPC != "2.0" {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32600, "unsupported jsonrpc version", nil)
		return
	}
	if req.methodInvalid {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32600, "method must be a string", nil)
		return
	}
	if strings.TrimSpace(req.Method) == "" {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32600, "method required", nil)
		return
	}
	if !req.idPresent && !strings.HasPrefix(req.Method, "notifications/") {
		writeRPCError(w, nil, http.StatusBadRequest, -32600, "id required", nil)
		return
	}
	if req.paramsInvalid {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32602, "params must be an object", nil)
		return
	}
	if err := s.validateProtocolRequest(w, r, req); err != nil {
		return
	}

	if prefersSingleShotSSE(r) {
		s.handleMCPSSE(w, r, req)
		return
	}
	s.handleMCPJSON(w, r, req)
}

func (s *Server) handleMCPJSON(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	// All notifications (including notifications/initialized) return 202
	// Accepted with no JSON-RPC body per the MCP spec.
	if strings.HasPrefix(req.Method, "notifications/") {
		writeAccepted(w)
		return
	}
	switch req.Method {
	case "initialize":
		s.handleInitialize(w, r, req)
	case "tools/list":
		s.handleToolsList(w, r, req)
	case "resources/list":
		s.handleResourcesList(w, r, req)
	case "resources/templates/list":
		s.handleResourceTemplatesList(w, r, req)
	case "resources/read":
		s.handleResourcesRead(w, r, req)
	case "resources/subscribe":
		s.handleResourcesSubscribe(w, r, req)
	case "resources/unsubscribe":
		s.handleResourcesUnsubscribe(w, r, req)
	case "prompts/list":
		s.handlePromptsList(w, r, req)
	case "prompts/get":
		s.handlePromptGet(w, r, req)
	case "completion/complete":
		s.handleCompletion(w, r, req)
	case "tools/call":
		s.handleToolsCall(w, r, req)
	case "ping":
		writeRPC(w, req.ID, map[string]any{})
	default:
		writeRPCError(w, req.ID, http.StatusBadRequest, -32601, fmt.Sprintf("unknown method %q", req.Method), nil)
	}
}

func (s *Server) handleInitialize(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	rejectInvalidInitializeParams := func(message string) {
		s.debugLogRejected(r, &req, http.StatusBadRequest, -32602, message, nil)
		writeRPCError(w, req.ID, http.StatusBadRequest, -32602, message, nil)
	}
	requested, err := initializeProtocolVersion(req.Params)
	if err != nil {
		rejectInvalidInitializeParams(err.Error())
		return
	}
	protocol, ok := chooseProtocol(requested, s.cfg.SupportedProtocols)
	if !ok {
		s.debugLogRejected(r, &req, http.StatusBadRequest, -32002, "unsupported protocol version", map[string]any{"supported_protocols": s.cfg.SupportedProtocols})
		writeRPCError(w, req.ID, http.StatusBadRequest, -32002, "unsupported protocol version", map[string]any{"supported": s.cfg.SupportedProtocols})
		return
	}
	clientInfo, err := optionalObjectParamWithName(req.Params, "clientInfo", "clientInfo")
	if err != nil {
		rejectInvalidInitializeParams(err.Error())
		return
	}

	sess, err := s.sessions.Create(protocol, clientInfo)
	if err != nil {
		writeRPCError(w, req.ID, http.StatusInternalServerError, -32603, "create session failed", nil)
		return
	}
	w.Header().Set(sessionHeader, sess.ID)
	w.Header().Set(protocolHeader, protocol)
	writeRPC(w, req.ID, s.initializeResult(protocol))
}

func initializeProtocolVersion(params map[string]any) (string, error) {
	rawProtocolVersion, ok := params["protocolVersion"]
	if !ok || rawProtocolVersion == nil {
		return "", errors.New("protocolVersion required")
	}
	requested, ok := rawProtocolVersion.(string)
	if !ok {
		return "", errors.New("protocolVersion must be a string")
	}
	if strings.TrimSpace(requested) == "" {
		return "", errors.New("protocolVersion required")
	}
	return requested, nil
}

func (s *Server) handleToolsList(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	if _, ok := s.requireSession(w, r, req.ID); !ok {
		return
	}
	writeRPC(w, req.ID, map[string]any{"tools": s.registry.List()})
}

func (s *Server) handleResourcesList(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	if _, ok := s.requireSession(w, r, req.ID); !ok {
		return
	}
	writeRPC(w, req.ID, map[string]any{"resources": s.listResources()})
}

func (s *Server) handleResourceTemplatesList(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	if _, ok := s.requireSession(w, r, req.ID); !ok {
		return
	}
	writeRPC(w, req.ID, map[string]any{"resourceTemplates": s.listResourceTemplates()})
}

func (s *Server) handleResourcesRead(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	if _, ok := s.requireSession(w, r, req.ID); !ok {
		return
	}
	uri, err := resourceURIParam(req.Params)
	if err != nil {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32602, err.Error(), nil)
		return
	}
	result, err := s.readResource(uri)
	if err != nil {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32602, err.Error(), nil)
		return
	}
	writeRPC(w, req.ID, result)
}

func (s *Server) handlePromptsList(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	if _, ok := s.requireSession(w, r, req.ID); !ok {
		return
	}
	writeRPC(w, req.ID, map[string]any{"prompts": s.listPrompts()})
}

func (s *Server) handleResourcesSubscribe(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	if _, ok := s.requireSession(w, r, req.ID); !ok {
		return
	}
	uri, err := resourceURIParam(req.Params)
	if err != nil {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32602, err.Error(), nil)
		return
	}
	if err := s.subscribeResource(r.Header.Get(sessionHeader), uri); err != nil {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32602, err.Error(), nil)
		return
	}
	writeRPC(w, req.ID, map[string]any{})
}

func (s *Server) handleResourcesUnsubscribe(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	if _, ok := s.requireSession(w, r, req.ID); !ok {
		return
	}
	uri, err := resourceURIParam(req.Params)
	if err != nil {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32602, err.Error(), nil)
		return
	}
	if err := s.unsubscribeResource(r.Header.Get(sessionHeader), uri); err != nil {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32602, err.Error(), nil)
		return
	}
	writeRPC(w, req.ID, map[string]any{})
}

func (s *Server) handlePromptGet(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	if _, ok := s.requireSession(w, r, req.ID); !ok {
		return
	}
	result, rpcErr := s.promptGetResult(req)
	if rpcErr != nil {
		writeRPCError(w, req.ID, http.StatusBadRequest, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		return
	}
	writeRPC(w, req.ID, result)
}

func (s *Server) handleToolsCall(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	result, ok := s.callTool(r, req, w)
	if !ok {
		return
	}
	writeRPC(w, req.ID, result)
}

func (s *Server) handleCompletion(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	if _, ok := s.requireSession(w, r, req.ID); !ok {
		return
	}
	result, err := s.complete(req.Params)
	if err != nil {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32602, err.Error(), nil)
		return
	}
	writeRPC(w, req.ID, result)
}

func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get(sessionHeader)
	if strings.TrimSpace(id) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": sessionHeader + " header required"})
		return
	}
	sess, ok := s.lookupSession(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "invalid or expired session"})
		return
	}
	if err := validateRequestedProtocolAgainstSession(r, sess); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "protocol version does not match session", "expected": sess.ProtocolVersion})
		return
	}
	w.Header().Set(protocolHeader, sess.ProtocolVersion)
	s.streams.closeSession(id)
	s.resSubs.clearSession(id)
	s.sessions.Delete(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) requireSession(w http.ResponseWriter, r *http.Request, id any) (session.Session, bool) {
	sessionID := r.Header.Get(sessionHeader)
	if strings.TrimSpace(sessionID) == "" {
		writeRPCError(w, id, http.StatusBadRequest, -32003, "missing session header", nil)
		return session.Session{}, false
	}
	sess, ok := s.lookupSession(sessionID)
	if !ok {
		writeRPCError(w, id, http.StatusNotFound, -32004, "invalid or expired session", nil)
		return session.Session{}, false
	}
	if err := validateRequestedProtocolAgainstSession(r, sess); err != nil {
		writeRPCError(w, id, http.StatusBadRequest, -32002, "protocol version does not match session", map[string]any{"expected": sess.ProtocolVersion})
		return session.Session{}, false
	}
	w.Header().Set(protocolHeader, sess.ProtocolVersion)
	return sess, true
}

func (s *Server) lookupSession(sessionID string) (session.Session, bool) {
	sess, ok := s.sessions.Get(sessionID)
	if !ok {
		s.streams.closeSession(sessionID)
		s.resSubs.clearSession(sessionID)
	}
	return sess, ok
}

func (s *Server) callTool(r *http.Request, req rpcRequest, w http.ResponseWriter) (mcp.Result, bool) {
	sess, ok := s.requireSessionForMode(w, r, req.ID, false)
	if !ok {
		return mcp.Result{}, false
	}
	result, rpcErr := s.callToolWithSession(r, req, sess)
	if rpcErr != nil {
		writeRPCError(w, req.ID, http.StatusBadRequest, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		return mcp.Result{}, false
	}
	return result, true
}

func (s *Server) callToolWithSession(r *http.Request, req rpcRequest, sess session.Session) (mcp.Result, *rpcError) {
	toolName, err := requiredStringParam(req.Params, "name", "tool name")
	if err != nil {
		return mcp.Result{}, &rpcError{Code: -32602, Message: err.Error()}
	}
	args, err := optionalObjectParam(req.Params, "arguments")
	if err != nil {
		return mcp.Result{}, &rpcError{Code: -32602, Message: err.Error()}
	}
	result, err := s.registry.Call(r.Context(), mcp.CallContext{
		RemoteAddr:      r.RemoteAddr,
		Host:            r.Host,
		RequestID:       requestID(r),
		SessionID:       sess.ID,
		ProtocolVersion: sess.ProtocolVersion,
	}, toolName, args)
	if err != nil {
		return mcp.Result{}, &rpcError{Code: -32010, Message: err.Error()}
	}
	s.trackTemplateCall(toolName, args, result)
	s.maybeNotifyResourceUpdated(toolName, args, result)
	return result, nil
}

func (s *Server) requireSessionForMode(w http.ResponseWriter, r *http.Request, id any, sse bool) (session.Session, bool) {
	sessionID := r.Header.Get(sessionHeader)
	if strings.TrimSpace(sessionID) == "" {
		if sse {
			return session.Session{}, false
		}
		writeRPCError(w, id, http.StatusBadRequest, -32003, "missing session header", nil)
		return session.Session{}, false
	}
	sess, ok := s.lookupSession(sessionID)
	if !ok {
		if sse {
			return session.Session{}, false
		}
		writeRPCError(w, id, http.StatusNotFound, -32004, "invalid or expired session", nil)
		return session.Session{}, false
	}
	if err := validateRequestedProtocolAgainstSession(r, sess); err != nil {
		if sse {
			return session.Session{}, false
		}
		writeRPCError(w, id, http.StatusBadRequest, -32002, "protocol version does not match session", map[string]any{"expected": sess.ProtocolVersion})
		return session.Session{}, false
	}
	w.Header().Set(protocolHeader, sess.ProtocolVersion)
	return sess, true
}

func (s *Server) initializeResult(protocol string) map[string]any {
	return map[string]any{
		"protocolVersion": protocol,
		"serverInfo": map[string]any{
			"name":    s.cfg.ServerName,
			"version": s.cfg.ServerVersion,
		},
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
	}
}

func (s *Server) listResources() []map[string]any {
	resources := make([]map[string]any, 0, len(s.cfg.AllowedRoots)+1)
	for _, root := range s.cfg.AllowedRoots {
		resources = append(resources, map[string]any{
			"uri":         localFileURI(root),
			"name":        root,
			"description": "Allowed workspace root for MCP file and command operations.",
			"mimeType":    "inode/directory",
		})
	}
	resources = append(resources, map[string]any{
		"uri":         localFileURI(s.cfg.AuditLogPath),
		"name":        "audit-log",
		"description": "JSONL audit log generated by the MCP server.",
		"mimeType":    "application/jsonl",
	})
	return resources
}

func (s *Server) listResourceTemplates() []map[string]any {
	templates := make([]map[string]any, 0, len(s.cfg.AllowedRoots))
	for _, root := range s.cfg.AllowedRoots {
		name := filepath.Base(root)
		if name == "." || name == string(filepath.Separator) || name == "" {
			name = "root"
		}
		rootURI := localFileURI(root)
		uriTemplate := rootURI + "/{path}"
		if strings.HasSuffix(rootURI, "/") {
			uriTemplate = rootURI + "{path}"
		}
		templates = append(templates, map[string]any{
			"uriTemplate": uriTemplate,
			"name":        name + "-path",
			"description": "Read any file or directory under this allowed root by replacing {path} with a relative path.",
			"mimeType":    "application/octet-stream",
			"_meta": map[string]any{
				"templateArguments": []map[string]any{
					{
						"name":        "path",
						"description": "Relative path under the allowed root.",
						"required":    true,
						"type":        "path",
						"examples":    []string{"README.md", "internal/httpapi/server.go"},
					},
				},
				"root": root,
			},
		})
	}
	return templates
}

func (s *Server) readResource(uri string) (map[string]any, error) {
	path, err := parseFileResourcePath(uri)
	if err != nil {
		return nil, err
	}
	resolved, err := s.resolveResourcePath(path)
	if err != nil {
		return nil, err
	}
	canonicalURI := localFileURI(resolved)
	if filepath.Clean(resolved) == filepath.Clean(s.cfg.AuditLogPath) {
		payload, err := os.ReadFile(resolved)
		if err != nil {
			return nil, fmt.Errorf("read audit log: %w", err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("stat audit log: %w", err)
		}
		return map[string]any{
			"contents": []map[string]any{{
				"uri":      canonicalURI,
				"mimeType": "application/jsonl",
				"text":     string(payload),
				"_meta":    resourceMetadata(resolved, info, false, 0),
			}},
		}, nil
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("stat resource: %w", err)
	}
	if info.IsDir() {
		entries, err := os.ReadDir(resolved)
		if err != nil {
			return nil, fmt.Errorf("read resource dir: %w", err)
		}
		names := make([]string, 0, len(entries))
		lines := make([]string, 0, len(entries)+1)
		lines = append(lines, resolved)
		for _, entry := range entries {
			kind := "file"
			if entry.IsDir() {
				kind = "dir"
			}
			name := entry.Name()
			names = append(names, name)
			lines = append(lines, fmt.Sprintf("- [%s] %s", kind, name))
		}
		sort.Strings(names)
		return map[string]any{
			"contents": []map[string]any{{
				"uri":      canonicalURI,
				"mimeType": "text/plain",
				"text":     strings.Join(lines, "\n"),
				"_meta":    resourceMetadata(resolved, info, true, len(entries), names...),
			}},
		}, nil
	}
	payload, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read resource file: %w", err)
	}
	mimeType := detectResourceMimeType(resolved)
	return map[string]any{
		"contents": []map[string]any{{
			"uri":      canonicalURI,
			"mimeType": mimeType,
			"text":     string(payload),
			"_meta":    resourceMetadata(resolved, info, false, 0),
		}},
	}, nil
}

func parseFileResourcePath(uri string) (string, error) {
	if strings.TrimSpace(uri) == "" {
		return "", errors.New("resource uri required")
	}
	if !strings.HasPrefix(uri, "file://") {
		return "", fmt.Errorf("unsupported resource uri %q", uri)
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("invalid resource uri %q", uri)
	}
	if parsed.Scheme != "file" {
		return "", fmt.Errorf("unsupported resource uri %q", uri)
	}
	if parsed.Opaque != "" {
		return "", fmt.Errorf("invalid file resource uri %q", uri)
	}
	if parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost") {
		return "", fmt.Errorf("unsupported file resource host %q", parsed.Host)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return "", fmt.Errorf("file resource uri must not include query")
	}
	if parsed.Fragment != "" {
		return "", fmt.Errorf("file resource uri must not include fragment")
	}
	if parsed.Path == "" {
		return "", errors.New("file resource path required")
	}
	return filepath.FromSlash(parsed.Path), nil
}

func resourceURIParam(params map[string]any) (string, error) {
	if params == nil {
		return "", errors.New("resource uri required")
	}
	value, ok := params["uri"]
	if !ok || value == nil {
		return "", errors.New("resource uri required")
	}
	uri, ok := value.(string)
	if !ok {
		return "", errors.New("resource uri must be a string")
	}
	if strings.TrimSpace(uri) == "" {
		return "", errors.New("resource uri required")
	}
	return uri, nil
}

func detectResourceMimeType(path string) string {
	if filepath.Ext(path) == ".jsonl" {
		return "application/jsonl"
	}
	if guessed := mime.TypeByExtension(filepath.Ext(path)); guessed != "" {
		return guessed
	}
	return "text/plain"
}

func localFileURI(path string) string {
	uriPath := filepath.ToSlash(filepath.Clean(path))
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	return (&url.URL{Scheme: "file", Path: uriPath}).String()
}

func resourceMetadata(path string, info os.FileInfo, isDir bool, childCount int, entries ...string) map[string]any {
	metadata := map[string]any{
		"path":        path,
		"name":        info.Name(),
		"is_dir":      isDir,
		"size":        info.Size(),
		"mod_time":    info.ModTime().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		"mode":        info.Mode().String(),
		"child_count": childCount,
	}
	if len(entries) > 0 {
		metadata["entries"] = entries
	}
	return metadata
}

func (s *Server) complete(params map[string]any) (map[string]any, error) {
	ref, err := requiredObjectParam(params, "ref", "ref")
	if err != nil {
		return nil, err
	}
	refType, err := completionStringArg(ref, "type", "ref.type")
	if err != nil {
		return nil, err
	}
	argument, err := requiredObjectParam(params, "argument", "argument")
	if err != nil {
		return nil, err
	}
	argName, err := completionStringArg(argument, "name", "argument.name")
	if err != nil {
		return nil, err
	}
	argValue, err := completionStringArg(argument, "value", "argument.value")
	if err != nil {
		return nil, err
	}
	context, err := optionalObjectParamWithName(params, "context", "context")
	if err != nil {
		return nil, err
	}
	contextArgs, err := optionalObjectParamWithName(context, "arguments", "context.arguments")
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(refType) == "" || strings.TrimSpace(argName) == "" {
		return nil, errors.New("completion requires ref.type and argument.name")
	}

	var values []string
	switch refType {
	case "ref/prompt":
		name, err := completionStringArg(ref, "name", "ref.name")
		if err != nil {
			return nil, err
		}
		completed, err := s.completePrompt(name, argName, argValue, contextArgs)
		if err != nil {
			return nil, err
		}
		values = completed
	case "ref/resource":
		uri, err := completionStringArg(ref, "uri", "ref.uri")
		if err != nil {
			return nil, err
		}
		completed, err := s.completeResource(uri, argName, argValue)
		if err != nil {
			return nil, err
		}
		values = completed
	default:
		return nil, fmt.Errorf("unsupported completion ref type %q", refType)
	}

	if len(values) > 100 {
		values = values[:100]
	}
	return map[string]any{
		"completion": map[string]any{
			"values":  values,
			"total":   len(values),
			"hasMore": false,
		},
	}, nil
}

func (s *Server) completePrompt(name, argName, argValue string, contextArgs map[string]any) ([]string, error) {
	switch name {
	case "safe_file_edit":
		if argName == "path" {
			return s.completeAcrossRoots(argValue, false), nil
		}
		return nil, nil
	case "go_dev_loop":
		switch argName {
		case "workdir":
			return s.completeAcrossRoots(argValue, true), nil
		case "test_target":
			workdir, _, err := completionOptionalStringArg(contextArgs, "workdir", "context.arguments.workdir")
			if err != nil {
				return nil, err
			}
			return s.completeGoTestTarget(workdir, argValue), nil
		default:
			return nil, nil
		}
	default:
		return nil, fmt.Errorf("unknown prompt %q", name)
	}
}

func (s *Server) completeResource(uriTemplate, argName, argValue string) ([]string, error) {
	if argName != "path" {
		return nil, nil
	}
	root := ""
	for _, tpl := range s.listResourceTemplates() {
		if tpl["uriTemplate"] == uriTemplate {
			meta := normalizeMap(tpl["_meta"])
			root, _ = meta["root"].(string)
			break
		}
	}
	if root == "" {
		return nil, fmt.Errorf("unknown resource template %q", uriTemplate)
	}
	return completeRelativePath(root, argValue, false), nil
}

func (s *Server) completeAcrossRoots(prefix string, dirsOnly bool) []string {
	merged := make([]string, 0)
	seen := map[string]bool{}
	for _, root := range s.cfg.AllowedRoots {
		for _, value := range completeRelativePath(root, prefix, dirsOnly) {
			if !seen[value] {
				seen[value] = true
				merged = append(merged, value)
			}
		}
	}
	sort.Strings(merged)
	return merged
}

func (s *Server) completeGoTestTarget(workdir string, prefix string) []string {
	suggestions := []string{"./..."}
	for _, root := range s.cfg.AllowedRoots {
		base := root
		if strings.TrimSpace(workdir) != "" && workdir != "." {
			var (
				candidate string
				err       error
			)
			workdirPath := resolveAgainstStartup(workdir, s.cfg.StartupDirectory)
			if s.cfg.UnsafeAllowAll {
				candidate, err = security.ResolvePathUnsafe(workdirPath)
			} else {
				candidate, err = security.ResolvePath(workdirPath, s.cfg.AllowedRoots)
			}
			if err == nil {
				base = candidate
			}
		}
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				suggestions = append(suggestions, "./"+entry.Name()+"/...")
			}
		}
	}
	return filterByPrefix(dedupeLocalStrings(suggestions), prefix)
}

func completeRelativePath(root, prefix string, dirsOnly bool) []string {
	searchDir := root
	basePrefix := ""
	if prefix != "" {
		basePrefix = completionBasePrefix(prefix)
		if basePrefix != "" {
			candidateDir := filepath.Join(root, basePrefix)
			resolvedDir, err := security.ResolvePath(candidateDir, []string{root})
			if err != nil {
				return nil
			}
			if info, err := os.Stat(resolvedDir); err == nil && info.IsDir() {
				searchDir = resolvedDir
			} else {
				return nil
			}
		}
	}
	entries, err := os.ReadDir(searchDir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if dirsOnly && !entry.IsDir() {
			continue
		}
		name := entry.Name()
		rel := name
		if basePrefix != "" {
			rel = filepath.ToSlash(filepath.Join(basePrefix, name))
		}
		if entry.IsDir() {
			rel += "/"
		}
		out = append(out, rel)
	}
	sort.Strings(out)
	return filterByPrefix(out, prefix)
}

func completionBasePrefix(prefix string) string {
	if !strings.Contains(prefix, "/") {
		return ""
	}
	cleanPrefix := filepath.Clean(prefix)
	if cleanPrefix == "." {
		return ""
	}
	if strings.HasSuffix(prefix, "/") {
		return cleanPrefix
	}
	basePrefix := filepath.Dir(cleanPrefix)
	if basePrefix == "." {
		return ""
	}
	return basePrefix
}

func filterByPrefix(values []string, prefix string) []string {
	if prefix == "" {
		return values
	}
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func dedupeLocalStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (s *Server) listPrompts() []map[string]any {
	return []map[string]any{
		{
			"name":        "safe_file_edit",
			"title":       "Safe File Edit",
			"description": "Plan a scoped file edit using fs.read_file plus fs.apply_unified_diff for complex edits or fs.edit_lines for small changes.",
			"arguments": []map[string]any{
				promptArgument("task", "The edit objective.", true),
				promptArgument("path", "Target file path inside allowed roots.", true),
				promptArgument("expected_old_text", "Optional old text for optimistic concurrency during fs.apply_unified_diff or fs.edit_lines.", false),
			},
			"_meta": map[string]any{
				"inputSchema": map[string]any{
					"type":     "object",
					"required": []string{"task", "path"},
					"properties": map[string]any{
						"task": map[string]any{
							"type":        "string",
							"minLength":   1,
							"description": "The edit objective.",
							"examples":    []string{"update readme heading"},
						},
						"path": map[string]any{
							"type":        "string",
							"minLength":   1,
							"description": "Target file path inside allowed roots.",
							"examples":    []string{"README.md"},
						},
						"expected_old_text": map[string]any{
							"type":        "string",
							"description": "Optional old text for optimistic concurrency during fs.apply_unified_diff or fs.edit_lines.",
							"examples":    []string{"# old heading"},
						},
					},
				},
			},
		},
		{
			"name":        "go_dev_loop",
			"title":       "Go Dev Loop",
			"description": "Iterate on a Go code change using go.list_symbols/go.find_definition, fs editing tools, and exec.run gofmt/go test/go vet.",
			"arguments": []map[string]any{
				promptArgument("goal", "Requested Go change or bug fix.", true),
				promptArgument("workdir", "Working directory for Go commands.", true),
				promptArgument("test_target", "Optional go test target. Defaults to ./....", false),
				promptArgument("run_vet", "Whether to include go_vet in the suggested loop. Defaults to true.", false),
			},
			"_meta": map[string]any{
				"inputSchema": map[string]any{
					"type":     "object",
					"required": []string{"goal", "workdir"},
					"properties": map[string]any{
						"goal": map[string]any{
							"type":        "string",
							"minLength":   1,
							"description": "Requested Go change or bug fix.",
							"examples":    []string{"fix failing tests"},
						},
						"workdir": map[string]any{
							"type":        "string",
							"minLength":   1,
							"description": "Working directory for Go commands.",
							"examples":    []string{"."},
						},
						"test_target": map[string]any{
							"type":        "string",
							"description": "Optional go test target. Defaults to ./....",
							"default":     "./...",
							"examples":    []string{"./internal/httpapi"},
						},
						"run_vet": map[string]any{
							"type":        "boolean",
							"description": "Whether to include go_vet in the suggested loop. Defaults to true.",
							"default":     true,
						},
					},
				},
			},
		},
	}
}

func (s *Server) getPrompt(name string, args map[string]any) (map[string]any, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("prompt name required")
	}
	switch name {
	case "safe_file_edit":
		task, taskSet, err := promptRequiredStringArg(args, "task")
		if err != nil {
			return nil, err
		}
		path, pathSet, err := promptRequiredStringArg(args, "path")
		if err != nil {
			return nil, err
		}
		expectedOldText, expectedOldTextSet, err := promptOptionalStringArg(args, "expected_old_text")
		if err != nil {
			return nil, err
		}
		if !taskSet || !pathSet {
			return nil, errors.New("safe_file_edit requires task and path")
		}
		text := fmt.Sprintf("Edit %s for task %q. First inspect with fs.read_file, then use fs.apply_unified_diff for complex or multi-hunk edits (or fs.edit_lines for small scoped changes), then verify with fs.read_file or exec.run if relevant. Keep the target inside allowed roots.", path, task)
		if expectedOldTextSet && strings.TrimSpace(expectedOldText) != "" {
			text += fmt.Sprintf(" Use expected_old_text=%q when calling fs.apply_unified_diff or fs.edit_lines if the old text must match exactly.", expectedOldText)
		}
		return map[string]any{
			"description": "Guide an agent through a safe scoped file edit.",
			"messages": []map[string]any{{
				"role": "user",
				"content": map[string]any{
					"type": "text",
					"text": text,
				},
			}},
		}, nil
	case "go_dev_loop":
		goal, goalSet, err := promptRequiredStringArg(args, "goal")
		if err != nil {
			return nil, err
		}
		workdir, workdirSet, err := promptRequiredStringArg(args, "workdir")
		if err != nil {
			return nil, err
		}
		testTarget, testTargetSet, err := promptOptionalStringArg(args, "test_target")
		if err != nil {
			return nil, err
		}
		runVet, _, err := promptOptionalBoolArg(args, "run_vet", true)
		if err != nil {
			return nil, err
		}
		if !goalSet || !workdirSet {
			return nil, errors.New("go_dev_loop requires goal and workdir")
		}
		if !testTargetSet || strings.TrimSpace(testTarget) == "" {
			testTarget = "./..."
		}
		text := fmt.Sprintf("Work in %s to achieve %q. Start with go.list_symbols and go.find_definition when that helps you understand the Go code, then inspect relevant files, use fs.apply_unified_diff for complex edits (or fs.edit_lines / fs.write_file when simpler), then run exec.run with go_fmt and go_test (target %s) as needed.", workdir, goal, testTarget)
		if runVet {
			text += " Include go_vet before concluding."
		}
		return map[string]any{
			"description": "Guide an agent through a Go-focused edit/test loop.",
			"messages": []map[string]any{{
				"role": "user",
				"content": map[string]any{
					"type": "text",
					"text": text,
				},
			}},
		}, nil
	default:
		return nil, fmt.Errorf("unknown prompt %q", name)
	}
}

func (s *Server) promptGetResult(req rpcRequest) (map[string]any, *rpcError) {
	name, err := requiredStringParam(req.Params, "name", "prompt name")
	if err != nil {
		return nil, &rpcError{Code: -32602, Message: err.Error()}
	}
	args, err := optionalObjectParam(req.Params, "arguments")
	if err != nil {
		return nil, &rpcError{Code: -32602, Message: err.Error()}
	}
	result, err := s.getPrompt(name, args)
	if err != nil {
		return nil, &rpcError{Code: -32602, Message: err.Error()}
	}
	return result, nil
}

func promptArgument(name, description string, required bool) map[string]any {
	return map[string]any{
		"name":        name,
		"description": description,
		"required":    required,
	}
}

func promptRequiredStringArg(args map[string]any, key string) (string, bool, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", true, fmt.Errorf("%s must be a string", key)
	}
	if strings.TrimSpace(value) == "" {
		return "", false, nil
	}
	return value, true, nil
}

func promptOptionalStringArg(args map[string]any, key string) (string, bool, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", true, fmt.Errorf("%s must be a string", key)
	}
	return value, true, nil
}

func promptOptionalBoolArg(args map[string]any, key string, defaultValue bool) (bool, bool, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return defaultValue, false, nil
	}
	value, ok := raw.(bool)
	if !ok {
		return false, true, fmt.Errorf("%s must be a boolean", key)
	}
	return value, true, nil
}

func completionStringArg(args map[string]any, key string, field string) (string, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return "", fmt.Errorf("completion requires %s", field)
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", field)
	}
	return value, nil
}

func completionOptionalStringArg(args map[string]any, key string, field string) (string, bool, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", true, fmt.Errorf("%s must be a string", field)
	}
	return value, true, nil
}

func (s *Server) validateProtocolRequest(w http.ResponseWriter, r *http.Request, req rpcRequest) error {
	headerVersion := strings.TrimSpace(r.Header.Get(protocolHeader))
	if req.Method == "initialize" {
		if headerVersion != "" {
			if _, ok := chooseProtocol(headerVersion, s.cfg.SupportedProtocols); !ok {
				s.debugLogRejected(r, &req, http.StatusBadRequest, -32002, "unsupported protocol version", map[string]any{"supported_protocols": s.cfg.SupportedProtocols})
				writeRPCError(w, req.ID, http.StatusBadRequest, -32002, "unsupported protocol version", map[string]any{"supported": s.cfg.SupportedProtocols})
				return errors.New("unsupported protocol version")
			}
		}
		return nil
	}
	sessionID := strings.TrimSpace(r.Header.Get(sessionHeader))
	if sessionID == "" {
		return nil
	}
	sess, ok := s.lookupSession(sessionID)
	if !ok {
		return nil
	}
	if err := validateRequestedProtocolAgainstSession(r, sess); err != nil {
		s.debugLogRejected(r, &req, http.StatusBadRequest, -32002, "protocol version does not match session", map[string]any{"expected_protocol": sess.ProtocolVersion})
		writeRPCError(w, req.ID, http.StatusBadRequest, -32002, "protocol version does not match session", map[string]any{"expected": sess.ProtocolVersion})
		return errors.New("protocol version mismatch")
	}
	w.Header().Set(protocolHeader, sess.ProtocolVersion)
	return nil
}

func validateRequestedProtocolAgainstSession(r *http.Request, sess session.Session) error {
	headerVersion := strings.TrimSpace(r.Header.Get(protocolHeader))
	if headerVersion == "" {
		return nil
	}
	if headerVersion != sess.ProtocolVersion {
		return fmt.Errorf("protocol version mismatch")
	}
	return nil
}

func (s *Server) publishResourceUpdate(uri string) {
	for _, sessionID := range s.resSubs.sessionsFor(uri) {
		s.metrics.resourceNotificationAttempts.Add(1)
		if s.streams.publishNotification(sessionID, "notifications/resources/updated", map[string]any{"uri": uri}) {
			s.metrics.resourceNotificationSent.Add(1)
			continue
		}
		s.metrics.resourceNotificationDropped.Add(1)
	}
}

func (s *Server) maybeNotifyResourceUpdated(toolName string, args map[string]any, result mcp.Result) {
	if result.IsError {
		return
	}
	var uris []string
	switch toolName {
	case "fs.write_file", "fs.replace_text", "fs.apply_unified_diff", "fs.edit_lines", "fs.make_dir":
		if path, _ := args["path"].(string); strings.TrimSpace(path) != "" {
			if resolved, err := s.resolvePathForResource(resolveAgainstStartup(path, s.cfg.StartupDirectory)); err == nil {
				uris = append(uris, resourceUpdateTargets(resolved, s.cfg.AllowedRoots, s.cfg.UnsafeAllowAll)...)
			}
		}
	case "fs.delete_path":
		if path, _ := args["path"].(string); strings.TrimSpace(path) != "" {
			if resolved, err := s.resolvePathNoFollowFinalForResource(resolveAgainstStartup(path, s.cfg.StartupDirectory)); err == nil {
				uris = append(uris, resourceUpdateTargets(resolved, s.cfg.AllowedRoots, s.cfg.UnsafeAllowAll)...)
			}
		}
	case "fs.move_path":
		for _, key := range []string{"src", "dst"} {
			if path, _ := args[key].(string); strings.TrimSpace(path) != "" {
				if resolved, err := s.resolvePathNoFollowFinalForResource(resolveAgainstStartup(path, s.cfg.StartupDirectory)); err == nil {
					uris = append(uris, resourceUpdateTargets(resolved, s.cfg.AllowedRoots, s.cfg.UnsafeAllowAll)...)
				}
			}
		}
	}
	for _, uri := range dedupeLocalStrings(uris) {
		s.publishResourceUpdate(uri)
	}
}

func resolveAgainstStartup(path string, startup string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(startup, path)
}

func (s *Server) stateSnapshot() map[string]any {
	s.pruneExpiredSessions()
	return map[string]any{
		"server": map[string]any{
			"name":                s.cfg.ServerName,
			"version":             s.cfg.ServerVersion,
			"supported_protocols": s.cfg.SupportedProtocols,
		},
		"config": map[string]any{
			"listen_addr":              s.cfg.ListenAddr,
			"log_level":                s.cfg.LogLevel,
			"unsafe_allow_all":         s.cfg.UnsafeAllowAll,
			"allowed_roots":            s.cfg.AllowedRoots,
			"allowed_origins":          s.cfg.AllowedOrigins,
			"audit_log_path":           s.cfg.AuditLogPath,
			"audit_rotate_max_mb":      s.cfg.AuditRotateMaxMB,
			"audit_rotate_max_backups": s.cfg.AuditRotateMaxBackups,
			"output_max_bytes":         s.cfg.OutputMaxBytes,
			"stream_queue_size":        s.cfg.StreamQueueSize,
			"max_request_bytes":        s.cfg.MaxRequestBytes,
			"command_timeout_ms":       s.cfg.CommandTimeout.Milliseconds(),
			"session_ttl_ms":           s.cfg.SessionTTL.Milliseconds(),
			"exec_presets":             summarizeExecPresets(s.cfg.ExecPresets),
			"command_templates":        summarizeCommandTemplates(s.cfg.CommandTemplates),
		},
		"runtime": map[string]any{
			"active_sessions":                s.sessions.Count(),
			"active_stream_sessions":         s.streams.activeSessionCount(),
			"active_stream_connections":      s.streams.activeConnectionCount(),
			"resource_subscription_sessions": s.resSubs.sessionCount(),
			"resource_subscription_total":    s.resSubs.totalCount(),
		},
		"counters":         s.metrics.snapshot(),
		"template_metrics": s.metrics.templateSnapshot(),
	}
}

func (s *Server) pruneExpiredSessions() {
	for _, sessionID := range s.sessions.PruneExpired() {
		s.streams.closeSession(sessionID)
		s.resSubs.clearSession(sessionID)
	}
}

func summarizeExecPresets(presets map[string]config.ExecPreset) map[string]any {
	out := make(map[string]any, len(presets))
	for name, preset := range presets {
		entry := map[string]any{
			"command":      preset.Command,
			"fixed_args":   util.CloneStrings(preset.FixedArgs),
			"allowed_args": util.CloneStrings(preset.AllowedArgs),
			"read_only":    preset.ReadOnly,
			"timeout_ms":   preset.Timeout.Milliseconds(),
		}
		if len(preset.Env) > 0 {
			keys := make([]string, 0, len(preset.Env))
			for key := range preset.Env {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			entry["env_keys"] = keys
		}
		out[name] = entry
	}
	return out
}

func summarizeCommandTemplates(templates map[string]config.CommandTemplate) map[string]any {
	out := make(map[string]any, len(templates))
	for name, tpl := range templates {
		entry := map[string]any{
			"command":               util.CloneStrings(tpl.Command),
			"category":              tpl.Category,
			"destructive":           tpl.Destructive,
			"requires_confirmation": tpl.RequiresConfirmation,
			"read_only":             tpl.ReadOnly,
		}
		if len(tpl.Env) > 0 {
			keys := make([]string, 0, len(tpl.Env))
			for key := range tpl.Env {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			entry["env_keys"] = keys
		}
		if len(tpl.AllowedWorkdirs) > 0 {
			entry["allowed_workdirs"] = util.CloneStrings(tpl.AllowedWorkdirs)
		}
		out[name] = entry
	}
	return out
}

func (s *Server) trackTemplateCall(toolName string, args map[string]any, result mcp.Result) {
	if toolName != "exec.run_template" {
		return
	}
	templateName, _ := args["template"].(string)
	if strings.TrimSpace(templateName) == "" {
		templateName = "<unknown>"
	}
	confirmationBlocked := false
	if tpl, ok := s.cfg.CommandTemplates[templateName]; ok && tpl.RequiresConfirmation {
		if blocked, _ := result.StructuredContent["confirmation_required"].(bool); blocked && result.IsError {
			confirmationBlocked = true
		}
	}
	s.metrics.recordTemplateCall(templateName, !result.IsError, confirmationBlocked)
}

func resourceUpdateTargets(resolved string, allowedRoots []string, unsafeAllowAll bool) []string {
	targets := []string{localFileURI(resolved)}
	if unsafeAllowAll {
		current := filepath.Dir(resolved)
		for {
			if current == resolved {
				current = filepath.Dir(current)
			}
			if current == "." || current == string(filepath.Separator) {
				if current == string(filepath.Separator) {
					targets = append(targets, localFileURI(current))
				}
				break
			}
			targets = append(targets, localFileURI(current))
			next := filepath.Dir(current)
			if next == current {
				break
			}
			current = next
		}
		return dedupeLocalStrings(targets)
	}
	for _, root := range allowedRoots {
		canonicalRoot, err := security.ResolvePath(root, allowedRoots)
		if err != nil {
			canonicalRoot = filepath.Clean(root)
		}
		rel, err := filepath.Rel(canonicalRoot, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		current := filepath.Dir(resolved)
		for {
			if current == resolved {
				current = filepath.Dir(current)
			}
			if current == "." || current == string(filepath.Separator) {
				break
			}
			targets = append(targets, localFileURI(current))
			if filepath.Clean(current) == filepath.Clean(canonicalRoot) {
				break
			}
			next := filepath.Dir(current)
			if next == current {
				break
			}
			current = next
		}
	}
	return targets
}

func (s *Server) subscribeResource(sessionID, uri string) error {
	canonicalURI, err := s.canonicalResourceURI(uri)
	if err != nil {
		return err
	}
	if _, err := s.readResource(canonicalURI); err != nil {
		return err
	}
	s.resSubs.subscribe(sessionID, canonicalURI)
	return nil
}

func (s *Server) unsubscribeResource(sessionID, uri string) error {
	if strings.TrimSpace(uri) == "" {
		return errors.New("resource uri required")
	}
	canonicalURI, err := s.canonicalResourceURI(uri)
	if err != nil {
		return err
	}
	s.resSubs.unsubscribe(sessionID, canonicalURI)
	return nil
}

func (s *Server) canonicalResourceURI(uri string) (string, error) {
	path, err := parseFileResourcePath(uri)
	if err != nil {
		return "", err
	}
	resolved, err := s.resolveResourcePath(path)
	if err != nil {
		return "", err
	}
	return localFileURI(resolved), nil
}

func (s *Server) resolveResourcePath(path string) (string, error) {
	var (
		resolved string
		err      error
	)
	if s.cfg.UnsafeAllowAll {
		resolved, err = security.ResolvePathUnsafe(path)
	} else {
		resolved, err = security.ResolvePath(path, s.cfg.AllowedRoots)
	}
	if err != nil {
		if filepath.Clean(path) == filepath.Clean(s.cfg.AuditLogPath) {
			return filepath.Clean(path), nil
		}
		return "", fmt.Errorf("resolve resource: %w", err)
	}
	return resolved, nil
}

func (s *Server) resolvePathForResource(path string) (string, error) {
	if s.cfg.UnsafeAllowAll {
		return security.ResolvePathUnsafe(path)
	}
	return security.ResolvePath(path, s.cfg.AllowedRoots)
}

func (s *Server) resolvePathNoFollowFinalForResource(path string) (string, error) {
	if s.cfg.UnsafeAllowAll {
		return security.ResolvePathUnsafeNoFollowFinal(path)
	}
	return security.ResolvePathNoFollowFinal(path, s.cfg.AllowedRoots)
}

func authorizeRequest(r *http.Request, token string) error {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return errors.New("missing bearer token")
	}
	if strings.TrimPrefix(header, "Bearer ") != token {
		return errors.New("invalid bearer token")
	}
	return nil
}

func decodeRequest(body io.Reader) (rpcRequest, error) {
	decoder := json.NewDecoder(body)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return rpcRequest{}, fmt.Errorf("invalid json")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return rpcRequest{}, fmt.Errorf("invalid json")
	}
	req := rpcRequest{
		JSONRPC: "2.0",
		Params:  make(map[string]any),
	}
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		req.requestInvalid = true
		return req, nil
	}
	var wire struct {
		JSONRPC json.RawMessage `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Method  json.RawMessage `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return rpcRequest{}, fmt.Errorf("invalid json")
	}
	if len(wire.ID) > 0 {
		id, err := decodeRawJSONValue(wire.ID)
		if err != nil {
			return rpcRequest{}, fmt.Errorf("invalid json")
		}
		req.ID = id
		req.idPresent = true
	}
	if len(wire.JSONRPC) == 0 {
		req.JSONRPC = ""
	} else if strings.TrimSpace(string(wire.JSONRPC)) == "null" {
		req.jsonrpcInvalid = true
		return req, nil
	} else if err := json.Unmarshal(wire.JSONRPC, &req.JSONRPC); err != nil {
		req.jsonrpcInvalid = true
		return req, nil
	}
	if len(wire.Method) > 0 {
		if err := json.Unmarshal(wire.Method, &req.Method); err != nil {
			req.methodInvalid = true
			return req, nil
		}
	}
	if len(wire.Params) > 0 {
		trimmed := strings.TrimSpace(string(wire.Params))
		if trimmed != "" && trimmed != "null" {
			if !strings.HasPrefix(trimmed, "{") {
				req.paramsInvalid = true
				return req, nil
			}
			value, err := decodeRawJSONValue(wire.Params)
			if err != nil {
				return rpcRequest{}, fmt.Errorf("invalid json")
			}
			params, ok := value.(map[string]any)
			if !ok {
				req.paramsInvalid = true
				return req, nil
			}
			req.Params = params
		}
	}
	return req, nil
}

func decodeRawJSONValue(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, err
	}
	return value, nil
}

func validRPCID(id any) bool {
	switch id.(type) {
	case nil, string, float64, json.Number,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return true
	default:
		return false
	}
}

func chooseProtocol(requested string, supported []string) (string, bool) {
	if len(supported) == 0 {
		return "", false
	}
	if strings.TrimSpace(requested) == "" {
		return supported[0], true
	}
	for _, candidate := range supported {
		if candidate == requested {
			return candidate, true
		}
	}
	return "", false
}

func nestedString(obj map[string]any, key string) string {
	if obj == nil {
		return ""
	}
	value, _ := obj[key].(string)
	return value
}

func nestedMap(obj map[string]any, key string) map[string]any {
	if obj == nil {
		return nil
	}
	return normalizeMap(obj[key])
}

func normalizeMap(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return map[string]any{}
}

func requiredStringParam(params map[string]any, key string, field string) (string, error) {
	if params == nil {
		return "", fmt.Errorf("%s required", field)
	}
	value, ok := params[key]
	if !ok || value == nil {
		return "", fmt.Errorf("%s required", field)
	}
	typed, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", field)
	}
	if strings.TrimSpace(typed) == "" {
		return "", fmt.Errorf("%s required", field)
	}
	return typed, nil
}

func optionalObjectParam(params map[string]any, key string) (map[string]any, error) {
	return optionalObjectParamWithName(params, key, key)
}

func requiredObjectParam(params map[string]any, key string, field string) (map[string]any, error) {
	if params == nil {
		return nil, fmt.Errorf("completion requires %s", field)
	}
	value, ok := params[key]
	if !ok || value == nil {
		return nil, fmt.Errorf("completion requires %s", field)
	}
	typed, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", field)
	}
	return typed, nil
}

func optionalObjectParamWithName(params map[string]any, key string, field string) (map[string]any, error) {
	if params == nil {
		return map[string]any{}, nil
	}
	value, ok := params[key]
	if !ok || value == nil {
		return map[string]any{}, nil
	}
	typed, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", field)
	}
	return typed, nil
}

func requestID(r *http.Request) string {
	return r.Header.Get("X-Request-Id")
}

func writeRPC(w http.ResponseWriter, id any, result any) {
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeRPCError(w http.ResponseWriter, id any, status int, code int, message string, data interface{}) {
	writeJSON(w, status, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message, Data: data}})
}

// writeAccepted sends HTTP 202 Accepted. Used for MCP notifications which
// are accepted for processing but produce no JSON-RPC response body.
func writeAccepted(w http.ResponseWriter) {
	w.WriteHeader(http.StatusAccepted)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if s.pull == nil || !s.cfg.PullFile.Enabled {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.pull.RecordDownload(started, r.RemoteAddr, "", "method not allowed", false)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/file/")
	if token == "" || strings.Contains(token, "/") {
		s.pull.RecordDownload(started, r.RemoteAddr, "", "invalid token", false)
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "invalid or expired download link"})
		return
	}
	path, err := s.pull.Resolve(token, time.Now())
	if err != nil {
		s.pull.RecordDownload(started, r.RemoteAddr, "", err.Error(), false)
		if errors.Is(err, pullfile.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "invalid or expired download link"})
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		s.pull.RecordDownload(started, r.RemoteAddr, path, "file not found", false)
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		s.pull.RecordDownload(started, r.RemoteAddr, path, "open failed", false)
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	filename := filepath.Base(path)
	contentType := mime.TypeByExtension(filepath.Ext(filename))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `inline; filename="`+sanitizeHeaderValue(filename)+`"`)
	http.ServeContent(w, r, filename, info.ModTime(), f)
	s.pull.RecordDownload(started, r.RemoteAddr, path, fmt.Sprintf("served %d bytes", info.Size()), true)
}

func sanitizeHeaderValue(value string) string {
	value = strings.ReplaceAll(value, `"`, "")
	value = strings.ReplaceAll(value, "\n", "")
	value = strings.ReplaceAll(value, "\n", "")
	return value
}

func withCORS(cfg config.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && originAllowed(origin, cfg.AllowedOrigins) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id, "+sessionHeader+", "+protocolHeader)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Expose-Headers", sessionHeader+", "+protocolHeader)
		}
		next.ServeHTTP(w, r)
	})
}

func originAllowed(origin string, allowed []string) bool {
	if len(allowed) == 0 {
		return false
	}
	for _, candidate := range allowed {
		if candidate == "*" || candidate == origin {
			return true
		}
	}
	return false
}
