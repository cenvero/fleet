// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SafeLocalJoin joins base with rel, where rel is an UNTRUSTED relative path
// supplied by a managed agent (e.g. a remote directory listing). It rejects
// absolute paths and any path that would escape base via "..", so a compromised
// or malicious agent cannot make the controller read or write outside the
// intended directory. The agent re-validates its own side; this protects the
// controller's local filesystem.
func SafeLocalJoin(base, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("empty path from server")
	}
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("refusing absolute path %q from server", rel)
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing path %q from server: escapes target directory", rel)
	}
	joined := filepath.Join(base, clean)
	// Defense in depth: confirm the result really is within base.
	rp, err := filepath.Rel(base, joined)
	if err != nil || rp == ".." || strings.HasPrefix(rp, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing path %q from server: escapes target directory", rel)
	}
	return joined, nil
}

// SafeComponent reports whether name is a single safe path component supplied by
// an agent (no separators, not "." or ".."). Used to vet remote file names
// before they are joined into a local path.
func SafeComponent(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsRune(name, '/') && !strings.ContainsRune(name, filepath.Separator)
}

// safeRel reports whether an untrusted relative path is safe (not absolute, no
// "..", non-empty).
func safeRel(rel string) bool {
	if strings.TrimSpace(rel) == "" {
		return false
	}
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) {
		return false
	}
	return clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}
