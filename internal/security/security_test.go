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
