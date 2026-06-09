// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"path/filepath"
	"testing"
)

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
