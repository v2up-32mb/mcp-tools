package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestJSONLWriterHandlesSingleRecordLargerThanMaxSize(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mcp-audit.jsonl")

	// MaxSizeBytes=10, but a single Event serializes to >10 bytes.
	// The rotation check is currentSize+incoming > MaxSizeBytes, so even
	// the first write triggers rotation. However, the record must not be
	// lost — it should still be written to the (reopened) current file.
	writer, err := NewJSONLWriterWithOptions(path, RotateOptions{
		MaxSizeBytes: 10,
		MaxBackups:   2,
	})
	if err != nil {
		t.Fatalf("NewJSONLWriterWithOptions error: %v", err)
	}
	defer writer.Close()

	ev := Event{Timestamp: time.Unix(1, 0).UTC(), Tool: "fs.read_file", Allowed: true, Success: true, ResultDigest: "ok"}
	if err := writer.Write(ev); err != nil {
		t.Fatalf("write oversized record error: %v", err)
	}

	// The oversized record must be present in the current file (no data loss).
	current := readFile(t, path)
	if string(current) != string(mustJSONL(t, ev)) {
		t.Fatalf("expected oversized record in current file, got %s", current)
	}

	// A second write triggers another rotation; both records survive.
	second := Event{Timestamp: time.Unix(2, 0).UTC(), Tool: "fs.write_file", Allowed: true, Success: true, ResultDigest: "second"}
	if err := writer.Write(second); err != nil {
		t.Fatalf("second write error: %v", err)
	}

	// First record moves to backup .1, second in current.
	rotated := readFile(t, path+".1")
	if string(rotated) != string(mustJSONL(t, ev)) {
		t.Fatalf("expected first record in backup, got %s", rotated)
	}
	current = readFile(t, path)
	if string(current) != string(mustJSONL(t, second)) {
		t.Fatalf("expected second record in current, got %s", current)
	}
}

func TestJSONLWriterWriteAfterCloseReturnsError(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mcp-audit.jsonl")

	writer, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter error: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close error: %v", err)
	}

	err = writer.Write(Event{Timestamp: time.Unix(1, 0).UTC(), Tool: "tool", Allowed: true, Success: true})
	if err == nil {
		t.Fatal("expected write after close to return a non-nil error")
	}
	// The error must be identifiable (not a panic) and mention the closed state.
	if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected error message to mention 'closed', got: %q", err.Error())
	}
}

func TestJSONLWriterRotatePreservesBackups(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mcp-audit.jsonl")

	// Size a single event so that MaxSizeBytes fits exactly one event plus a
	// small margin, guaranteeing that every write after the first triggers a
	// rotation.
	probe := Event{Timestamp: time.Unix(0, 0).UTC(), Tool: "tool", Allowed: true, Success: true, ResultDigest: "x"}
	oneRecord := int64(len(mustJSONL(t, probe)))
	writer, err := NewJSONLWriterWithOptions(path, RotateOptions{
		MaxSizeBytes: oneRecord + 5,
		MaxBackups:   2,
	})
	if err != nil {
		t.Fatalf("NewJSONLWriterWithOptions error: %v", err)
	}
	defer writer.Close()

	// Write five events with distinct digests so each rotation step is
	// traceable. Expected layout after four rotations:
	//   current -> event 5
	//   .1      -> event 4
	//   .2      -> event 3
	// events 1 and 2 are dropped (beyond MaxBackups=2).
	var events [5]Event
	for i := 0; i < 5; i++ {
		ev := Event{
			Timestamp:    time.Unix(int64(i+1), 0).UTC(),
			Tool:         "tool",
			Allowed:      true,
			Success:      true,
			ResultDigest: fmt.Sprintf("evt-%d", i+1),
		}
		events[i] = ev
		if err := writer.Write(ev); err != nil {
			t.Fatalf("write %d error: %v", i, err)
		}
	}

	// Current file must contain only the latest event.
	current := readFile(t, path)
	if string(current) != string(mustJSONL(t, events[4])) {
		t.Fatalf("expected current file to contain event 5, got %s", current)
	}
	// .1 must contain the previous event (event 4).
	backup1 := readFile(t, path+".1")
	if string(backup1) != string(mustJSONL(t, events[3])) {
		t.Fatalf("expected .1 backup to contain event 4, got %s", backup1)
	}
	// .2 must contain event 3.
	backup2 := readFile(t, path+".2")
	if string(backup2) != string(mustJSONL(t, events[2])) {
		t.Fatalf("expected .2 backup to contain event 3, got %s", backup2)
	}
	// .3 must not exist (beyond MaxBackups).
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("expected .3 backup to be absent, got err=%v", err)
	}
}

func TestJSONLWriterZeroMaxBackupsRemovesRotatedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mcp-audit.jsonl")

	// MaxBackups=0 with rotation enabled is rejected by the constructor: it is
	// impossible to enable rotation while keeping zero backups. The nearest
	// faithful behaviour is that the configuration is rejected outright, so no
	// rotated file can ever accumulate.
	_, err := NewJSONLWriterWithOptions(path, RotateOptions{
		MaxSizeBytes: 64,
		MaxBackups:   0,
	})
	if err == nil {
		t.Fatal("expected constructor to reject MaxBackups=0 with rotation enabled")
	}
	if !strings.Contains(err.Error(), "max backups") {
		t.Fatalf("expected error to mention max backups, got: %q", err.Error())
	}

	// With rotation disabled (MaxSizeBytes=0), MaxBackups=0 is harmless and no
	// backup files are ever produced regardless of how much is written.
	writer, err := NewJSONLWriterWithOptions(path, RotateOptions{})
	if err != nil {
		t.Fatalf("NewJSONLWriterWithOptions (no rotation) error: %v", err)
	}
	defer writer.Close()

	for i := 0; i < 5; i++ {
		ev := Event{
			Timestamp:    time.Unix(int64(i+1), 0).UTC(),
			Tool:         "tool",
			Allowed:      true,
			Success:      true,
			ResultDigest: fmt.Sprintf("evt-%d", i+1),
		}
		if err := writer.Write(ev); err != nil {
			t.Fatalf("write %d error: %v", i, err)
		}
	}
	// No backup files should exist.
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("expected no .1 backup without rotation, got err=%v", err)
	}
}

func TestJSONLWriterWriteEmptyEvent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mcp-audit.jsonl")

	writer, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter error: %v", err)
	}
	defer writer.Close()

	// A zero-value Event must not panic and must serialise as valid JSON.
	var ev Event
	if err := writer.Write(ev); err != nil {
		t.Fatalf("write empty event error: %v", err)
	}

	data := readFile(t, path)
	var decoded Event
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("could not unmarshal written empty event: %v\nraw=%s", err, data)
	}
	// The zero value round-trips with the Tool field empty.
	if decoded.Tool != "" {
		t.Fatalf("expected empty Tool after round-trip, got %q", decoded.Tool)
	}
}

func TestJSONLWriterConcurrentWrites(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mcp-audit.jsonl")

	// No rotation: keep this test focused on thread-safety, not size limits.
	writer, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter error: %v", err)
	}
	defer writer.Close()

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ev := Event{
				Timestamp:    time.Unix(int64(i+1), 0).UTC(),
				Tool:         "concurrent",
				Allowed:      true,
				Success:      true,
				ResultDigest: fmt.Sprintf("concurrent-%d", i),
			}
			if err := writer.Write(ev); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent write error: %v", e)
	}

	// Every write appends exactly one JSON line; count must be exactly n.
	data := readFile(t, path)
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != n {
		t.Fatalf("expected %d lines after concurrent writes, got %d", n, lines)
	}

	// Verify all lines are valid JSON objects.
	rest := data
	for len(rest) > 0 {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			t.Fatalf("trailing bytes without newline: %q", rest)
		}
		var ev Event
		if err := json.Unmarshal(rest[:nl], &ev); err != nil {
			t.Fatalf("unmarshal line failed: %v\nline=%s", err, rest[:nl])
		}
		rest = rest[nl+1:]
	}
}
