// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build linux

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestEditPreservesExtendedAttributes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	writeTestFile(t, path, "a\n", 0o644)
	if err := unix.Setxattr(path, "user.fleet-test", []byte("keep-me"), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) {
			t.Skipf("user xattrs unsupported here: %v", err)
		}
		t.Fatal(err)
	}
	res, err := NewFileManager().(fileEditor).Edit(context.Background(), editReplace(path, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := unix.Getxattr(path, "user.fleet-test", buf)
	if err != nil || string(buf[:n]) != "keep-me" {
		t.Fatalf("xattr after edit = %q, %v", buf[:n], err)
	}
	found := false
	for _, k := range res.Preserved {
		found = found || k == "xattrs"
	}
	if !found {
		t.Fatalf("preserved = %v, want xattrs", res.Preserved)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
