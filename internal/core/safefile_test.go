// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicLocalFileFinalSymlinkSwapDoesNotClobberTarget(t *testing.T) {
	base := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(base, "out.txt")
	out, err := OpenAtomicLocalFile(dst, "race-safe-id", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Abort()
	if _, err := out.File().Write([]byte("result")); err != nil {
		t.Fatal(err)
	}
	// Swap the final component after the sidecar is open but before commit.
	if err := os.Symlink(victim, dst); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := out.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "original" {
		t.Fatalf("final symlink target was clobbered: %q", got)
	}
	if got, _ := os.ReadFile(dst); string(got) != "result" {
		t.Fatalf("installed content mismatch: %q", got)
	}
}

func TestAtomicLocalFileIntermediateSymlinkSwapFailsClosed(t *testing.T) {
	base := t.TempDir()
	sub := filepath.Join(base, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	out, err := OpenAtomicLocalFileUnder(base, filepath.Join("sub", "out.txt"), "race-safe-id", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Abort()
	if _, err := out.File().Write([]byte("result")); err != nil {
		t.Fatal(err)
	}
	// Replace an intermediate directory after the descriptor/sidecar is open.
	held := filepath.Join(base, "held")
	if err := os.Rename(sub, held); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, sub); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := out.Commit(); err == nil {
		t.Fatal("commit should fail closed after an intermediate symlink escapes the root")
	}
	if _, err := os.Stat(filepath.Join(outside, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("atomic write escaped through swapped intermediate symlink: %v", err)
	}
}

func TestCopyLocalSourcesRejectSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := CopyLocalFileAtomic(link, filepath.Join(dir, "copy"), 0o600); err == nil {
		t.Fatal("CopyLocalFileAtomic accepted a symlink source")
	}

	tree := filepath.Join(dir, "tree")
	if err := os.Mkdir(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(tree, "escape")); err != nil {
		t.Skipf("symlinks unavailable in tree: %v", err)
	}
	if err := CopyLocalTreeAtomic(tree, filepath.Join(dir, "tree-copy")); err == nil {
		t.Fatal("CopyLocalTreeAtomic accepted a symlink member")
	}
}
