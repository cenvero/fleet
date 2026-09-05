// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
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
	syncWrite(t, filepath.Join(localDir, ".hidden"), "secret")
	if err := os.MkdirAll(filepath.Join(localDir, "empty", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

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
	syncAssertFile(t, filepath.Join(remoteDir, ".hidden"), "secret")
	if info, err := os.Stat(filepath.Join(remoteDir, "empty", "nested")); err != nil || !info.IsDir() {
		t.Fatalf("synced empty directory missing: info=%v err=%v", info, err)
	}

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

	if err := os.RemoveAll(filepath.Join(localDir, "empty")); err != nil {
		t.Fatal(err)
	}
	syncWaitKind(t, evs, SyncDelete, "empty")
	if _, err := os.Stat(filepath.Join(remoteDir, "empty")); !os.IsNotExist(err) {
		t.Fatalf("expected remote empty directory tree to be deleted: %v", err)
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
	syncWrite(t, filepath.Join(remoteDir, ".hidden"), "H")
	if err := os.MkdirAll(filepath.Join(remoteDir, "empty", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
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
	syncAssertFile(t, filepath.Join(localDir, ".hidden"), "H")
	if info, err := os.Stat(filepath.Join(localDir, "empty", "nested")); err != nil || !info.IsDir() {
		t.Fatalf("pulled empty directory missing: info=%v err=%v", info, err)
	}
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

func TestSyncReconcileRetriesFailedCopyWithoutSourceChange(t *testing.T) {
	writer := map[string]fileMeta{"a.txt": {modUnixNano: 1, size: 5}}
	pending := newSyncPending()
	attempts := 0
	var copied int
	plan := syncPlan{
		copy: func(string, fileMeta) (int64, error) {
			attempts++
			if attempts == 1 {
				return 0, errors.New("injected copy failure")
			}
			return 5, nil
		},
		remove: func(string, fileMeta) error { return nil },
	}
	events := func(event SyncEvent) {
		if event.Kind == SyncCopy {
			copied++
		}
	}

	if syncReconcile(writer, nil, map[string]fileMeta{}, false, SyncOptions{}, plan, pending, events) {
		t.Fatal("failed copy reported a complete reconciliation")
	}
	// Simulate SyncDir advancing its source snapshot. The second pass must still
	// retry solely because the operation remains pending.
	if !syncReconcile(writer, nil, writer, false, SyncOptions{}, plan, pending, events) {
		t.Fatal("successful retry did not complete reconciliation")
	}
	if attempts != 2 || copied != 1 {
		t.Fatalf("copy attempts=%d successful events=%d, want 2 and 1", attempts, copied)
	}
	if len(pending.copies) != 0 {
		t.Fatalf("successful copy remained pending: %#v", pending.copies)
	}
}

func TestSyncReconcileRetriesFailedDeleteWithoutSourceChange(t *testing.T) {
	previous := map[string]fileMeta{"gone.txt": {modUnixNano: 1, size: 5}}
	writer := map[string]fileMeta{}
	pending := newSyncPending()
	attempts := 0
	var deleted int
	plan := syncPlan{
		copy: func(string, fileMeta) (int64, error) { return 0, nil },
		remove: func(string, fileMeta) error {
			attempts++
			if attempts == 1 {
				return errors.New("injected delete failure")
			}
			return nil
		},
	}
	events := func(event SyncEvent) {
		if event.Kind == SyncDelete {
			deleted++
		}
	}

	if syncReconcile(writer, nil, previous, false, SyncOptions{}, plan, pending, events) {
		t.Fatal("failed delete reported a complete reconciliation")
	}
	// The source snapshot is now unchanged and no longer mentions the path.
	if !syncReconcile(writer, nil, writer, false, SyncOptions{}, plan, pending, events) {
		t.Fatal("successful delete retry did not complete reconciliation")
	}
	if attempts != 2 || deleted != 1 {
		t.Fatalf("delete attempts=%d successful events=%d, want 2 and 1", attempts, deleted)
	}
	if len(pending.deletes) != 0 {
		t.Fatalf("successful delete remained pending: %#v", pending.deletes)
	}
}

func TestSyncPullRejectsIntermediateSymlinkEscape(t *testing.T) {
	rig := newTransferRig(t)
	remoteDir := t.TempDir()
	syncWrite(t, filepath.Join(remoteDir, "sub", "file.txt"), "remote")
	localDir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(localDir, "sub")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	plan := rig.app.makeSyncPlan("loopback", localDir, remoteDir, SyncOptions{From: SyncFromRemote}, true, TargetPathPOSIX)
	if _, err := plan.copy(filepath.Join("sub", "file.txt"), fileMeta{kind: fileKindRegular}); err == nil {
		t.Fatal("sync pull should reject an intermediate symlink escaping the replica root")
	}
	if _, err := os.Stat(filepath.Join(outside, "file.txt")); !os.IsNotExist(err) {
		t.Fatalf("sync pull escaped through intermediate symlink: %v", err)
	}
}

func TestCleanRelativeKeyIsSlashNeutralAndSafe(t *testing.T) {
	for input, want := range map[string]string{
		"dir/file.txt":   "dir/file.txt",
		"./dir/file.txt": "dir/file.txt",
		"x:stream":       "x:stream",
	} {
		got, err := cleanRelativeKey(input)
		if err != nil {
			t.Errorf("cleanRelativeKey(%q): %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("cleanRelativeKey(%q) = %q, want %q", input, got, want)
		}
	}
	for _, input := range []string{"../escape", `dir\sub\file.txt`, `dir\..\escape`, "/absolute", `C:\absolute`, `C:/absolute`, `\\server\share\file`} {
		if _, err := cleanRelativeKey(input); err == nil {
			t.Errorf("unsafe relative key %q accepted", input)
		}
	}
}

func TestRelativeKeyJoinsEndpointStylesIndependently(t *testing.T) {
	key, err := cleanRelativeKey("nested/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := TargetPathPOSIX.Join("/srv/source", key); got != "/srv/source/nested/file.txt" {
		t.Fatalf("POSIX join = %q", got)
	}
	if got := TargetPathWindows.Join(`D:\dest`, key); got != `D:\dest\nested\file.txt` {
		t.Fatalf("Windows join = %q", got)
	}
}

func TestSyncInvalidWriterSnapshotCannotDeleteReplica(t *testing.T) {
	rig := newTransferRig(t)
	go func() {
		for range rig.errCh {
		}
	}()
	localDir := t.TempDir()
	remoteDir := filepath.Join(t.TempDir(), "replica")
	syncWrite(t, filepath.Join(localDir, "keep.txt"), "keep")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan SyncEvent, 128)
	done := make(chan error, 1)
	go func() {
		done <- rig.app.SyncDir(ctx, "loopback", localDir, remoteDir, SyncOptions{Interval: 20 * time.Millisecond}, func(event SyncEvent) {
			events <- event
		})
	}()
	syncWaitKind(t, events, SyncReady, "")
	syncAssertFile(t, filepath.Join(remoteDir, "keep.txt"), "keep")

	// Make the next writer tree invalid before omitting keep.txt. The scanner may
	// have accumulated entries before it reaches the link, but must discard that
	// partial snapshot and never turn the omission into a replica deletion.
	if err := os.Symlink(filepath.Join(localDir, "keep.txt"), filepath.Join(localDir, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.Remove(filepath.Join(localDir, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Kind == SyncError && strings.Contains(event.Err.Error(), "symlink") {
				goto observed
			}
		case <-deadline:
			t.Fatal("timed out waiting for invalid writer scan error")
		}
	}

observed:
	time.Sleep(80 * time.Millisecond)
	syncAssertFile(t, filepath.Join(remoteDir, "keep.txt"), "keep")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("SyncDir returned %v, want context.Canceled", err)
	}
}

func TestSyncReconcileNoDeleteResolvesDirectoryToFileConflictInDepthOrder(t *testing.T) {
	writer := map[string]fileMeta{"tree": {kind: fileKindRegular, size: 1}}
	replica := map[string]fileMeta{
		"tree":          {kind: fileKindDirectory},
		"tree/empty":    {kind: fileKindDirectory},
		"tree/file.txt": {kind: fileKindRegular, size: 1},
	}
	var operations []string
	plan := syncPlan{
		remove: func(rel string, _ fileMeta) error {
			operations = append(operations, "remove "+rel)
			return nil
		},
		copy: func(rel string, _ fileMeta) (int64, error) {
			operations = append(operations, "copy "+rel)
			return 1, nil
		},
	}
	if !syncReconcile(writer, replica, nil, true, SyncOptions{NoDelete: true}, plan, newSyncPending(), func(SyncEvent) {}) {
		t.Fatal("type-conflict reconciliation did not complete")
	}
	want := []string{"remove tree/file.txt", "remove tree/empty", "remove tree", "copy tree"}
	if strings.Join(operations, "|") != strings.Join(want, "|") {
		t.Fatalf("operations = %v, want %v", operations, want)
	}
}

func TestValidateTreeForStyleRejectsUnrepresentableDestinations(t *testing.T) {
	t.Parallel()
	regular := func(names ...string) map[string]fileMeta {
		entries := make(map[string]fileMeta, len(names))
		for _, name := range names {
			entries[name] = fileMeta{kind: fileKindRegular}
		}
		return entries
	}
	for _, entries := range []map[string]fileMeta{
		regular("README", "Readme"),
		regular("A/file.txt", "a/other.txt"),
		regular("CON"),
		regular("x:stream"),
		regular("trailing."),
		regular("trailing "),
		regular("wild*card"),
	} {
		if err := validateTreeForStyle(entries, TargetPathWindows); err == nil {
			t.Errorf("Windows destination accepted unrepresentable tree: %#v", entries)
		}
	}
	caseDistinct := regular("README", "Readme", "A/file.txt", "a/other.txt")
	if err := validateTreeForStyle(caseDistinct, TargetPathPOSIX); err != nil {
		t.Fatalf("POSIX destination rejected case-distinct paths: %v", err)
	}
	conflict := regular("node", "node/child.txt")
	if err := validateTreeForStyle(conflict, TargetPathPOSIX); err == nil {
		t.Fatal("file used as an ancestor directory was accepted")
	}
}

func TestSyncPullValidatesWriterBeforeCreatingReplica(t *testing.T) {
	if NativePathStyle().IsWindows() {
		t.Skip("fixture uses a POSIX-valid backslash name")
	}
	rig := newTransferRig(t)
	go func() {
		for range rig.errCh {
		}
	}()
	remoteDir := t.TempDir()
	syncWrite(t, filepath.Join(remoteDir, `bad\name.txt`), "bad")
	localDir := filepath.Join(t.TempDir(), "absent-replica")
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan SyncEvent, 16)
	done := make(chan error, 1)
	go func() {
		done <- rig.app.SyncDir(ctx, "loopback", localDir, remoteDir,
			SyncOptions{Interval: 20 * time.Millisecond, From: SyncFromRemote},
			func(event SyncEvent) { events <- event })
	}()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Kind == SyncError {
				if _, err := os.Stat(localDir); !os.IsNotExist(err) {
					t.Fatalf("invalid writer created local replica before validation: %v", err)
				}
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("SyncDir returned %v, want context.Canceled", err)
				}
				return
			}
		case <-deadline:
			cancel()
			t.Fatal("timed out waiting for destination namespace error")
		}
	}
}

func TestValidateTreeForCaseInsensitivePOSIXDestination(t *testing.T) {
	t.Parallel()
	entries := map[string]fileMeta{
		"README": {kind: fileKindRegular},
		"Readme": {kind: fileKindRegular},
	}
	if err := validateTreeForStyleCase(entries, TargetPathPOSIX, false); err != nil {
		t.Fatalf("case-sensitive POSIX destination rejected names: %v", err)
	}
	if err := validateTreeForStyleCase(entries, TargetPathPOSIX, true); err == nil {
		t.Fatal("case-insensitive POSIX destination accepted colliding names")
	}
}

func TestRemoteEntryKindRejectsLegacyUnclassifiedMetadata(t *testing.T) {
	t.Parallel()
	legacy := proto.FileEntry{Path: "/tmp/pipe", Mode: uint32(0o600)}
	if _, err := remoteEntryKind(legacy); err == nil || !strings.Contains(err.Error(), "agent must be updated") {
		t.Fatalf("legacy unclassified entry error = %v", err)
	}
	if kind, err := remoteEntryKind(proto.FileEntry{Path: "/tmp/file", Type: proto.FileEntryTypeRegular}); err != nil || kind != fileKindRegular {
		t.Fatalf("current regular entry kind=%v err=%v", kind, err)
	}
	if _, err := remoteEntryKind(proto.FileEntry{Path: "/tmp/socket", Type: proto.FileEntryTypeOther}); err == nil {
		t.Fatal("explicit special entry was accepted")
	}
}
