package security

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePathRejectsSymlinkEscapeWithMissingDescendants(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(link, "new", "file.txt")
	_, err := ResolvePath(target, []string{root})
	if !errors.Is(err, ErrPathOutsideAllowedRoots) {
		t.Fatalf("expected ErrPathOutsideAllowedRoots, got %v", err)
	}
}

func TestResolvePathAllowsMissingDescendantsInsideAllowedRoot(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "nested", "child", "file.txt")
	resolved, err := ResolvePath(target, []string{root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved != filepath.Clean(target) {
		t.Fatalf("expected %q, got %q", filepath.Clean(target), resolved)
	}
}

func TestResolvePathNoFollowFinalKeepsFinalSymlinkPath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(target, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	resolved, err := ResolvePathNoFollowFinal(link, []string{root})
	if err != nil {
		t.Fatalf("ResolvePathNoFollowFinal failed: %v", err)
	}
	if resolved != filepath.Clean(link) {
		t.Fatalf("expected final symlink path %q, got %q", filepath.Clean(link), resolved)
	}
}

func TestResolvePathNoFollowFinalRejectsParentSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	linkDir := filepath.Join(root, "link-dir")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err := ResolvePathNoFollowFinal(filepath.Join(linkDir, "target.txt"), []string{root})
	if !errors.Is(err, ErrPathOutsideAllowedRoots) {
		t.Fatalf("expected ErrPathOutsideAllowedRoots, got %v", err)
	}
}
