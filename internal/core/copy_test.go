// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cenvero/fleet/internal/transport"
)

// TestServerToServerCopyMove exercises the relay copy/move between two servers
// (both backed by the in-memory agent's real filesystem in the rig).
func TestServerToServerCopyMove(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	go func() {
		for range rig.errCh {
		}
	}()
	if err := rig.app.AddServer(ServerRecord{
		Name: "loopback2", Address: "127.0.0.1", Port: 2222, Mode: transport.ModeDirect, User: "cenvero-agent",
	}); err != nil {
		t.Fatalf("AddServer loopback2: %v", err)
	}

	base := t.TempDir()

	// CopyFile: loopback:src -> loopback2:dst (relayed through the controller).
	src := filepath.Join(base, "src.txt")
	if err := os.WriteFile(src, []byte("server-to-server payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(base, "dst.txt")
	if _, err := rig.app.CopyFile("loopback", src, "loopback2", dst, FileTransferOptions{}, nil); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "server-to-server payload" {
		t.Fatalf("copied content = %q", b)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("copy must keep the source: %v", err)
	}

	// CopyDir: recursive tree.
	srcDir := filepath.Join(base, "tree")
	if err := os.MkdirAll(filepath.Join(srcDir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("A"), 0o644)
	_ = os.WriteFile(filepath.Join(srcDir, "sub", "b.txt"), []byte("B"), 0o644)
	dstDir := filepath.Join(base, "tree-copy")
	if n, err := rig.app.CopyDir("loopback", srcDir, "loopback2", dstDir, FileTransferOptions{}, nil); err != nil || n != 2 {
		t.Fatalf("CopyDir n=%d err=%v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dstDir, "sub", "b.txt")); string(b) != "B" {
		t.Fatalf("recursive copy wrong: %q", b)
	}

	// MoveFile within one server == rename (source removed).
	mvSrc := filepath.Join(base, "mv.txt")
	_ = os.WriteFile(mvSrc, []byte("M"), 0o644)
	mvDst := filepath.Join(base, "mv-renamed.txt")
	if err := rig.app.MoveFile("loopback", mvSrc, "loopback", mvDst, FileTransferOptions{}, nil); err != nil {
		t.Fatalf("MoveFile rename: %v", err)
	}
	if _, err := os.Stat(mvSrc); !os.IsNotExist(err) {
		t.Fatalf("move must remove the source")
	}
	if b, _ := os.ReadFile(mvDst); string(b) != "M" {
		t.Fatalf("moved content = %q", b)
	}

	// MoveFile across servers == copy then delete source.
	xSrc := filepath.Join(base, "x.txt")
	_ = os.WriteFile(xSrc, []byte("X"), 0o644)
	xDst := filepath.Join(base, "x-moved.txt")
	if err := rig.app.MoveFile("loopback", xSrc, "loopback2", xDst, FileTransferOptions{}, nil); err != nil {
		t.Fatalf("MoveFile cross: %v", err)
	}
	if _, err := os.Stat(xSrc); !os.IsNotExist(err) {
		t.Fatalf("cross-server move must remove the source")
	}
	if b, _ := os.ReadFile(xDst); string(b) != "X" {
		t.Fatalf("cross-moved content = %q", b)
	}
}
