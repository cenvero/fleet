// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cenvero/fleet/internal/transport"
)

// countDials wraps the rig's dialer so a test can assert how many SSH
// connections the controller established.
func countDials(rig *transferTestRig) *atomic.Int64 {
	var dials atomic.Int64
	orig := rig.app.NetworkDialContext
	rig.app.NetworkDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return orig(ctx, network, addr)
	}
	return &dials
}

// writeTree creates dirs x files small files under root and returns the file
// contents keyed by slash-separated relative path.
func writeTree(t *testing.T, root string, dirs, files int) map[string]string {
	t.Helper()
	want := make(map[string]string, dirs*files)
	for d := range dirs {
		for f := range files {
			rel := fmt.Sprintf("d%02d/f%02d.txt", d, f)
			content := fmt.Sprintf("payload %s %d", rel, d*files+f)
			syncWrite(t, filepath.Join(root, filepath.FromSlash(rel)), content)
			want[rel] = content
		}
	}
	return want
}

// TestDirTransfersWithManyFilesShareOneConnection is the regression test for
// folder transfers of eight or more files failing in direct mode. Every file
// used to dial its own SSH connection on top of the pooled one, and the agent
// accepts at most maxConnectionsPerIdentity connections per key, so the ninth
// concurrent connection was refused ("open fleet-rpc channel: EOF"). Transfers
// now lease channels on the pooled connection instead.
func TestDirTransfersWithManyFilesShareOneConnection(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	go func() {
		for range rig.errCh {
		}
	}()
	dials := countDials(rig)
	if err := rig.app.AddServer(ServerRecord{
		Name: "loopback2", Address: "127.0.0.1", Port: 2222, Mode: transport.ModeDirect, User: "cenvero-agent",
	}); err != nil {
		t.Fatalf("AddServer loopback2: %v", err)
	}

	src := t.TempDir()
	want := writeTree(t, src, 10, 4) // 40 files, 5x the agent's per-key connection cap

	remote := filepath.Join(t.TempDir(), "up")
	if n, err := rig.app.UploadDir("loopback", src, remote, FileTransferOptions{}, nil); err != nil || n != len(want) {
		t.Fatalf("UploadDir n=%d err=%v", n, err)
	}
	for rel, content := range want {
		syncAssertFile(t, filepath.Join(remote, filepath.FromSlash(rel)), content)
	}

	local := filepath.Join(t.TempDir(), "down")
	if n, err := rig.app.DownloadDir("loopback", remote, local, FileTransferOptions{}, nil); err != nil || n != len(want) {
		t.Fatalf("DownloadDir n=%d err=%v", n, err)
	}
	for rel, content := range want {
		syncAssertFile(t, filepath.Join(local, filepath.FromSlash(rel)), content)
	}

	copied := filepath.Join(t.TempDir(), "copy")
	if n, err := rig.app.CopyDir("loopback", remote, "loopback2", copied, FileTransferOptions{}, nil); err != nil || n != len(want) {
		t.Fatalf("CopyDir n=%d err=%v", n, err)
	}
	for rel, content := range want {
		syncAssertFile(t, filepath.Join(copied, filepath.FromSlash(rel)), content)
	}

	// loopback and loopback2 are two server records, so two pooled
	// connections at most — never one per file.
	if got := dials.Load(); got > 2 {
		t.Fatalf("directory transfers dialed %d SSH connections; want them to share the pooled connection(s)", got)
	}
	if _, err := os.Stat(filepath.Join(remote, "d00")); err != nil {
		t.Fatal(err)
	}
}
