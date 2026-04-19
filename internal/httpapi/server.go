package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
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
}

type rpcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

func NewServer(cfg config.Config, registry *mcp.Registry) http.Handler {
	srv := &Server{
		cfg:      cfg,
		registry: registry,
		sessions: session.NewManager(cfg.SessionTTL),
		streams:  newStreamHub(cfg.StreamQueueSize),
		resSubs:  newResourceSubscriptions(),
		metrics:  newServerMetrics(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.handleHealthz)
	mux.HandleFunc("/readyz", srv.handleReadyz)
	mux.HandleFunc("/debug/statez", srv.handleStatez)
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
	if origin := r.Header.Get("Origin"); origin != "" && !originAllowed(origin, s.cfg.AllowedOrigins) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "origin not allowed"})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
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
	if strings.HasPrefix(req.Method, "notifications/") {
		if req.Method == "notifications/initialized" {
			writeNoContent(w)
			return
		}
		writeNoContent(w)
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
	requested := nestedString(req.Params, "protocolVersion")
	protocol, ok := chooseProtocol(requested, s.cfg.SupportedProtocols)
	if !ok {
		s.debugLogRejected(r, &req, http.StatusBadRequest, -32002, "unsupported protocol version", map[string]any{"supported_protocols": s.cfg.SupportedProtocols})
		writeRPCError(w, req.ID, http.StatusBadRequest, -32002, "unsupported protocol version", map[string]any{"supported": s.cfg.SupportedProtocols})
		return
	}

	sess, err := s.sessions.Create(protocol, nestedMap(req.Params, "clientInfo"))
	if err != nil {
		writeRPCError(w, req.ID, http.StatusInternalServerError, -32603, "create session failed", nil)
		return
	}
	w.Header().Set(sessionHeader, sess.ID)
	w.Header().Set(protocolHeader, protocol)
	writeRPC(w, req.ID, s.initializeResult(protocol))
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
	uri, _ := req.Params["uri"].(string)
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
	uri, _ := req.Params["uri"].(string)
	if _, err := s.readResource(uri); err != nil {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32602, err.Error(), nil)
		return
	}
	s.resSubs.subscribe(r.Header.Get(sessionHeader), uri)
	writeRPC(w, req.ID, map[string]any{})
}

func (s *Server) handleResourcesUnsubscribe(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	if _, ok := s.requireSession(w, r, req.ID); !ok {
		return
	}
	uri, _ := req.Params["uri"].(string)
	if strings.TrimSpace(uri) == "" {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32602, "resource uri required", nil)
		return
	}
	s.resSubs.unsubscribe(r.Header.Get(sessionHeader), uri)
	writeRPC(w, req.ID, map[string]any{})
}

func (s *Server) handlePromptGet(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	if _, ok := s.requireSession(w, r, req.ID); !ok {
		return
	}
	name, _ := req.Params["name"].(string)
	args := normalizeMap(req.Params["arguments"])
	result, err := s.getPrompt(name, args)
	if err != nil {
		writeRPCError(w, req.ID, http.StatusBadRequest, -32602, err.Error(), nil)
		return
	}
	writeRPC(w, req.ID, result)
}

func (s *Server) handleToolsCall(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	result, ok := s.callTool(r, req, w, false)
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
	return s.sessions.Get(sessionID)
}

func (s *Server) callTool(r *http.Request, req rpcRequest, w http.ResponseWriter, sse bool) (mcp.Result, bool) {
	sess, ok := s.requireSessionForMode(w, r, req.ID, sse)
	if !ok {
		return mcp.Result{}, false
	}
	toolName, _ := req.Params["name"].(string)
	if strings.TrimSpace(toolName) == "" {
		if sse {
			return mcp.ErrorResult("tool name required", mcp.AuditData{Allowed: true, ResultDigest: "validation error"}), true
		}
		writeRPCError(w, req.ID, http.StatusBadRequest, -32602, "tool name required", nil)
		return mcp.Result{}, false
	}
	args := normalizeMap(req.Params["arguments"])
	result, err := s.registry.Call(r.Context(), mcp.CallContext{
		RemoteAddr:      r.RemoteAddr,
		RequestID:       requestID(r),
		SessionID:       sess.ID,
		ProtocolVersion: sess.ProtocolVersion,
	}, toolName, args)
	s.trackTemplateCall(toolName, args, result)
	if err != nil {
		if sse {
			return mcp.ErrorResult(err.Error(), mcp.AuditData{Allowed: true, ResultDigest: "tool call error"}), true
		}
		writeRPCError(w, req.ID, http.StatusBadRequest, -32010, err.Error(), nil)
		return mcp.Result{}, false
	}
	s.maybeNotifyResourceUpdated(toolName, args, result)
	return result, true
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
			"uri":         "file://" + root,
			"name":        root,
			"description": "Allowed workspace root for MCP file and command operations.",
			"mimeType":    "inode/directory",
		})
	}
	resources = append(resources, map[string]any{
		"uri":         "file://" + s.cfg.AuditLogPath,
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
		uriTemplate := strings.TrimRight("file://"+root, "/") + "/{path}"
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
	if strings.TrimSpace(uri) == "" {
		return nil, errors.New("resource uri required")
	}
	if !strings.HasPrefix(uri, "file://") {
		return nil, fmt.Errorf("unsupported resource uri %q", uri)
	}
	path := strings.TrimPrefix(uri, "file://")
	resolved, err := security.ResolvePath(path, s.cfg.AllowedRoots)
	if err != nil {
		if filepath.Clean(path) == filepath.Clean(s.cfg.AuditLogPath) {
			resolved = filepath.Clean(path)
		} else {
			return nil, fmt.Errorf("resolve resource: %w", err)
		}
	}
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
				"uri":      uri,
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
				"uri":      uri,
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
			"uri":      uri,
			"mimeType": mimeType,
			"text":     string(payload),
			"_meta":    resourceMetadata(resolved, info, false, 0),
		}},
	}, nil
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
	ref := normalizeMap(params["ref"])
	refType, _ := ref["type"].(string)
	argument := normalizeMap(params["argument"])
	argName, _ := argument["name"].(string)
	argValue, _ := argument["value"].(string)
	contextArgs := normalizeMap(normalizeMap(params["context"])["arguments"])

	if strings.TrimSpace(refType) == "" || strings.TrimSpace(argName) == "" {
		return nil, errors.New("completion requires ref.type and argument.name")
	}

	var values []string
	switch refType {
	case "ref/prompt":
		name, _ := ref["name"].(string)
		completed, err := s.completePrompt(name, argName, argValue, contextArgs)
		if err != nil {
			return nil, err
		}
		values = completed
	case "ref/resource":
		uri, _ := ref["uri"].(string)
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
			workdir, _ := contextArgs["workdir"].(string)
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
			candidate, err := security.ResolvePath(workdir, s.cfg.AllowedRoots)
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
	prefix = strings.TrimSpace(prefix)
	searchDir := root
	basePrefix := ""
	if prefix != "" {
		cleanPrefix := filepath.Clean(prefix)
		if cleanPrefix == "." {
			cleanPrefix = ""
		}
		if strings.Contains(prefix, "/") {
			basePrefix = filepath.Dir(cleanPrefix)
			if basePrefix == "." {
				basePrefix = ""
			}
			candidateDir := filepath.Join(root, basePrefix)
			if info, err := os.Stat(candidateDir); err == nil && info.IsDir() {
				searchDir = candidateDir
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
			"description": "Plan a scoped file edit using fs.read_file, fs.edit_lines, and validation steps.",
			"arguments": []map[string]any{
				promptArgument("task", "The edit objective.", true),
				promptArgument("path", "Target file path inside allowed roots.", true),
				promptArgument("expected_old_text", "Optional old text for optimistic concurrency during fs.edit_lines.", false),
			},
			"_meta": map[string]any{
				"inputSchema": map[string]any{
					"type":     "object",
					"required": []string{"task", "path"},
					"properties": map[string]any{
						"task": map[string]any{
							"type":        "string",
							"description": "The edit objective.",
							"examples":    []string{"update readme heading"},
						},
						"path": map[string]any{
							"type":        "string",
							"description": "Target file path inside allowed roots.",
							"examples":    []string{"README.md"},
						},
						"expected_old_text": map[string]any{
							"type":        "string",
							"description": "Optional old text for optimistic concurrency during fs.edit_lines.",
							"examples":    []string{"# old heading"},
						},
					},
				},
			},
		},
		{
			"name":        "go_dev_loop",
			"title":       "Go Dev Loop",
			"description": "Iterate on a Go code change using fs tools plus exec.run gofmt/go test/go vet.",
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
							"description": "Requested Go change or bug fix.",
							"examples":    []string{"fix failing tests"},
						},
						"workdir": map[string]any{
							"type":        "string",
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
		task, _ := args["task"].(string)
		path, _ := args["path"].(string)
		expectedOldText, _ := args["expected_old_text"].(string)
		if strings.TrimSpace(task) == "" || strings.TrimSpace(path) == "" {
			return nil, errors.New("safe_file_edit requires task and path")
		}
		text := fmt.Sprintf("Edit %s for task %q. First inspect with fs.read_file, then make minimal changes with fs.edit_lines, then verify with fs.read_file or exec.run if relevant. Keep the target inside allowed roots.", path, task)
		if strings.TrimSpace(expectedOldText) != "" {
			text += fmt.Sprintf(" Use expected_old_text=%q when calling fs.edit_lines if the old text must match exactly.", expectedOldText)
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
		goal, _ := args["goal"].(string)
		workdir, _ := args["workdir"].(string)
		testTarget, _ := args["test_target"].(string)
		runVet, runVetOK := args["run_vet"].(bool)
		if strings.TrimSpace(goal) == "" || strings.TrimSpace(workdir) == "" {
			return nil, errors.New("go_dev_loop requires goal and workdir")
		}
		if strings.TrimSpace(testTarget) == "" {
			testTarget = "./..."
		}
		if !runVetOK {
			runVet = true
		}
		text := fmt.Sprintf("Work in %s to achieve %q. Inspect relevant files, edit with fs.edit_lines or fs.write_file, then run exec.run with go_fmt and go_test (target %s) as needed.", workdir, goal, testTarget)
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

func promptArgument(name, description string, required bool) map[string]any {
	return map[string]any{
		"name":        name,
		"description": description,
		"required":    required,
	}
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
	case "fs.write_file", "fs.replace_text", "fs.edit_lines", "fs.make_dir", "fs.delete_path":
		if path, _ := args["path"].(string); strings.TrimSpace(path) != "" {
			if resolved, err := security.ResolvePath(resolveAgainstStartup(path, s.cfg.StartupDirectory), s.cfg.AllowedRoots); err == nil {
				uris = append(uris, resourceUpdateTargets(resolved, s.cfg.AllowedRoots)...)
			}
		}
	case "fs.move_path":
		for _, key := range []string{"src", "dst"} {
			if path, _ := args[key].(string); strings.TrimSpace(path) != "" {
				if resolved, err := security.ResolvePath(resolveAgainstStartup(path, s.cfg.StartupDirectory), s.cfg.AllowedRoots); err == nil {
					uris = append(uris, resourceUpdateTargets(resolved, s.cfg.AllowedRoots)...)
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
	return map[string]any{
		"server": map[string]any{
			"name":                s.cfg.ServerName,
			"version":             s.cfg.ServerVersion,
			"supported_protocols": s.cfg.SupportedProtocols,
		},
		"config": map[string]any{
			"listen_addr":        s.cfg.ListenAddr,
			"allowed_roots":      s.cfg.AllowedRoots,
			"allowed_origins":    s.cfg.AllowedOrigins,
			"audit_log_path":     s.cfg.AuditLogPath,
			"output_max_bytes":   s.cfg.OutputMaxBytes,
			"stream_queue_size":  s.cfg.StreamQueueSize,
			"max_request_bytes":  s.cfg.MaxRequestBytes,
			"command_timeout_ms": s.cfg.CommandTimeout.Milliseconds(),
			"session_ttl_ms":     s.cfg.SessionTTL.Milliseconds(),
			"command_templates":  summarizeCommandTemplates(s.cfg.CommandTemplates),
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

func summarizeCommandTemplates(templates map[string]config.CommandTemplate) map[string]any {
	out := make(map[string]any, len(templates))
	for name, tpl := range templates {
		entry := map[string]any{
			"command":               cloneStrings(tpl.Command),
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
			entry["allowed_workdirs"] = cloneStrings(tpl.AllowedWorkdirs)
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
		confirm, _ := args["confirm"].(bool)
		if !confirm && result.IsError {
			confirmationBlocked = true
		}
	}
	s.metrics.recordTemplateCall(templateName, !result.IsError, confirmationBlocked)
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func resourceUpdateTargets(resolved string, allowedRoots []string) []string {
	targets := []string{"file://" + resolved}
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
			targets = append(targets, "file://"+current)
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
	var req rpcRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		return rpcRequest{}, fmt.Errorf("invalid json")
	}
	if req.JSONRPC == "" {
		req.JSONRPC = "2.0"
	}
	if req.JSONRPC != "2.0" {
		return rpcRequest{}, fmt.Errorf("unsupported jsonrpc version")
	}
	if req.Params == nil {
		req.Params = make(map[string]any)
	}
	return req, nil
}

func chooseProtocol(requested string, supported []string) (string, bool) {
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

func requestID(r *http.Request) string {
	return r.Header.Get("X-Request-Id")
}

func writeRPC(w http.ResponseWriter, id any, result any) {
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeRPCError(w http.ResponseWriter, id any, status int, code int, message string, data interface{}) {
	writeJSON(w, status, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message, Data: data}})
}

func writeNoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusAccepted)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func withCORS(cfg config.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && originAllowed(origin, cfg.AllowedOrigins) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id, "+sessionHeader)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
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
