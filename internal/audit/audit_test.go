package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewJSONLWriterCreatesParentDirectories(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "logs", "mcp-audit.jsonl")

	writer, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter error: %v", err)
	}
	defer writer.Close()

	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("expected parent directory to exist: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected audit log file to exist: %v", err)
	}
}
