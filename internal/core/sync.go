// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/pkg/proto"
)

// DefaultSyncInterval is how often the writer side is re-scanned for changes.
const DefaultSyncInterval = time.Second

// SyncDirection selects which side is the authoritative writer (source of truth).
// The other side becomes a read-only replica that mirrors the writer.
type SyncDirection string

const (
	// SyncFromLocal makes the local directory the writer and the server the
	// replica (push).
	SyncFromLocal SyncDirection = "local"
	// SyncFromRemote makes the server directory the writer and the local
	// directory the replica (pull).
	SyncFromRemote SyncDirection = "remote"
)

// SyncOptions configures a live mirror.
type SyncOptions struct {
	Interval time.Duration // re-scan interval (default 1s)
	NoDelete bool          // keep replica files that don't exist on the writer (default: delete them)
	Parallel int           // parallel streams per file copy
	From     SyncDirection // which side is the writer; default local
}

// SyncEventKind classifies a sync event.
type SyncEventKind string

const (
	SyncCopy   SyncEventKind = "copy"   // a file was propagated writer -> replica
	SyncDelete SyncEventKind = "delete" // a replica file absent on the writer was removed
	SyncReady  SyncEventKind = "ready"  // initial mirror complete, now watching
	SyncError  SyncEventKind = "error"
)

// SyncEvent is reported to the caller as the sync runs.
type SyncEvent struct {
	Kind  SyncEventKind
	Path  string // relative path within the synced directory
	Bytes int64
	Err   error
}

type fileMeta struct {
	modUnixNano int64
	size        int64
}

// syncPlan abstracts the writer/replica sides so one loop drives both push and
// pull.
type syncPlan struct {
	scanWriter         func() (map[string]fileMeta, error)
	scanReplica        func() (map[string]fileMeta, error)
	copy               func(rel string) (int64, error) // writer -> replica
	remove             func(rel string) error          // delete on replica
	ensureReplicaDir   func() error
	transientCopyError func(error) bool
}

// syncPending tracks operations that failed after a successful writer scan.
// The writer snapshot may still advance, but these entries force another
// attempt even when the source metadata does not change again.
type syncPending struct {
	copies  map[string]struct{}
	deletes map[string]struct{}
}

func newSyncPending() *syncPending {
	return &syncPending{
		copies:  make(map[string]struct{}),
		deletes: make(map[string]struct{}),
	}
}

// SyncDir keeps two directories mirrored, live, until ctx is cancelled.
//
// One side is the writer (source of truth, chosen by opts.From) and the other is
// a read-only replica. The writer is pushed to the replica once, then re-scanned
// every Interval: files that are new or differ are copied to the replica
// (overriding it), and — unless NoDelete is set — replica files that do not exist
// on the writer are removed, so the replica becomes an exact mirror. It returns
// ctx.Err() when stopped.
func (a *App) SyncDir(ctx context.Context, serverName, localDir, remoteDir string, opts SyncOptions, events func(SyncEvent)) error {
	if events == nil {
		events = func(SyncEvent) {}
	}
	if _, err := a.GetServer(serverName); err != nil {
		return err
	}
	localDir = filepath.Clean(localDir)
	remoteDir = path.Clean(remoteDir)
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultSyncInterval
	}
	pull := opts.From == SyncFromRemote

	if pull {
		// Writer is remote; create and descriptor-verify the local replica root.
		root, err := openVerifiedLocalDir(localDir, 0o750)
		if err != nil {
			return fmt.Errorf("local directory: %w", err)
		}
		_ = root.Close()
	} else {
		// Writer is local; it must exist.
		info, err := os.Stat(localDir)
		if err != nil {
			return fmt.Errorf("local directory: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", localDir)
		}
	}

	_ = a.AuditLog.Append(logs.AuditEntry{
		Action:   "file.sync.start",
		Target:   serverName,
		Operator: a.operator(),
		Details:  syncAuditDetails(serverName, localDir, remoteDir, opts),
	})

	plan := a.makeSyncPlan(serverName, localDir, remoteDir, opts, pull)

	prev := map[string]fileMeta{}
	pending := newSyncPending()
	first := true
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		writer, err := plan.scanWriter()
		if err != nil {
			// A local writer can legitimately delete a file while WalkDir is
			// inspecting it. Abort this entire scan (never reconcile a partial
			// tree) and retry on the next tick, but do not surface normal source
			// churn as an operational failure.
			if pull || !errors.Is(err, os.ErrNotExist) {
				events(SyncEvent{Kind: SyncError, Err: err})
			}
		} else {
			var replica map[string]fileMeta
			if first {
				// Re-create and re-scan the replica until the entire initial
				// reconciliation succeeds. This avoids declaring readiness
				// prematurely and retries transient mkdir, copy, and delete failures.
				if err = plan.ensureReplicaDir(); err != nil {
					events(SyncEvent{Kind: SyncError, Err: fmt.Errorf("create replica directory: %w", err)})
				} else {
					replica, err = plan.scanReplica()
					if err != nil {
						events(SyncEvent{Kind: SyncError, Err: err})
					}
				}
			}
			if err == nil {
				complete := syncReconcile(writer, replica, prev, first, opts, plan, pending, events)
				prev = writer
				if first && complete {
					events(SyncEvent{Kind: SyncReady})
					first = false
				}
			}
		}
		select {
		case <-ctx.Done():
			_ = a.AuditLog.Append(logs.AuditEntry{
				Action:   "file.sync.stop",
				Target:   serverName,
				Operator: a.operator(),
				Details:  syncAuditDetails(serverName, localDir, remoteDir, opts),
			})
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *App) makeSyncPlan(serverName, localDir, remoteDir string, opts SyncOptions, pull bool) syncPlan {
	xfer := FileTransferOptions{Parallel: opts.Parallel}
	if pull {
		return syncPlan{
			scanWriter:  func() (map[string]fileMeta, error) { return a.scanRemoteDir(serverName, remoteDir) },
			scanReplica: func() (map[string]fileMeta, error) { return scanLocalDir(localDir) },
			copy: func(rel string) (int64, error) {
				remotePath := path.Join(remoteDir, filepath.ToSlash(rel))
				if _, err := SafeLocalJoin(localDir, rel); err != nil {
					return 0, err
				}
				confined := xfer
				confined.localRoot = localDir
				confined.localRel = rel
				res, err := a.DownloadFile(serverName, remotePath, "", confined, nil)
				return res.Entry.Size, err
			},
			remove: func(rel string) error {
				if _, err := SafeLocalJoin(localDir, rel); err != nil {
					return err
				}
				return RemoveLocalUnder(localDir, rel, false)
			},
			ensureReplicaDir: func() error {
				root, err := openVerifiedLocalDir(localDir, 0o750)
				if err != nil {
					return err
				}
				return root.Close()
			},
		}
	}
	createdRemoteDirs := map[string]bool{}
	return syncPlan{
		scanWriter:  func() (map[string]fileMeta, error) { return scanLocalDir(localDir) },
		scanReplica: func() (map[string]fileMeta, error) { return a.scanRemoteDir(serverName, remoteDir) },
		copy: func(rel string) (int64, error) {
			remotePath := path.Join(remoteDir, filepath.ToSlash(rel))
			a.ensureRemoteParent(serverName, remotePath, createdRemoteDirs)
			res, err := a.UploadFile(serverName, filepath.Join(localDir, rel), remotePath, xfer, nil)
			return res.Size, err
		},
		remove: func(rel string) error {
			return a.RemoteDelete(serverName, path.Join(remoteDir, filepath.ToSlash(rel)), false)
		},
		ensureReplicaDir:   func() error { return a.RemoteMkdir(serverName, remoteDir) },
		transientCopyError: func(err error) bool { return errors.Is(err, os.ErrNotExist) },
	}
}

// syncReconcile copies new/changed writer files to the replica and (unless
// NoDelete) removes replica files that the writer no longer has. It returns
// true when every operation selected for this pass succeeded.
func syncReconcile(writer, replica, prev map[string]fileMeta, first bool, opts SyncOptions, plan syncPlan, pending *syncPending, events func(SyncEvent)) bool {
	complete := true
	rels := make([]string, 0, len(writer))
	for rel := range writer {
		rels = append(rels, rel)
		// A source path that reappeared must no longer be pending deletion.
		delete(pending.deletes, rel)
	}
	sort.Strings(rels)

	for _, rel := range rels {
		meta := writer[rel]
		_, retry := pending.copies[rel]
		needCopy := retry
		if first {
			// Override the replica where it is missing the file or its size
			// differs (rsync-style quick check).
			r, ok := replica[rel]
			needCopy = needCopy || !ok || r.size != meta.size
		} else {
			old, ok := prev[rel]
			needCopy = needCopy || !ok || old != meta
		}
		if !needCopy {
			continue
		}
		bytes, err := plan.copy(rel)
		if err != nil {
			if plan.transientCopyError != nil && plan.transientCopyError(err) {
				delete(pending.copies, rel)
				complete = false
				continue
			}
			pending.copies[rel] = struct{}{}
			complete = false
			events(SyncEvent{Kind: SyncError, Path: rel, Err: err})
			continue
		}
		delete(pending.copies, rel)
		events(SyncEvent{Kind: SyncCopy, Path: rel, Bytes: bytes})
	}

	// A source path deleted while its copy was pending should be removed from
	// the replica, not copied from a path that no longer exists.
	for rel := range pending.copies {
		if _, ok := writer[rel]; !ok {
			delete(pending.copies, rel)
		}
	}

	if opts.NoDelete {
		// Deletion is disabled, including retries left by an earlier pass.
		clear(pending.deletes)
		return complete
	}
	// Delete replica files absent on the writer. On the first pass compare
	// against the replica's actual contents (removes pre-existing extras);
	// afterwards compare against the previous writer snapshot (removes files the
	// writer deleted). Include earlier failures so they retry without another
	// source-side change.
	base := prev
	if first {
		base = replica
	}
	goneSet := make(map[string]struct{}, len(pending.deletes))
	for rel := range pending.deletes {
		if _, exists := writer[rel]; !exists {
			goneSet[rel] = struct{}{}
		}
	}
	for rel := range base {
		if _, ok := writer[rel]; !ok {
			goneSet[rel] = struct{}{}
		}
	}
	gone := make([]string, 0, len(goneSet))
	for rel := range goneSet {
		gone = append(gone, rel)
	}
	sort.Strings(gone)
	for _, rel := range gone {
		if err := plan.remove(rel); err != nil {
			pending.deletes[rel] = struct{}{}
			complete = false
			events(SyncEvent{Kind: SyncError, Path: rel, Err: err})
			continue
		}
		delete(pending.deletes, rel)
		events(SyncEvent{Kind: SyncDelete, Path: rel})
	}
	return complete
}

// ensureRemoteParent mkdir -p's the remote parent directory of remotePath once.
func (a *App) ensureRemoteParent(serverName, remotePath string, createdDirs map[string]bool) {
	dir := path.Dir(remotePath)
	if dir == "" || dir == "." || createdDirs[dir] {
		return
	}
	if err := a.RemoteMkdir(serverName, dir); err == nil {
		createdDirs[dir] = true
	}
}

// scanLocalDir returns every regular file under root as relpath -> {mtime,size}.
// Directories and symlinks are skipped; .git metadata is excluded.
func scanLocalDir(root string) (map[string]fileMeta, error) {
	out := map[string]fileMeta{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", p, err)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return fmt.Errorf("resolve relative path for %s: %w", p, err)
		}
		out[rel] = fileMeta{modUnixNano: info.ModTime().UnixNano(), size: info.Size()}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// maxRemoteScanDepth and maxRemoteScanFiles bound a recursive remote listing so
// a malicious or malformed agent cannot exhaust the controller's stack/memory
// with a self-referential or enormous directory tree.
const (
	maxRemoteScanDepth = 64
	maxRemoteScanFiles = 1_000_000
)

// scanRemoteDir recursively lists a remote directory tree as relpath ->
// {mtime,size}. Listing errors fail the entire scan so a partial writer tree
// can never be mistaken for deletions. Bounded in depth and total files; the
// agent is not trusted to be honest about its tree.
func (a *App) scanRemoteDir(serverName, root string) (map[string]fileMeta, error) {
	out := map[string]fileMeta{}
	rootRes, err := a.ListRemoteDir(serverName, root)
	if err != nil {
		return nil, fmt.Errorf("list remote sync root %s: %w", root, err)
	}
	resolvedRoot := rootRes.Path
	if resolvedRoot == "" {
		resolvedRoot = root
	}
	prefix := strings.TrimSuffix(resolvedRoot, "/") + "/"

	var visit func(entries []proto.FileEntry, depth int) error
	visit = func(entries []proto.FileEntry, depth int) error {
		if depth > maxRemoteScanDepth {
			return fmt.Errorf("remote directory tree exceeds maximum depth %d", maxRemoteScanDepth)
		}
		for _, e := range entries {
			if len(out) >= maxRemoteScanFiles {
				return fmt.Errorf("remote directory tree exceeds maximum of %d files", maxRemoteScanFiles)
			}
			if e.IsDir {
				sub, err := a.ListRemoteDir(serverName, e.Path)
				if err != nil {
					return fmt.Errorf("list remote sync directory %s: %w", e.Path, err)
				}
				if err := visit(sub.Entries, depth+1); err != nil {
					return err
				}
				continue
			}
			rel := strings.TrimPrefix(e.Path, prefix)
			// A compromised agent could return a path that escapes the sync root;
			// never let that reach a local write during pull.
			if !safeRel(rel) {
				continue
			}
			out[rel] = fileMeta{modUnixNano: e.ModTime.UnixNano(), size: e.Size}
		}
		return nil
	}
	if err := visit(rootRes.Entries, 0); err != nil {
		return nil, err
	}
	return out, nil
}

func syncAuditDetails(server, localDir, remoteDir string, opts SyncOptions) string {
	dir := "local->remote"
	if opts.From == SyncFromRemote {
		dir = "remote->local"
	}
	return fmt.Sprintf("%s (%s) %s:%s", localDir, dir, server, remoteDir)
}

// SyncSummary returns a one-line "writer → replica" banner for the CLI.
func SyncSummary(server, localDir, remoteDir string, from SyncDirection) string {
	local := localDir
	remote := server + ":" + strings.TrimRight(remoteDir, "/")
	if from == SyncFromRemote {
		return remote + "  →  " + local + "   (server is the writer)"
	}
	return local + "  →  " + remote + "   (local is the writer)"
}
