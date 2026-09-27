// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build unix

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

func inPlace(p proto.FileEditPayload) proto.FileEditPayload {
	p.InPlace = true
	return p
}

func TestEditInPlaceKeepsTheSameFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.conf")
	writeTestFile(t, path, "port=80\nname=web\n", 0o640)
	before, _ := os.Stat(path)

	res, err := NewFileManager().(fileEditor).Edit(context.Background(), inPlace(editReplace(path, "port=80", "port=8080")))
	if err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, path); got != "port=8080\nname=web\n" {
		t.Fatalf("content = %q", got)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) {
		t.Fatal("an in-place edit replaced the file")
	}
	if after.Mode().Perm() != 0o640 || !res.InPlace || !res.Verified || res.NewSHA256 != sha256Hex([]byte("port=8080\nname=web\n")) {
		t.Fatalf("mode %v, res %+v", after.Mode(), res)
	}
	assertNoTempFiles(t, dir)
}

func TestEditInPlaceGrowsAndShrinksExactly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	writeTestFile(t, path, "short\n", 0o644)
	long := strings.Repeat("0123456789", 5000) + "\n"
	for _, want := range []string{long, "tiny\n", "", "x"} {
		cur := readTestFile(t, path)
		p := proto.FileEditPayload{Path: path, Replace: true, Content: []byte(want), ContentSHA256: sha256Hex([]byte(want)), BaseSHA256: sha256Hex([]byte(cur)), InPlace: true}
		if _, err := NewFileManager().(fileEditor).Edit(context.Background(), p); err != nil {
			t.Fatalf("write %d bytes in place: %v", len(want), err)
		}
		if got := readTestFile(t, path); got != want {
			t.Fatalf("after writing %d bytes the file holds %d bytes", len(want), len(got))
		}
	}
}

func TestEditInPlaceRefusesWhenFileChangesMidEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	writeTestFile(t, path, "a\n", 0o644)
	testHookBeforeEditInstall = func() {
		if err := os.WriteFile(path, []byte("written elsewhere\n"), 0o644); err != nil {
			t.Error(err)
		}
	}
	defer func() { testHookBeforeEditInstall = nil }()
	_, err := NewFileManager().(fileEditor).Edit(context.Background(), inPlace(editReplace(path, "a", "b")))
	if editErrCode(t, err) != "edit_conflict" {
		t.Fatalf("err = %v", err)
	}
	if got := readTestFile(t, path); got != "written elsewhere\n" {
		t.Fatalf("the concurrent write was clobbered: %q", got)
	}
}

func TestEditInPlaceWritesTheOriginalBackWhenAWriteGoesWrong(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	writeTestFile(t, path, "original\n", 0o644)
	testHookBeforeInPlaceVerify = func(w *os.File) {
		testHookBeforeInPlaceVerify = nil // only the first (new content) write goes wrong
		_, _ = w.WriteAt([]byte("#"), 0)
	}
	defer func() { testHookBeforeInPlaceVerify = nil }()
	_, err := NewFileManager().(fileEditor).Edit(context.Background(), inPlace(editReplace(path, "original", "changed")))
	if editErrCode(t, err) != "write_failed" || !strings.Contains(err.Error(), "written back") {
		t.Fatalf("err = %v", err)
	}
	if got := readTestFile(t, path); got != "original\n" {
		t.Fatalf("the original was not restored: %q", got)
	}
}

func TestEditInPlaceReportsAWriteItCouldNotUndo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	writeTestFile(t, path, "original\n", 0o644)
	testHookBeforeInPlaceVerify = func(w *os.File) { _, _ = w.WriteAt([]byte("#"), 0) }
	defer func() { testHookBeforeInPlaceVerify = nil }()
	_, err := NewFileManager().(fileEditor).Edit(context.Background(), inPlace(editReplace(path, "original", "changed")))
	if editErrCode(t, err) != "write_incomplete" || !strings.Contains(err.Error(), "--undo --force") {
		t.Fatalf("err = %v", err)
	}
}

func TestEditInPlaceStillRefusesHardLinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a")
	writeTestFile(t, path, "x\n", 0o644)
	if err := os.Link(path, filepath.Join(dir, "b")); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	_, err := NewFileManager().(fileEditor).Edit(context.Background(), inPlace(editReplace(path, "x", "y")))
	if editErrCode(t, err) != "hard_linked" || !strings.Contains(err.Error(), "other names") {
		t.Fatalf("err = %v", err)
	}
	if got := readTestFile(t, path); got != "x\n" {
		t.Fatalf("file changed: %q", got)
	}
}

func TestEditDryRunCanReturnTheCurrentContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	writeTestFile(t, path, "a\n", 0o600)
	p := editReplace(path, "a", "b")
	p.DryRun, p.ReturnOriginal, p.InPlace = true, true, true
	res, err := NewFileManager().(fileEditor).Edit(context.Background(), p)
	if err != nil || string(res.Original) != "a\n" || res.InPlace || res.Verified {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if got := readTestFile(t, path); got != "a\n" {
		t.Fatalf("a dry run changed the file: %q", got)
	}
}

// A file the agent may write in a folder it may not write: installing a new
// file is impossible there, writing in place is not.
func TestEditInFolderTheAgentCannotWriteNeedsInPlace(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may write any folder")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	writeTestFile(t, path, "a\n", 0o644)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	_, err := NewFileManager().(fileEditor).Edit(context.Background(), editReplace(path, "a", "b"))
	if editErrCode(t, err) != "cannot_replace" || !strings.Contains(err.Error(), "--in-place") {
		t.Fatalf("err = %v", err)
	}
	if got := readTestFile(t, path); got != "a\n" {
		t.Fatalf("file changed: %q", got)
	}
	if _, err := NewFileManager().(fileEditor).Edit(context.Background(), inPlace(editReplace(path, "a", "b"))); err != nil {
		t.Fatalf("in place: %v", err)
	}
	if got := readTestFile(t, path); got != "b\n" {
		t.Fatalf("content = %q", got)
	}
}
