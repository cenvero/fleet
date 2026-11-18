// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package testutil

import (
	"path/filepath"
	"testing"
)

// CanonicalTempDir returns a new t.TempDir() with every symlink in its path
// resolved. The agent reports the canonical form of the paths it is handed, so
// a test that compares those with paths it built itself must build them from a
// canonical root: on macOS t.TempDir() lies under /var, a symlink to
// /private/var, and on Windows it can contain 8.3 short names.
func CanonicalTempDir(t testing.TB) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}
