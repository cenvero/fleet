// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScanRemoteDirUsesOneTreeCall(t *testing.T) {
	t.Parallel()
	rig := quietRig(t)
	root := t.TempDir()
	want := writeTree(t, root, 6, 2)
	if err := os.MkdirAll(filepath.Join(root, "empty", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The first call dials and records the agent's capabilities.
	if _, err := rig.app.ListRemoteDir("loopback", root); err != nil {
		t.Fatal(err)
	}
	rig.fileMgr.resetCalls()
	got, err := rig.app.scanRemoteDir("loopback", root)
	if err != nil {
		t.Fatalf("scanRemoteDir: %v", err)
	}
	if rig.fileMgr.callCount("tree") != 1 || rig.fileMgr.callCount("list") != 0 {
		t.Fatalf("scan used tree=%d list=%d", rig.fileMgr.callCount("tree"), rig.fileMgr.callCount("list"))
	}
	for rel := range want {
		if meta, ok := got[rel]; !ok || meta.isDir() {
			t.Fatalf("scan missing file %s: %v", rel, got)
		}
	}
	for _, d := range []string{"d00", "empty", "empty/nested"} {
		if meta, ok := got[d]; !ok || !meta.isDir() {
			t.Fatalf("scan missing directory %s", d)
		}
	}
	if len(got) != len(want)+6+2 {
		t.Fatalf("scan has %d entries, want %d", len(got), len(want)+8)
	}
}

// TestSyncPushCopiesManyFilesConcurrently mirrors a tree with more files than
// the per-pass concurrency and checks every file and directory arrives.
func TestSyncPushCopiesManyFilesConcurrently(t *testing.T) {
	t.Parallel()
	rig := quietRig(t)
	local := t.TempDir()
	want := writeTree(t, local, 5, 6)
	remote := filepath.Join(t.TempDir(), "replica")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan SyncEvent, 256)
	done := make(chan error, 1)
	go func() {
		done <- rig.app.SyncDir(ctx, "loopback", local, remote, SyncOptions{Interval: 20 * time.Millisecond}, func(e SyncEvent) { events <- e })
	}()
	syncWaitKind(t, events, SyncReady, "")
	for rel, content := range want {
		syncAssertFile(t, filepath.Join(remote, filepath.FromSlash(rel)), content)
	}
	// A later change is still picked up after the scan interval backed off.
	time.Sleep(150 * time.Millisecond)
	syncWrite(t, filepath.Join(local, "d01", "late.txt"), "late")
	syncWaitKind(t, events, SyncCopy, "d01/late.txt")
	syncAssertFile(t, filepath.Join(remote, "d01", "late.txt"), "late")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("SyncDir returned %v", err)
	}
}

func TestSyncPullScansWithTreeAndKeepsDeleteSemantics(t *testing.T) {
	t.Parallel()
	rig := quietRig(t)
	remote := t.TempDir()
	want := writeTree(t, remote, 4, 3)
	local := t.TempDir()
	syncWrite(t, filepath.Join(local, "stale.txt"), "old")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan SyncEvent, 256)
	done := make(chan error, 1)
	go func() {
		done <- rig.app.SyncDir(ctx, "loopback", local, remote, SyncOptions{Interval: 20 * time.Millisecond, From: SyncFromRemote}, func(e SyncEvent) { events <- e })
	}()
	syncWaitKind(t, events, SyncReady, "")
	for rel, content := range want {
		syncAssertFile(t, filepath.Join(local, filepath.FromSlash(rel)), content)
	}
	if _, err := os.Stat(filepath.Join(local, "stale.txt")); !os.IsNotExist(err) {
		t.Fatal("pull did not remove the replica extra")
	}
	if rig.fileMgr.callCount("list") > 1 {
		t.Fatalf("pull scans listed %d directories one by one; file.tree should cover them", rig.fileMgr.callCount("list"))
	}
	if err := os.Remove(filepath.Join(remote, "d02", "f01.txt")); err != nil {
		t.Fatal(err)
	}
	syncWaitKind(t, events, SyncDelete, fmt.Sprintf("d%02d/f%02d.txt", 2, 1))
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("SyncDir returned %v", err)
	}
}

func TestNextSyncWaitBacksOffAndResets(t *testing.T) {
	t.Parallel()
	base := time.Second
	wait := base
	var seen []time.Duration
	for range 6 {
		wait = nextSyncWait(base, wait, false, time.Millisecond)
		seen = append(seen, wait)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second, 5 * time.Second, 5 * time.Second}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("idle waits = %v, want %v", seen, want)
		}
	}
	if got := nextSyncWait(base, wait, true, time.Millisecond); got != base {
		t.Fatalf("a change should reset the wait, got %v", got)
	}
	// A scan that takes long is never repeated back to back.
	if got := nextSyncWait(base, base, true, 3*time.Second); got != 6*time.Second {
		t.Fatalf("slow scan wait = %v", got)
	}
	// Short test intervals back off to eight intervals.
	if got := nextSyncWait(20*time.Millisecond, 160*time.Millisecond, false, 0); got != 160*time.Millisecond {
		t.Fatalf("short interval ceiling = %v", got)
	}
}
