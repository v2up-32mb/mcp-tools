package applog

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

type Field struct {
	Key   string
	Value any
}

type Logger struct {
	mu       sync.Mutex
	out      io.Writer
	minLevel Level
	now      func() time.Time
}

var (
	defaultMu     sync.RWMutex
	defaultLogger = New(io.Discard, "INFO")
)

func New(out io.Writer, minLevel string) *Logger {
	if out == nil {
		out = io.Discard
	}
	return &Logger{
		out:      out,
		minLevel: parseLevel(minLevel),
		now:      time.Now,
	}
}

func SetDefault(logger *Logger) {
	if logger == nil {
		logger = New(io.Discard, "INFO")
	}
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultLogger = logger
}

func Default() *Logger {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultLogger
}

func (l *Logger) Enabled(level Level) bool {
	if l == nil {
		return false
	}
	return level >= l.minLevel
}

func (l *Logger) Debug(component, message string, fields ...Field) {
	l.log(LevelDebug, component, message, fields...)
}

func (l *Logger) Info(component, message string, fields ...Field) {
	l.log(LevelInfo, component, message, fields...)
}

func (l *Logger) Warn(component, message string, fields ...Field) {
	l.log(LevelWarn, component, message, fields...)
}

func (l *Logger) Error(component, message string, fields ...Field) {
	l.log(LevelError, component, message, fields...)
}

func (l *Logger) log(level Level, component, message string, fields ...Field) {
	if !l.Enabled(level) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	ts := l.now().Format("2006-01-02 15:04:05")
	fmt.Fprintf(l.out, "[%s] %s  %s\n", ts, level.String(), strings.TrimSpace(component))
	fmt.Fprintln(l.out, strings.TrimSpace(message))
	for _, field := range fields {
		if strings.TrimSpace(field.Key) == "" {
			continue
		}
		fmt.Fprintf(l.out, "  %s: %s\n", field.Key, stringify(field.Value))
	}
	fmt.Fprintln(l.out)
}

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

func parseLevel(raw string) Level {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "DEBUG":
		return LevelDebug
	case "WARN":
		return LevelWarn
	case "ERROR":
		return LevelError
	default:
		return LevelInfo
	}
}

func stringify(v any) string {
	switch value := v.(type) {
	case nil:
		return ""
	case []string:
		return strings.Join(value, ", ")
	default:
		return fmt.Sprintf("%v", value)
	}
}

func NewDefaultStderr(level string) *Logger {
	return New(os.Stderr, level)
}
