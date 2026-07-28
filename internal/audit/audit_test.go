package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestJSONLWriterRotatesWhenMaxSizeExceeded(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mcp-audit.jsonl")

	first := Event{Timestamp: time.Unix(1, 0).UTC(), Tool: "fs.read_file", Allowed: true, Success: true, ResultDigest: "first"}
	payload := mustJSONL(t, first)
	writer, err := NewJSONLWriterWithOptions(path, RotateOptions{
		MaxSizeBytes: int64(len(payload) + 10),
		MaxBackups:   2,
	})
	if err != nil {
		t.Fatalf("NewJSONLWriterWithOptions error: %v", err)
	}
	defer writer.Close()

	if err := writer.Write(first); err != nil {
		t.Fatalf("first write error: %v", err)
	}
	second := Event{Timestamp: time.Unix(2, 0).UTC(), Tool: "fs.write_file", Allowed: true, Success: true, ResultDigest: "second"}
	if err := writer.Write(second); err != nil {
		t.Fatalf("second write error: %v", err)
	}

	current := readFile(t, path)
	if string(current) != string(mustJSONL(t, second)) {
		t.Fatalf("unexpected current audit log contents: %s", current)
	}
	rotated := readFile(t, path+".1")
	if string(rotated) != string(mustJSONL(t, first)) {
		t.Fatalf("unexpected rotated audit log contents: %s", rotated)
	}
}

func TestJSONLWriterRemovesOldestBackupBeyondMaxBackups(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mcp-audit.jsonl")

	writer, err := NewJSONLWriterWithOptions(path, RotateOptions{
		MaxSizeBytes: int64(len(mustJSONL(t, Event{Timestamp: time.Unix(1, 0).UTC(), Tool: "tool", Allowed: true, Success: true})) + 5),
		MaxBackups:   2,
	})
	if err != nil {
		t.Fatalf("NewJSONLWriterWithOptions error: %v", err)
	}
	defer writer.Close()

	for i := 0; i < 4; i++ {
		ev := Event{
			Timestamp:    time.Unix(int64(i+1), 0).UTC(),
			Tool:         "tool",
			Allowed:      true,
			Success:      true,
			ResultDigest: time.Unix(int64(i+1), 0).UTC().Format(time.RFC3339),
		}
		if err := writer.Write(ev); err != nil {
			t.Fatalf("write %d error: %v", i, err)
		}
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected first backup to exist: %v", err)
	}
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Fatalf("expected second backup to exist: %v", err)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("expected third backup to be removed, got err=%v", err)
	}
}

func TestJSONLWriterUsesExistingFileSizeOnStartup(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mcp-audit.jsonl")

	existing := mustJSONL(t, Event{Timestamp: time.Unix(1, 0).UTC(), Tool: "existing", Allowed: true, Success: true, ResultDigest: "existing"})
	if err := os.WriteFile(path, existing, 0o600); err != nil {
		t.Fatalf("write existing file: %v", err)
	}
	writer, err := NewJSONLWriterWithOptions(path, RotateOptions{
		MaxSizeBytes: int64(len(existing) + 10),
		MaxBackups:   1,
	})
	if err != nil {
		t.Fatalf("NewJSONLWriterWithOptions error: %v", err)
	}
	defer writer.Close()

	next := Event{Timestamp: time.Unix(2, 0).UTC(), Tool: "next", Allowed: true, Success: true, ResultDigest: "next"}
	if err := writer.Write(next); err != nil {
		t.Fatalf("write next: %v", err)
	}

	current := readFile(t, path)
	if string(current) != string(mustJSONL(t, next)) {
		t.Fatalf("expected current file to contain only second event, got %s", current)
	}
	rotated := readFile(t, path+".1")
	if string(rotated) != string(existing) {
		t.Fatalf("expected rotated file to contain existing event, got %s", rotated)
	}
}

func TestJSONLWriterCloseIsIdempotentAndWriteAfterCloseFails(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mcp-audit.jsonl")

	writer, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter error: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("first close error: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("second close should be idempotent, got %v", err)
	}
	err = writer.Write(Event{Timestamp: time.Unix(1, 0).UTC(), Tool: "tool", Allowed: true, Success: true})
	if err == nil {
		t.Fatal("expected write after close to fail")
	}
}

func TestJSONLWriterCanRetryAfterRotationFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mcp-audit.jsonl")

	first := Event{Timestamp: time.Unix(1, 0).UTC(), Tool: "first", Allowed: true, Success: true, ResultDigest: "first"}
	writer, err := NewJSONLWriterWithOptions(path, RotateOptions{
		MaxSizeBytes: int64(len(mustJSONL(t, first)) + 10),
		MaxBackups:   1,
	})
	if err != nil {
		t.Fatalf("NewJSONLWriterWithOptions error: %v", err)
	}
	defer writer.Close()

	if err := writer.Write(first); err != nil {
		t.Fatalf("first write error: %v", err)
	}
	blocker := path + ".1"
	if err := os.Mkdir(blocker, 0o755); err != nil {
		t.Fatalf("make blocker dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blocker, "keep"), []byte("block\n"), 0o600); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}

	second := Event{Timestamp: time.Unix(2, 0).UTC(), Tool: "second", Allowed: true, Success: true, ResultDigest: "second"}
	if err := writer.Write(second); err == nil {
		t.Fatal("expected rotation to fail while oldest backup is a non-empty directory")
	}
	if err := os.Remove(filepath.Join(blocker, "keep")); err != nil {
		t.Fatalf("remove blocker file: %v", err)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatalf("remove blocker dir: %v", err)
	}
	if err := writer.Write(second); err != nil {
		t.Fatalf("retry write after clearing rotation blocker: %v", err)
	}

	current := readFile(t, path)
	if string(current) != string(mustJSONL(t, second)) {
		t.Fatalf("unexpected current audit log contents after retry: %s", current)
	}
	rotated := readFile(t, path+".1")
	if string(rotated) != string(mustJSONL(t, first)) {
		t.Fatalf("unexpected rotated audit log contents after retry: %s", rotated)
	}
}

func mustJSONL(t *testing.T, ev Event) []byte {
	t.Helper()
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return append(payload, '\n')
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return payload
}
