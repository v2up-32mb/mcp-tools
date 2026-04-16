package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Event struct {
	Timestamp       time.Time      `json:"timestamp"`
	RequestID       string         `json:"request_id,omitempty"`
	SessionID       string         `json:"session_id,omitempty"`
	ProtocolVersion string         `json:"protocol_version,omitempty"`
	RemoteAddr      string         `json:"remote_addr,omitempty"`
	Tool            string         `json:"tool"`
	Arguments       map[string]any `json:"arguments,omitempty"`
	Workdir         string         `json:"workdir,omitempty"`
	TargetPath      string         `json:"target_path,omitempty"`
	Allowed         bool           `json:"allowed"`
	Success         bool           `json:"success"`
	Error           string         `json:"error,omitempty"`
	ExitCode        *int           `json:"exit_code,omitempty"`
	Stdout          string         `json:"stdout,omitempty"`
	Stderr          string         `json:"stderr,omitempty"`
	DurationMS      int64          `json:"duration_ms"`
	ResultDigest    string         `json:"result_digest,omitempty"`
}

type Logger interface {
	Write(Event) error
	Close() error
}

type JSONLWriter struct {
	mu sync.Mutex
	f  *os.File
}

func NewJSONLWriter(path string) (*JSONLWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &JSONLWriter{f: f}, nil
}

func (w *JSONLWriter) Write(ev Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = w.f.Write(append(payload, '\n'))
	return err
}

func (w *JSONLWriter) Close() error {
	return w.f.Close()
}
