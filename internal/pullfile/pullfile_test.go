package pullfile

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/mcp-tools/internal/config"
)

var pngMagic = "\x89PNG\r\n\x1a\n" + strings.Repeat("x", 64)

func testConfig(t *testing.T, mutate func(*config.PullFileConfig)) (config.Config, string) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Config{
		AllowedRoots:     []string{root},
		StartupDirectory: root,
		PullFile: config.PullFileConfig{
			Enabled:    true,
			MaxBytes:   1024,
			TTLSeconds: 300,
		},
	}
	if mutate != nil {
		mutate(&cfg.PullFile)
	}
	return cfg, root
}

func writePNG(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(pngMagic), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIssueResolveRoundtrip(t *testing.T) {
	cfg, root := testConfig(t, nil)
	mgr := NewManager(cfg, nil)
	path := writePNG(t, root, "progress.png")
	now := time.Now()

	dl, err := mgr.Issue(path, 0, now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if dl.Token == "" {
		t.Fatal("token should not be empty")
	}
	if dl.Filename != "progress.png" {
		t.Fatalf("filename=%q", dl.Filename)
	}
	if dl.MIMEType != "image/png" {
		t.Fatalf("mime=%q", dl.MIMEType)
	}
	if dl.Bytes != int64(len(pngMagic)) {
		t.Fatalf("bytes=%d", dl.Bytes)
	}
	got, err := mgr.Resolve(dl.Token, now.Add(10*time.Second))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != path {
		t.Fatalf("resolved path=%q want %q", got, path)
	}
}

func TestIssueDisabled(t *testing.T) {
	cfg, root := testConfig(t, func(c *config.PullFileConfig) { c.Enabled = false })
	mgr := NewManager(cfg, nil)
	path := writePNG(t, root, "a.png")
	if _, err := mgr.Issue(path, 0, time.Now()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("want ErrDisabled, got %v", err)
	}
}

func TestIssueRejectsMissingAndDirectory(t *testing.T) {
	cfg, root := testConfig(t, nil)
	mgr := NewManager(cfg, nil)
	if _, err := mgr.Issue(filepath.Join(root, "nope.png"), 0, time.Now()); !errors.Is(err, ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
	dir := filepath.Join(root, "dir.png")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Issue(dir, 0, time.Now()); !errors.Is(err, ErrNotExist) {
		t.Fatalf("want ErrNotExist for dir, got %v", err)
	}
}

func TestIssueEnforcesMaxBytes(t *testing.T) {
	cfg, root := testConfig(t, func(c *config.PullFileConfig) { c.MaxBytes = 10 })
	mgr := NewManager(cfg, nil)
	path := writePNG(t, root, "big.png")
	if _, err := mgr.Issue(path, 0, time.Now()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	// Caller-provided max_bytes must not raise the configured cap.
	if _, err := mgr.Issue(path, 9999, time.Now()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("caller max_bytes should still honor config cap, got %v", err)
	}
	// A smaller caller cap can tighten the limit.
	if _, err := mgr.Issue(path, 1, time.Now()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge with tighter caller cap, got %v", err)
	}
}

func TestIssueExtensionWhitelistCaseInsensitive(t *testing.T) {
	cfg, root := testConfig(t, func(c *config.PullFileConfig) {
		c.AllowedExtensions = []string{".jpg", ".bmp"}
	})
	mgr := NewManager(cfg, nil)
	path := writePNG(t, root, "progress.png")
	if _, err := mgr.Issue(path, 0, time.Now()); !errors.Is(err, ErrTypeNotAllowed) {
		t.Fatalf("want ErrTypeNotAllowed, got %v", err)
	}
	// Extension matching is case-insensitive.
	upper := writePNG(t, root, "PROGRESS.PNG")
	if _, err := mgr.Issue(upper, 0, time.Now()); !errors.Is(err, ErrTypeNotAllowed) {
		t.Fatalf("want ErrTypeNotAllowed for .PNG, got %v", err)
	}
	jpg := filepath.Join(root, "photo.jpg")
	if err := os.WriteFile(jpg, []byte("jpg-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Issue(jpg, 0, time.Now()); err != nil {
		t.Fatalf("jpg should be allowed: %v", err)
	}
}

func TestResolveExpired(t *testing.T) {
	cfg, root := testConfig(t, func(c *config.PullFileConfig) { c.TTLSeconds = 1 })
	mgr := NewManager(cfg, nil)
	path := writePNG(t, root, "a.png")
	now := time.Now()
	dl, err := mgr.Issue(path, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Resolve(dl.Token, now.Add(2*time.Second)); !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

func TestResolveConsumesDownloadLimit(t *testing.T) {
	cfg, root := testConfig(t, func(c *config.PullFileConfig) { c.MaxDownloads = 1; c.TTLSeconds = 60 })
	mgr := NewManager(cfg, nil)
	path := writePNG(t, root, "a.png")
	now := time.Now()
	dl, err := mgr.Issue(path, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Resolve(dl.Token, now.Add(1*time.Second)); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if _, err := mgr.Resolve(dl.Token, now.Add(2*time.Second)); !errors.Is(err, ErrConsumed) && !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want consumed/invalid on second resolve, got %v", err)
	}
}

func TestResolveUnlimitedDownloadsWithinTTL(t *testing.T) {
	cfg, root := testConfig(t, nil) // max_downloads defaults to 0 = unlimited
	mgr := NewManager(cfg, nil)
	path := writePNG(t, root, "a.png")
	now := time.Now()
	dl, err := mgr.Issue(path, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := mgr.Resolve(dl.Token, now.Add(1*time.Second)); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
	}
}

func TestResolveRejectsTamperedToken(t *testing.T) {
	cfg, root := testConfig(t, nil)
	mgr := NewManager(cfg, nil)
	path := writePNG(t, root, "a.png")
	now := time.Now()
	dl, err := mgr.Issue(path, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(dl.Token)
	if err != nil {
		t.Fatal(err)
	}
	// Flip one byte in the path portion.
	pathStart := tokenHeaderLen
	tampered := append([]byte(nil), payload...)
	if tampered[pathStart+1] == 'a' {
		tampered[pathStart+1] = 'b'
	} else {
		tampered[pathStart+1] = 'a'
	}
	badToken := base64.RawURLEncoding.EncodeToString(tampered)
	if _, err := mgr.Resolve(badToken, now.Add(1*time.Second)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want ErrInvalidToken for tampered token, got %v", err)
	}
}

func TestResolveRejectsGarbageToken(t *testing.T) {
	cfg, root := testConfig(t, nil)
	mgr := NewManager(cfg, nil)
	_ = writePNG(t, root, "a.png")
	if _, err := mgr.Resolve("not-a-valid-token!!!!", time.Now()); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want ErrInvalidToken, got %v", err)
	}
}

func TestResolveRejectsMissingFileAfterIssue(t *testing.T) {
	cfg, root := testConfig(t, nil)
	mgr := NewManager(cfg, nil)
	path := writePNG(t, root, "a.png")
	now := time.Now()
	dl, err := mgr.Issue(path, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Resolve(dl.Token, now.Add(1*time.Second)); !errors.Is(err, ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
}

func TestResolveRejectsPathOutsideAllowedRoots(t *testing.T) {
	cfg, root := testConfig(t, nil)
	// Startup directory is root, allowed roots are root; issue a file outside.
	outside := t.TempDir()
	path := writePNG(t, outside, "a.png")
	mgr := NewManager(cfg, nil)
	_ = root
	dl, err := mgr.Issue(path, 0, time.Now())
	if err != nil {
		// Issue does not enforce allowed roots; only resolve does.
		t.Fatalf("issue should not enforce roots: %v", err)
	}
	if _, err := mgr.Resolve(dl.Token, time.Now().Add(1*time.Second)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want ErrInvalidToken for outside root, got %v", err)
	}
}
