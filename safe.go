package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// root is the absolute, symlink-resolved folder the server is allowed to serve.
var root string

// setRoot resolves dir to an absolute path and stores it as the confinement root.
func setRoot(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	// Resolve symlinks so the prefix check below can't be fooled by a symlinked root.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("root %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("root %q is not a directory", dir)
	}
	root = abs
	return nil
}

// resolve turns a caller-supplied relative path into an absolute path and
// verifies it stays within root. An empty path means the root itself.
func resolve(rel string) (string, error) {
	// Reject absolute inputs outright; everything is relative to root.
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path must be relative to the served folder: %q", rel)
	}
	abs := filepath.Clean(filepath.Join(root, rel))

	// Resolve symlinks where the target exists, so links can't escape root.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	relToRoot, err := filepath.Rel(root, abs)
	if err != nil {
		return "", fmt.Errorf("invalid path: %q", rel)
	}
	if relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the served folder: %q", rel)
	}
	// Refuse hidden paths (dotfiles / dot-dirs like .git) at the one chokepoint
	// every read, write, and edit passes through — so direct access matches what
	// listings and search already exclude, and a write can't reach into the
	// vault's own .git and corrupt it.
	for _, part := range strings.Split(relToRoot, string(filepath.Separator)) {
		if part != "." && strings.HasPrefix(part, ".") {
			return "", fmt.Errorf("hidden path is not served: %q", rel)
		}
	}
	return abs, nil
}

// relPath renders an absolute path back as a path relative to root, for output.
func relPath(abs string) string {
	r, err := filepath.Rel(root, abs)
	if err != nil {
		return abs
	}
	return r
}

// looksBinary reports whether b appears to be binary (contains a NUL byte).
func looksBinary(b []byte) bool {
	return bytes.IndexByte(b, 0) != -1
}

// isHidden reports whether a file or directory name is a dotfile (starts with
// "."). Hidden entries are excluded from every listing, search, and the link
// graph — which, among other things, keeps the vault's own .git tree out of
// results. In a recursive walk, a hidden directory should be pruned entirely
// (filepath.SkipDir), not just skipped as one entry.
func isHidden(name string) bool {
	return strings.HasPrefix(name, ".")
}
