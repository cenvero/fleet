// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"path/filepath"
	"testing"
)

func TestSafeLocalJoin(t *testing.T) {
	t.Parallel()
	base := filepath.Join(t.TempDir(), "replica")

	good := []string{"a.txt", "sub/b.txt", "deep/nested/c.bin", "weird name.txt"}
	for _, rel := range good {
		got, err := SafeLocalJoin(base, rel)
		if err != nil {
			t.Fatalf("SafeLocalJoin(%q) errored: %v", rel, err)
		}
		within, err := filepath.Rel(base, got)
		if err != nil || within == ".." || filepath.IsAbs(within) {
			t.Fatalf("SafeLocalJoin(%q) = %q escaped base", rel, got)
		}
	}

	// Anything a malicious agent could send to escape the target directory.
	bad := []string{
		"", "..", "../escape", "../../etc/passwd", "sub/../../x",
		"/etc/passwd", "/etc/cron.d/evil", "a/../../b",
	}
	for _, rel := range bad {
		if _, err := SafeLocalJoin(base, rel); err == nil {
			t.Fatalf("SafeLocalJoin(%q) should have been refused", rel)
		}
	}
}

func TestSafeComponent(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"a.txt", "file", "weird name", ".hidden"} {
		if !SafeComponent(ok) {
			t.Fatalf("SafeComponent(%q) = false, want true", ok)
		}
	}
	for _, no := range []string{"", ".", "..", "a/b", "../x", "/etc"} {
		if SafeComponent(no) {
			t.Fatalf("SafeComponent(%q) = true, want false", no)
		}
	}
}
