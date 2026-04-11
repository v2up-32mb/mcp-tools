package security

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrPathOutsideAllowedRoots = errors.New("path outside allowed roots")

func ResolvePath(path string, allowedRoots []string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("abs path: %w", err)
	}
	clean := filepath.Clean(abs)
	resolved, err := evalWithMissingLeaf(clean)
	if err != nil {
		return "", err
	}
	for _, root := range allowedRoots {
		canonicalRoot, err := evalWithMissingLeaf(root)
		if err != nil {
			canonicalRoot = filepath.Clean(root)
		}
		rel, err := filepath.Rel(canonicalRoot, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return resolved, nil
		}
	}
	return "", ErrPathOutsideAllowedRoots
}

func RequireAllowedWorkdir(path string, allowedRoots []string) (string, error) {
	resolved, err := ResolvePath(path, allowedRoots)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workdir must be directory")
	}
	return resolved, nil
}

func evalWithMissingLeaf(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("eval symlinks: %w", err)
	}
	parent := filepath.Dir(path)
	resolvedParent, parentErr := filepath.EvalSymlinks(parent)
	if parentErr != nil {
		if errors.Is(parentErr, os.ErrNotExist) {
			return filepath.Clean(path), nil
		}
		return "", fmt.Errorf("eval parent symlinks: %w", parentErr)
	}
	return filepath.Join(resolvedParent, filepath.Base(path)), nil
}
