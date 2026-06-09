// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSyncDirPush: local is the writer; the server mirrors it.
func TestSyncDirPush(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	go func() {
		for range rig.errCh {
		}
	}()

	localDir := t.TempDir()
	remoteDir := filepath.Join(t.TempDir(), "dest")
	syncWrite(t, filepath.Join(localDir, "a.txt"), "alpha")
	syncWrite(t, filepath.Join(localDir, "sub", "b.txt"), "beta")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evs := make(chan SyncEvent, 512)
	done := make(chan error, 1)
	go func() {
		done <- rig.app.SyncDir(ctx, "loopback", localDir, remoteDir,
			SyncOptions{Interval: 20 * time.Millisecond}, // mirror (delete) by default, push
			func(e SyncEvent) { evs <- e })
	}()

	syncWaitKind(t, evs, SyncReady, "")
	syncAssertFile(t, filepath.Join(remoteDir, "a.txt"), "alpha")
	syncAssertFile(t, filepath.Join(remoteDir, "sub", "b.txt"), "beta")

	syncWrite(t, filepath.Join(localDir, "a.txt"), "ALPHA-v2")
	syncWaitKind(t, evs, SyncCopy, "a.txt")
	syncAssertFile(t, filepath.Join(remoteDir, "a.txt"), "ALPHA-v2")

	syncWrite(t, filepath.Join(localDir, "c.txt"), "gamma")
	syncWaitKind(t, evs, SyncCopy, "c.txt")
	syncAssertFile(t, filepath.Join(remoteDir, "c.txt"), "gamma")

	if err := os.Remove(filepath.Join(localDir, "a.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	syncWaitKind(t, evs, SyncDelete, "a.txt")
	if _, err := os.Stat(filepath.Join(remoteDir, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected remote a.txt to be deleted")
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("SyncDir returned %v, want context.Canceled", err)
	}
}

// TestSyncDirPull: the server is the writer; the local directory mirrors it,
// and a pre-existing local extra is removed.
func TestSyncDirPull(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	go func() {
		for range rig.errCh {
		}
	}()

	remoteDir := t.TempDir() // server writer (real fs served by the in-memory agent)
	localDir := t.TempDir()  // local replica
	syncWrite(t, filepath.Join(remoteDir, "x.txt"), "X")
	syncWrite(t, filepath.Join(remoteDir, "d", "y.txt"), "Y")
	syncWrite(t, filepath.Join(localDir, "stale.txt"), "old") // extra → should be deleted

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evs := make(chan SyncEvent, 512)
	done := make(chan error, 1)
	go func() {
		done <- rig.app.SyncDir(ctx, "loopback", localDir, remoteDir,
			SyncOptions{Interval: 20 * time.Millisecond, From: SyncFromRemote},
			func(e SyncEvent) { evs <- e })
	}()

	syncWaitKind(t, evs, SyncReady, "")
	syncAssertFile(t, filepath.Join(localDir, "x.txt"), "X")
	syncAssertFile(t, filepath.Join(localDir, "d", "y.txt"), "Y")
	if _, err := os.Stat(filepath.Join(localDir, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected local stale.txt to be deleted")
	}

	syncWrite(t, filepath.Join(remoteDir, "x.txt"), "X-v2")
	syncWaitKind(t, evs, SyncCopy, "x.txt")
	syncAssertFile(t, filepath.Join(localDir, "x.txt"), "X-v2")

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("SyncDir returned %v, want context.Canceled", err)
	}
}

func syncWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func syncAssertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func syncWaitKind(t *testing.T, evs <-chan SyncEvent, kind SyncEventKind, relPath string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-evs:
			if e.Kind == SyncError {
				t.Fatalf("sync error on %q: %v", e.Path, e.Err)
			}
			if e.Kind == kind && (relPath == "" || e.Path == relPath) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s %q", kind, relPath)
		}
	}
}
