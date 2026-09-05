// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCatRemoteFileAbortsWithoutEOF confirms CatRemoteFile refuses to stream
// forever from a malicious agent that returns data but never sets EOF. Without
// the iteration/byte cap this would loop until the controller ran out of time
// or the writer's disk; with it, the loop aborts with an error.
func TestCatRemoteFileAbortsWithoutEOF(t *testing.T) {
	// NOT t.Parallel: this test mutates the package-global maxCatRemoteBytes,
	// which CatRemoteFile reads — running it concurrently with other CatRemoteFile
	// tests (e.g. TestFileOpsStatCatAndRecursive) is a data race. Keeping it serial
	// guarantees no other test runs while the global is lowered/restored.
	rig := newTransferRig(t)
	go func() {
		for range rig.errCh {
		}
	}()
	// Lower the byte ceiling so the abort fires after a few round-trips instead of
	// the hundreds the production 4 GiB cap would need (an envelope can't carry
	// more than ~11 MiB, so each chunk is small). Restore it after.
	prev := maxCatRemoteBytes
	maxCatRemoteBytes = 20 * 1024 * 1024 // 20 MiB
	defer func() { maxCatRemoteBytes = prev }()
	// Every Read returns ~8 MiB (under the envelope ceiling) with EOF never set.
	rig.fileMgr.setNeverEOF(bytes.Repeat([]byte("A"), 8*1024*1024))

	remoteFile := filepath.Join(t.TempDir(), "whatever")
	syncWrite(t, remoteFile, "seed")
	_, err := rig.app.CatRemoteFile("loopback", remoteFile, io.Discard)
	if err == nil {
		t.Fatal("CatRemoteFile must abort when the agent never sets EOF")
	}
	if !strings.Contains(err.Error(), "aborting remote read") {
		t.Fatalf("expected an abort error, got: %v", err)
	}
}

func TestFileOpsStatCatAndRecursive(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	go func() {
		for range rig.errCh {
		}
	}()

	// stat + cat
	remoteDir := t.TempDir()
	hello := filepath.Join(remoteDir, "hello.txt")
	syncWrite(t, hello, "hello world")
	st, err := rig.app.StatRemoteFile("loopback", hello)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Entry.Size != int64(len("hello world")) {
		t.Fatalf("stat size = %d", st.Entry.Size)
	}
	var buf bytes.Buffer
	if _, err := rig.app.CatRemoteFile("loopback", hello, &buf); err != nil {
		t.Fatalf("cat: %v", err)
	}
	if buf.String() != "hello world" {
		t.Fatalf("cat = %q", buf.String())
	}

	// recursive upload then recursive download round-trips hidden files and empty directories
	src := t.TempDir()
	syncWrite(t, filepath.Join(src, "a.txt"), "A")
	syncWrite(t, filepath.Join(src, "d", "b.txt"), "B")
	syncWrite(t, filepath.Join(src, ".hidden"), "H")
	syncWrite(t, filepath.Join(src, ".git", "config"), "G")
	if err := os.MkdirAll(filepath.Join(src, "empty", "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	remoteDest := filepath.Join(t.TempDir(), "up")
	if n, err := rig.app.UploadDir("loopback", src, remoteDest, FileTransferOptions{}, nil); err != nil || n != 4 {
		t.Fatalf("UploadDir n=%d err=%v", n, err)
	}
	for rel, want := range map[string]string{"a.txt": "A", "d/b.txt": "B", ".hidden": "H", ".git/config": "G"} {
		syncAssertFile(t, filepath.Join(remoteDest, filepath.FromSlash(rel)), want)
	}
	if info, err := os.Stat(filepath.Join(remoteDest, "empty", "nested")); err != nil || !info.IsDir() {
		t.Fatalf("uploaded empty directory missing: info=%v err=%v", info, err)
	}

	dest := t.TempDir()
	if n, err := rig.app.DownloadDir("loopback", remoteDest, dest, FileTransferOptions{}, nil); err != nil || n != 4 {
		t.Fatalf("DownloadDir n=%d err=%v", n, err)
	}
	for rel, want := range map[string]string{"a.txt": "A", "d/b.txt": "B", ".hidden": "H", ".git/config": "G"} {
		syncAssertFile(t, filepath.Join(dest, filepath.FromSlash(rel)), want)
	}
	if info, err := os.Stat(filepath.Join(dest, "empty", "nested")); err != nil || !info.IsDir() {
		t.Fatalf("downloaded empty directory missing: info=%v err=%v", info, err)
	}
}

func TestRecursiveTransfersRefuseSymlinks(t *testing.T) {
	rig := newTransferRig(t)
	go func() {
		for range rig.errCh {
		}
	}()

	localSrc := t.TempDir()
	syncWrite(t, filepath.Join(localSrc, "ok.txt"), "ok")
	if err := os.Symlink(filepath.Join(localSrc, "ok.txt"), filepath.Join(localSrc, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := rig.app.UploadDir("loopback", localSrc, filepath.Join(t.TempDir(), "upload"), FileTransferOptions{}, nil); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("UploadDir symlink error = %v", err)
	}

	remoteSrc := t.TempDir()
	syncWrite(t, filepath.Join(remoteSrc, "ok.txt"), "ok")
	if err := os.Symlink(filepath.Join(remoteSrc, "ok.txt"), filepath.Join(remoteSrc, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.app.DownloadDir("loopback", remoteSrc, t.TempDir(), FileTransferOptions{}, nil); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("DownloadDir symlink error = %v", err)
	}
}

func TestUploadDirValidatesDestinationBeforeRemoteMutation(t *testing.T) {
	rig := newTransferRig(t)
	server, err := rig.app.GetServer("loopback")
	if err != nil {
		t.Fatal(err)
	}
	server.Observed.OS = "windows"
	if err := rig.app.SaveServer(server); err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	syncWrite(t, filepath.Join(src, "CON"), "reserved")
	if _, err := rig.app.UploadDir("loopback", src, `C:\dest`, FileTransferOptions{}, nil); err == nil {
		t.Fatal("Windows-invalid source tree was accepted")
	}

	validSrc := t.TempDir()
	syncWrite(t, filepath.Join(validSrc, "ok.txt"), "ok")
	if _, err := rig.app.UploadDir("loopback", validSrc, `C:\CON`, FileTransferOptions{}, nil); err == nil {
		t.Fatal("Windows-invalid top-level destination directory was accepted")
	}
	localFile := filepath.Join(validSrc, "ok.txt")
	if _, err := rig.app.UploadFile("loopback", localFile, `C:\Data\CON`, FileTransferOptions{}, nil); err == nil {
		t.Fatal("Windows-invalid single-file destination was accepted")
	}
	if got := rig.fileMgr.mkdirCount(); got != 0 {
		t.Fatalf("destination validation ran after %d remote mkdir call(s)", got)
	}
	if got := rig.fileMgr.writeCount(); got != 0 {
		t.Fatalf("destination validation ran after %d remote write call(s)", got)
	}
}

func TestDownloadDirRefusesRemoteSpecialFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-domain socket fixture is unavailable on Windows")
	}
	rig := newTransferRig(t)
	go func() {
		for range rig.errCh {
		}
	}()
	remoteSrc := t.TempDir()
	socketPath := filepath.Join(remoteSrc, "service.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Skipf("Unix-domain sockets unavailable: %v", err)
	}
	defer listener.Close()
	if _, err := rig.app.DownloadDir("loopback", remoteSrc, t.TempDir(), FileTransferOptions{}, nil); err == nil || !strings.Contains(err.Error(), "special") {
		t.Fatalf("DownloadDir special-file error = %v", err)
	}
}

func TestDownloadFileValidatesLocalDestinationBeforeConnecting(t *testing.T) {
	if NativePathStyle().IsWindows() {
		t.Skip("fixture uses a POSIX-valid backslash name")
	}
	rig := newTransferRig(t)
	badLocal := filepath.Join(t.TempDir(), `bad\name.txt`)
	_, err := rig.app.DownloadFile("loopback", "/does-not-matter", badLocal, FileTransferOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid destination path") {
		t.Fatalf("DownloadFile destination error = %v", err)
	}
	select {
	case serveErr := <-rig.errCh:
		t.Fatalf("download connected before destination validation: %v", serveErr)
	default:
	}
}
