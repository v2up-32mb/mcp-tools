package audit

import (
	"encoding/json"
	"fmt"
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
	EnvKeys         []string       `json:"env_keys,omitempty"`
	DurationMS      int64          `json:"duration_ms"`
	ResultDigest    string         `json:"result_digest,omitempty"`
}

type Logger interface {
	Write(Event) error
	Close() error
}

type RotateOptions struct {
	MaxSizeBytes int64
	MaxBackups   int
}

type JSONLWriter struct {
	mu          sync.Mutex
	path        string
	f           *os.File
	currentSize int64
	rotate      RotateOptions
}

func NewJSONLWriter(path string) (*JSONLWriter, error) {
	return NewJSONLWriterWithOptions(path, RotateOptions{})
}

func NewJSONLWriterWithOptions(path string, opts RotateOptions) (*JSONLWriter, error) {
	if opts.MaxSizeBytes > 0 && opts.MaxBackups <= 0 {
		return nil, fmt.Errorf("rotate max backups must be positive when rotation is enabled")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &JSONLWriter{
		path:        path,
		f:           f,
		currentSize: info.Size(),
		rotate:      opts,
	}, nil
}

func (w *JSONLWriter) Write(ev Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if err := w.rotateIfNeeded(int64(len(payload))); err != nil {
		return err
	}
	n, err := w.f.Write(payload)
	w.currentSize += int64(n)
	return err
}

func (w *JSONLWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	return w.f.Close()
}

func (w *JSONLWriter) rotateIfNeeded(incomingBytes int64) error {
	if w.rotate.MaxSizeBytes <= 0 {
		return nil
	}
	if w.currentSize+incomingBytes <= w.rotate.MaxSizeBytes {
		return nil
	}
	if err := w.f.Close(); err != nil {
		return err
	}
	for idx := w.rotate.MaxBackups; idx >= 1; idx-- {
		src := fmt.Sprintf("%s.%d", w.path, idx)
		if idx == w.rotate.MaxBackups {
			if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		dst := fmt.Sprintf("%s.%d", w.path, idx+1)
		if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.f = f
	w.currentSize = info.Size()
	return nil
}
