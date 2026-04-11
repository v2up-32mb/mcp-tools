package git

import "testing"

func TestRepoRelativePathsRejectTraversal(t *testing.T) {
	_, err := repoRelativePaths([]any{"../outside"})
	if err == nil {
		t.Fatal("expected traversal path to be rejected")
	}
}

func TestRepoRelativePathsAllowLocalPaths(t *testing.T) {
	got, err := repoRelativePaths([]any{".", "subdir/file.go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("unexpected paths: %#v", got)
	}
}
