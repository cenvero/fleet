// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// TestCatRemoteFileAbortsWithoutEOF confirms CatRemoteFile refuses to stream
// forever from a malicious agent that returns data but never sets EOF. Without
// the iteration/byte cap this would loop until the controller ran out of time
// or the writer's disk; with it, the loop aborts with an error.
func TestCatRemoteFileAbortsWithoutEOF(t *testing.T) {
	t.Parallel()
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

	_, err := rig.app.CatRemoteFile("loopback", "/whatever", io.Discard)
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

	// recursive upload then recursive download round-trips the tree
	src := t.TempDir()
	syncWrite(t, filepath.Join(src, "a.txt"), "A")
	syncWrite(t, filepath.Join(src, "d", "b.txt"), "B")
	remoteDest := filepath.Join(t.TempDir(), "up")
	if n, err := rig.app.UploadDir("loopback", src, remoteDest, FileTransferOptions{}, nil); err != nil || n != 2 {
		t.Fatalf("UploadDir n=%d err=%v", n, err)
	}
	syncAssertFile(t, filepath.Join(remoteDest, "a.txt"), "A")
	syncAssertFile(t, filepath.Join(remoteDest, "d", "b.txt"), "B")

	dest := t.TempDir()
	if n, err := rig.app.DownloadDir("loopback", remoteDest, dest, FileTransferOptions{}, nil); err != nil || n != 2 {
		t.Fatalf("DownloadDir n=%d err=%v", n, err)
	}
	syncAssertFile(t, filepath.Join(dest, "a.txt"), "A")
	syncAssertFile(t, filepath.Join(dest, "d", "b.txt"), "B")
}
