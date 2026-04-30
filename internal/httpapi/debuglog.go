package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/example/mcp-tools/internal/applog"
)

type debugResponseRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *debugResponseRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *debugResponseRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(p)
}

func (r *debugResponseRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *debugResponseRecorder) StatusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func (s *Server) wrapDebugResponseWriter(w http.ResponseWriter) *debugResponseRecorder {
	return &debugResponseRecorder{ResponseWriter: w}
}

func (s *Server) debugEnabled() bool {
	return strings.EqualFold(s.cfg.LogLevel, "DEBUG")
}

func (s *Server) debugLog(event string, fields map[string]any) {
	if !s.debugEnabled() {
		return
	}
	allFields := []applog.Field{{Key: "event", Value: event}}
	allFields = append(allFields, debugMapFields(fields)...)
	applog.Default().Debug("mcp.http", debugMessage(event), allFields...)
}

func (s *Server) debugRequestFields(r *http.Request) map[string]any {
	authPresent, authScheme := summarizeAuthorization(r.Header.Get("Authorization"))
	return map[string]any{
		"remote_addr":            r.RemoteAddr,
		"http_method":            r.Method,
		"path":                   r.URL.Path,
		"content_type":           r.Header.Get("Content-Type"),
		"accept":                 r.Header.Get("Accept"),
		"origin":                 r.Header.Get("Origin"),
		"authorization_present":  authPresent,
		"authorization_scheme":   authScheme,
		"session_header_present": strings.TrimSpace(r.Header.Get(sessionHeader)) != "",
		"protocol_header":        strings.TrimSpace(r.Header.Get(protocolHeader)),
		"request_id":             strings.TrimSpace(r.Header.Get("X-Request-Id")),
	}
}

func (s *Server) debugRPCFields(req rpcRequest) map[string]any {
	fields := map[string]any{
		"rpc_jsonrpc":    req.JSONRPC,
		"rpc_method":     req.Method,
		"rpc_id_present": req.ID != nil,
	}
	if req.Method == "initialize" {
		fields["initialize_requested_protocol"] = nestedString(req.Params, "protocolVersion")
	}
	if req.Method == "tools/call" {
		if toolName, _ := req.Params["name"].(string); strings.TrimSpace(toolName) != "" {
			fields["tool_name"] = toolName
		}
	}
	return fields
}

func (s *Server) debugLogRejected(r *http.Request, req *rpcRequest, httpStatus int, rpcErrorCode int, rpcErrorMessage string, extra map[string]any) {
	fields := s.debugRequestFields(r)
	if req != nil {
		for k, v := range s.debugRPCFields(*req) {
			fields[k] = v
		}
	}
	fields["http_status"] = httpStatus
	fields["rpc_error_code"] = rpcErrorCode
	fields["rpc_error_message"] = rpcErrorMessage
	for k, v := range extra {
		fields[k] = v
	}
	s.debugLog("mcp_request_rejected", fields)
}

func (s *Server) debugLogCompleted(start time.Time, recorder *debugResponseRecorder, fields map[string]any) {
	if !s.debugEnabled() || recorder == nil {
		return
	}
	completed := copyDebugFields(fields)
	completed["http_status"] = recorder.StatusCode()
	completed["duration_ms"] = time.Since(start).Milliseconds()
	s.debugLog("http_request_completed", completed)
}

func summarizeAuthorization(header string) (bool, string) {
	trimmed := strings.TrimSpace(header)
	if trimmed == "" {
		return false, ""
	}
	parts := strings.Fields(trimmed)
	if len(parts) == 0 {
		return true, ""
	}
	return true, parts[0]
}

func copyDebugFields(fields map[string]any) map[string]any {
	copied := make(map[string]any, len(fields))
	for k, v := range fields {
		copied[k] = v
	}
	return copied
}

func debugMessage(event string) string {
	switch event {
	case "http_request_received":
		return "http request received"
	case "mcp_request_rejected":
		return "mcp request rejected"
	case "http_request_completed":
		return "http request completed"
	default:
		return strings.ReplaceAll(event, "_", " ")
	}
}

func debugMapFields(values map[string]any) []applog.Field {
	order := []string{
		"event",
		"handler",
		"remote_addr",
		"http_method",
		"path",
		"content_type",
		"accept",
		"origin",
		"authorization_present",
		"authorization_scheme",
		"session_header_present",
		"protocol_header",
		"request_id",
		"rpc_jsonrpc",
		"rpc_method",
		"rpc_id_present",
		"tool_name",
		"initialize_requested_protocol",
		"http_status",
		"rpc_error_code",
		"rpc_error_message",
		"expected_protocol",
		"supported_protocols",
		"transport",
		"duration_ms",
	}
	fields := make([]applog.Field, 0, len(values))
	seen := make(map[string]bool, len(order))
	for _, key := range order {
		value, ok := values[key]
		if !ok {
			continue
		}
		fields = append(fields, applog.Field{Key: key, Value: value})
		seen[key] = true
	}
	for key, value := range values {
		if seen[key] {
			continue
		}
		fields = append(fields, applog.Field{Key: key, Value: value})
	}
	return fields
}
