// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build unix

package tui

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestLocalEditSaveKeepsWhatAWriteWould covers what replacing a file could
// change that writing it would not: other hard links, set-id bits, owner.
func TestLocalEditSaveKeepsWhatAWriteWould(t *testing.T) {
	t.Parallel()

	t.Run("hard links", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		p := filepath.Join(dir, "a.txt")
		other := filepath.Join(dir, "b.txt")
		if err := os.WriteFile(p, []byte("v1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(p, other); err != nil {
			t.Skip("hard links unavailable:", err)
		}
		err := saveFileFromEdit(nil, "", p, []byte("v2\n"), []byte("v1\n"), false)
		if err == nil || !strings.Contains(err.Error(), "hard links") {
			t.Fatalf("want a hard-link refusal, got %v", err)
		}
		for _, n := range []string{p, other} {
			if got, _ := os.ReadFile(n); string(got) != "v1\n" {
				t.Fatalf("%s = %q", n, got)
			}
		}
	})

	t.Run("set-id bits", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(t.TempDir(), "tool")
		if err := os.WriteFile(p, []byte("v1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		const mode = os.ModeSetuid | 0o750
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		if fi, _ := os.Stat(p); fi.Mode()&(os.ModeSetuid|os.ModePerm) != mode {
			t.Skip("this filesystem does not keep the setuid bit")
		}
		if err := saveFileFromEdit(nil, "", p, []byte("v2\n"), []byte("v1\n"), false); err != nil {
			t.Fatalf("save: %v", err)
		}
		if fi, _ := os.Stat(p); fi.Mode()&(os.ModeSetuid|os.ModePerm) != mode {
			t.Fatalf("mode %v, want %v", fi.Mode(), mode)
		}
	})

	// A new file takes the folder's group on macOS (and in a setgid folder on
	// Linux), so a file whose group differs from its folder's must keep its
	// own. No root needed: the folder is given another group of the user's.
	t.Run("group differs from folder", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		egid := os.Getegid()
		groups, _ := os.Getgroups()
		other := -1
		for _, g := range groups {
			if g != egid && os.Chown(dir, -1, g) == nil {
				other = g
				break
			}
		}
		if other == -1 {
			t.Skip("no second group to give the folder")
		}
		if err := os.Chmod(dir, os.ModeSetgid|0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "a.txt")
		if err := os.WriteFile(p, []byte("v1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(p, -1, egid); err != nil {
			t.Fatal(err)
		}
		if err := saveFileFromEdit(nil, "", p, []byte("v2\n"), []byte("v1\n"), false); err != nil {
			t.Fatalf("save: %v", err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if gid := int(fi.Sys().(*syscall.Stat_t).Gid); gid != egid {
			t.Fatalf("group %d after save, want %d (the folder's is %d)", gid, egid, other)
		}
		if got, _ := os.ReadFile(p); string(got) != "v2\n" {
			t.Fatalf("content %q", got)
		}
	})

	t.Run("owner", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() != 0 {
			t.Skip("needs root to chown")
		}
		p := filepath.Join(t.TempDir(), "theirs.conf")
		if err := os.WriteFile(p, []byte("v1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(p, 65534, 65534); err != nil {
			t.Skipf("chown: %v", err)
		}
		if err := saveFileFromEdit(nil, "", p, []byte("v2\n"), []byte("v1\n"), false); err != nil {
			t.Fatalf("save: %v", err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if st.Uid != 65534 || st.Gid != 65534 {
			t.Fatalf("owner %d:%d, want 65534:65534", st.Uid, st.Gid)
		}
	})
}
