// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cenvero/fleet/internal/transport"
)

// TestCopyFileRejectsBlankFinalizeDigest confirms the relay integrity check
// fails closed: a destination agent that writes the file but returns an EMPTY
// finalize digest must be treated as a verification FAILURE, not a silent pass.
func TestCopyFileRejectsBlankFinalizeDigest(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	go func() {
		for range rig.errCh {
		}
	}()
	// The single in-memory agent backs both servers; make its Finalize blank out
	// the digest so the destination leg returns no SHA.
	rig.fileMgr.setBlankFinalize(true)
	if err := rig.app.AddServer(ServerRecord{
		Name: "loopback2", Address: "127.0.0.1", Port: 2222, Mode: transport.ModeDirect, User: "cenvero-agent",
	}); err != nil {
		t.Fatalf("AddServer loopback2: %v", err)
	}

	base := t.TempDir()
	src := filepath.Join(base, "src.txt")
	if err := os.WriteFile(src, []byte("integrity-or-bust"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(base, "dst.txt")

	_, err := rig.app.CopyFile("loopback", src, "loopback2", dst, FileTransferOptions{}, nil)
	if err == nil {
		t.Fatal("CopyFile must fail when the destination returns no finalize digest")
	}
	if !strings.Contains(err.Error(), "no finalize digest") {
		t.Fatalf("expected a blank-digest failure, got: %v", err)
	}
}

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
	_ = os.WriteFile(filepath.Join(srcDir, ".hidden"), []byte("H"), 0o644)
	if err := os.MkdirAll(filepath.Join(srcDir, "empty", "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	dstDir := filepath.Join(base, "tree-copy")
	if n, err := rig.app.CopyDir("loopback", srcDir, "loopback2", dstDir, FileTransferOptions{}, nil); err != nil || n != 3 {
		t.Fatalf("CopyDir n=%d err=%v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dstDir, "sub", "b.txt")); string(b) != "B" {
		t.Fatalf("recursive copy wrong: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dstDir, ".hidden")); string(b) != "H" {
		t.Fatalf("hidden copy wrong: %q", b)
	}
	if info, err := os.Stat(filepath.Join(dstDir, "empty", "nested")); err != nil || !info.IsDir() {
		t.Fatalf("copied empty directory missing: info=%v err=%v", info, err)
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

func TestCopyDirRefusesRemoteSymlink(t *testing.T) {
	rig := newTransferRig(t)
	go func() {
		for range rig.errCh {
		}
	}()
	if err := rig.app.AddServer(ServerRecord{
		Name: "loopback2", Address: "127.0.0.1", Port: 2222, Mode: transport.ModeDirect, User: "cenvero-agent",
	}); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	target := filepath.Join(src, "target.txt")
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(src, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := rig.app.CopyDir("loopback", src, "loopback2", filepath.Join(t.TempDir(), "dst"), FileTransferOptions{}, nil); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("CopyDir symlink error = %v", err)
	}
}

func TestCopyFileValidatesDestinationBeforeRelay(t *testing.T) {
	rig := newTransferRig(t)
	server, err := rig.app.GetServer("loopback")
	if err != nil {
		t.Fatal(err)
	}
	server.Observed.OS = "windows"
	if err := rig.app.SaveServer(server); err != nil {
		t.Fatal(err)
	}
	relayPattern := filepath.Join(rig.app.ConfigDir, "tmp", "fleet-relay-*")
	before, err := filepath.Glob(relayPattern)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rig.app.CopyFile("missing-source-server", "/source", "loopback", `C:\Data\CON`, FileTransferOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid destination path") {
		t.Fatalf("CopyFile destination error = %v", err)
	}
	after, err := filepath.Glob(relayPattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("relay files changed before destination validation: before=%v after=%v", before, after)
	}
}

// progressLog records every progress update of one transfer.
type progressLog struct {
	mu      sync.Mutex
	updates []ProgressUpdate
}

func (p *progressLog) add(u ProgressUpdate) {
	p.mu.Lock()
	p.updates = append(p.updates, u)
	p.mu.Unlock()
}

// check fails unless every update is bounded by want and the last one is a
// completed want/want.
func (p *progressLog) check(t *testing.T, what string, want int64) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.updates) == 0 {
		t.Fatalf("%s: no progress reported", what)
	}
	for _, u := range p.updates {
		if u.TotalBytes != want || u.BytesDone > want {
			t.Fatalf("%s: progress %d/%d; want at most %d/%d", what, u.BytesDone, u.TotalBytes, want, want)
		}
	}
	if last := p.updates[len(p.updates)-1]; !last.Done || last.BytesDone != want {
		t.Fatalf("%s: final progress %+v; want done at %d", what, last, want)
	}
}

// TestCopyProgressReportsFileSizeOnce is the regression for server-to-server
// copies reporting twice the file size (read + write) as their progress, so
// a 95.4 MiB copy showed "190.7 MiB/190.7 MiB" and a doubled rate.
func TestCopyProgressReportsFileSizeOnce(t *testing.T) {
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
	big := filepath.Join(base, "big.bin")
	const bigSize = 300 << 10 // several 64 KiB chunks: the chunked relay
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), bigSize), 0o644); err != nil {
		t.Fatal(err)
	}
	small := filepath.Join(base, "small.txt")
	if err := os.WriteFile(small, []byte("one chunk"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := FileTransferOptions{ChunkSize: 64 << 10, Parallel: 3}

	for _, tc := range []struct {
		what, src, dstServer string
		size                 int64
	}{
		{"chunked relay", big, "loopback2", bigSize},
		{"single-request relay", small, "loopback2", int64(len("one chunk"))},
		{"same-server copy", big, "loopback", bigSize},
	} {
		var log progressLog
		dst := filepath.Join(base, strings.ReplaceAll(tc.what, " ", "-"))
		if _, err := rig.app.CopyFile("loopback", tc.src, tc.dstServer, dst, opts, log.add); err != nil {
			t.Fatalf("%s: %v", tc.what, err)
		}
		log.check(t, tc.what, tc.size)
	}

	tree := filepath.Join(base, "tree")
	if err := os.MkdirAll(tree, 0o750); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := os.WriteFile(filepath.Join(tree, fmt.Sprintf("f%d", i)), bytes.Repeat([]byte("y"), 100<<10), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var log progressLog
	if _, err := rig.app.CopyDir("loopback", tree, "loopback2", filepath.Join(base, "tree-copy"), opts, log.add); err != nil {
		t.Fatal(err)
	}
	log.check(t, "directory copy", 3*(100<<10))
}
