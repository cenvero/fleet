// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAllowedFileRootsSandbox(t *testing.T) {
	root := t.TempDir()
	SetAllowedFileRoots([]string{root})
	defer SetAllowedFileRoots(nil)

	inside := filepath.Join(root, "sub", "f.txt")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, rerr := validateTransferPath(inside); rerr != nil {
		t.Fatalf("path within an allowed root should be permitted, got %v", rerr)
	}

	outside := filepath.Join(t.TempDir(), "g.txt")
	if err := os.WriteFile(outside, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, rerr := validateTransferPath(outside); rerr == nil {
		t.Fatalf("path outside the allowed roots should be rejected")
	}
	// A write target outside the roots is rejected too.
	if _, rerr := validateWriteTarget(filepath.Join(t.TempDir(), "new.txt")); rerr == nil {
		t.Fatalf("write target outside the allowed roots should be rejected")
	}
}

func TestReapStaleParts(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "a.txt.fleet-OLD.part")
	keep := filepath.Join(dir, "c.txt.fleet-KEEP.part")
	fresh := filepath.Join(dir, "b.txt.fleet-NEW.part")
	regular := filepath.Join(dir, "regular.txt")
	for _, p := range []string{old, keep, fresh, regular} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	twoDaysAgo := now.Add(-48 * time.Hour)
	if err := os.Chtimes(old, twoDaysAgo, twoDaysAgo); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(keep, twoDaysAgo, twoDaysAgo); err != nil { // old, but excluded
		t.Fatal(err)
	}

	reapStaleParts(dir, filepath.Base(keep), now)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("stale .part should have been reaped")
	}
	for _, p := range []string{keep, fresh, regular} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s should remain: %v", filepath.Base(p), err)
		}
	}
}
