// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build linux

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/cenvero/fleet/pkg/proto"
)

// A file bind-mounted over another (as containers do with config files)
// cannot be replaced by renaming a new file over it, but can be written in
// place — which changes the mounted file.
func TestEditBindMountedFileNeedsInPlace(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "host.conf")
	writeTestFile(t, src, "listen 80\n", 0o644)
	sub := filepath.Join(dir, "container")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(sub, "app.conf")
	writeTestFile(t, target, "", 0o644)
	if err := unix.Mount(src, target, "", unix.MS_BIND, ""); err != nil {
		t.Skipf("cannot bind-mount here: %v", err)
	}
	defer unix.Unmount(target, unix.MNT_DETACH)

	_, err := NewFileManager().(fileEditor).Edit(context.Background(), editReplace(target, "80", "8080"))
	if editErrCode(t, err) != "cannot_replace" || !strings.Contains(err.Error(), "mount point") || !strings.Contains(err.Error(), "--in-place") {
		t.Fatalf("err = %v", err)
	}
	if got := readTestFile(t, src); got != "listen 80\n" {
		t.Fatalf("file changed: %q", got)
	}
	assertNoTempFiles(t, sub)

	res, err := NewFileManager().(fileEditor).Edit(context.Background(), inPlace(editReplace(target, "80", "8080")))
	if err != nil {
		t.Fatalf("in place: %v", err)
	}
	if got := readTestFile(t, src); got != "listen 8080\n" || !res.InPlace {
		t.Fatalf("mounted file = %q, res %+v", got, res)
	}
	assertNoTempFiles(t, sub)
}

// On a nearly full filesystem, an in-place write that needs more room is
// refused before the first byte changes.
func TestEditInPlaceRefusesWhenTheDiskIsFull(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mount("tmpfs", dir, "tmpfs", 0, "size=64k"); err != nil {
		t.Skipf("cannot mount a small tmpfs here: %v", err)
	}
	defer unix.Unmount(dir, unix.MNT_DETACH)
	path := filepath.Join(dir, "f")
	orig := strings.Repeat("a", 40<<10)
	writeTestFile(t, path, orig, 0o644)
	next := strings.Repeat("b", 100<<10)
	p := proto.FileEditPayload{Path: path, Replace: true, Content: []byte(next), ContentSHA256: sha256Hex([]byte(next)), InPlace: true}
	_, err := NewFileManager().(fileEditor).Edit(context.Background(), p)
	t.Logf("full disk: %v", err)
	if code := editErrCode(t, err); code != "no_space" && code != "write_failed" {
		t.Fatalf("err = %v", err)
	}
	if got := readTestFile(t, path); got != orig {
		t.Fatalf("file changed on a full disk (%d bytes)", len(got))
	}
}
