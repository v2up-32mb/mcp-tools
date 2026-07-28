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
	resolved, err := canonicalizePathAllowMissing(path)
	if err != nil {
		return "", err
	}
	for _, root := range allowedRoots {
		canonicalRoot, err := canonicalizePathAllowMissing(root)
		if err != nil {
			if absRoot, absErr := filepath.Abs(root); absErr == nil {
				canonicalRoot = filepath.Clean(absRoot)
			} else {
				canonicalRoot = filepath.Clean(root)
			}
		}
		rel, err := filepath.Rel(canonicalRoot, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return resolved, nil
		}
	}
	return "", ErrPathOutsideAllowedRoots
}

func ResolvePathUnsafe(path string) (string, error) {
	return canonicalizePathAllowMissing(path)
}

func ResolvePathNoFollowFinal(path string, allowedRoots []string) (string, error) {
	resolved, err := canonicalizePathNoFollowFinal(path)
	if err != nil {
		return "", err
	}
	for _, root := range allowedRoots {
		canonicalRoot, err := canonicalizePathAllowMissing(root)
		if err != nil {
			if absRoot, absErr := filepath.Abs(root); absErr == nil {
				canonicalRoot = filepath.Clean(absRoot)
			} else {
				canonicalRoot = filepath.Clean(root)
			}
		}
		rel, err := filepath.Rel(canonicalRoot, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return resolved, nil
		}
	}
	return "", ErrPathOutsideAllowedRoots
}

func ResolvePathUnsafeNoFollowFinal(path string) (string, error) {
	return canonicalizePathNoFollowFinal(path)
}

func canonicalizePathAllowMissing(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("abs path: %w", err)
	}
	return evalWithMissingPath(filepath.Clean(abs))
}

func canonicalizePathNoFollowFinal(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("abs path: %w", err)
	}
	clean := filepath.Clean(abs)
	parent := filepath.Dir(clean)
	if parent == clean {
		return clean, nil
	}
	resolvedParent, err := evalWithMissingPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(resolvedParent, filepath.Base(clean))), nil
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

func RequireExistingWorkdir(path string) (string, error) {
	resolved, err := ResolvePathUnsafe(path)
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

func evalWithMissingPath(path string) (string, error) {
	current := filepath.Clean(path)
	missing := make([]string, 0, 4)
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("eval symlinks: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			resolved := current
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}
