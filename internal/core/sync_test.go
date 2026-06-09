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

func TestSyncDirLifecycle(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	// Drain the agent-serve results so the many per-file connections don't block.
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
			SyncOptions{Interval: 20 * time.Millisecond, Delete: true},
			func(e SyncEvent) { evs <- e })
	}()

	// Initial sync pushes the whole tree.
	syncWaitKind(t, evs, SyncReady, "")
	syncAssertFile(t, filepath.Join(remoteDir, "a.txt"), "alpha")
	syncAssertFile(t, filepath.Join(remoteDir, "sub", "b.txt"), "beta")

	// Modify a file → re-uploaded.
	syncWrite(t, filepath.Join(localDir, "a.txt"), "ALPHA-v2")
	syncWaitKind(t, evs, SyncUpload, "a.txt")
	syncAssertFile(t, filepath.Join(remoteDir, "a.txt"), "ALPHA-v2")

	// Add a new file → uploaded.
	syncWrite(t, filepath.Join(localDir, "c.txt"), "gamma")
	syncWaitKind(t, evs, SyncUpload, "c.txt")
	syncAssertFile(t, filepath.Join(remoteDir, "c.txt"), "gamma")

	// Delete locally → removed remotely (Delete: true).
	if err := os.Remove(filepath.Join(localDir, "a.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	syncWaitKind(t, evs, SyncDelete, "a.txt")
	if _, err := os.Stat(filepath.Join(remoteDir, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected remote a.txt to be deleted")
	}

	// Stopping the command stops the sync.
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
