// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
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
	scanWriter       func() (map[string]fileMeta, error)
	scanReplica      func() (map[string]fileMeta, error)
	copy             func(rel string) (int64, error) // writer -> replica
	remove           func(rel string) error          // delete on replica
	ensureReplicaDir func()
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
		// Writer is remote; the local replica directory must exist.
		if err := os.MkdirAll(localDir, 0o750); err != nil {
			return fmt.Errorf("local directory: %w", err)
		}
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
	plan.ensureReplicaDir()

	prev := map[string]fileMeta{}
	first := true
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		writer, err := plan.scanWriter()
		if err != nil {
			events(SyncEvent{Kind: SyncError, Err: err})
		} else {
			var replica map[string]fileMeta
			if first {
				// One full listing of the replica so pre-existing extras can be
				// removed and same-size files skipped.
				replica, _ = plan.scanReplica()
			}
			syncReconcile(writer, replica, prev, first, opts, plan, events)
			prev = writer
			if first {
				events(SyncEvent{Kind: SyncReady})
				first = false
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
				localPath, err := SafeLocalJoin(localDir, rel)
				if err != nil {
					return 0, err
				}
				res, err := a.DownloadFile(serverName, remotePath, localPath, xfer, nil)
				return res.Entry.Size, err
			},
			remove: func(rel string) error {
				p, err := SafeLocalJoin(localDir, rel)
				if err != nil {
					return err
				}
				return os.Remove(p)
			},
			ensureReplicaDir: func() { _ = os.MkdirAll(localDir, 0o750) },
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
		ensureReplicaDir: func() { _ = a.RemoteMkdir(serverName, remoteDir) },
	}
}

// syncReconcile copies new/changed writer files to the replica and (unless
// NoDelete) removes replica files that the writer no longer has.
func syncReconcile(writer, replica, prev map[string]fileMeta, first bool, opts SyncOptions, plan syncPlan, events func(SyncEvent)) {
	rels := make([]string, 0, len(writer))
	for rel := range writer {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	for _, rel := range rels {
		meta := writer[rel]
		var needCopy bool
		if first {
			// Override the replica where it is missing the file or its size
			// differs (rsync-style quick check).
			r, ok := replica[rel]
			needCopy = !ok || r.size != meta.size
		} else {
			old, ok := prev[rel]
			needCopy = !ok || old != meta
		}
		if !needCopy {
			continue
		}
		bytes, err := plan.copy(rel)
		if err != nil {
			events(SyncEvent{Kind: SyncError, Path: rel, Err: err})
			continue
		}
		events(SyncEvent{Kind: SyncCopy, Path: rel, Bytes: bytes})
	}

	if opts.NoDelete {
		return
	}
	// Delete replica files absent on the writer. On the first pass compare
	// against the replica's actual contents (removes pre-existing extras);
	// afterwards compare against the previous writer snapshot (removes files the
	// writer deleted).
	base := prev
	if first {
		base = replica
	}
	gone := make([]string, 0)
	for rel := range base {
		if _, ok := writer[rel]; !ok {
			gone = append(gone, rel)
		}
	}
	sort.Strings(gone)
	for _, rel := range gone {
		if err := plan.remove(rel); err != nil {
			events(SyncEvent{Kind: SyncError, Path: rel, Err: err})
			continue
		}
		events(SyncEvent{Kind: SyncDelete, Path: rel})
	}
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
			return nil // file vanished mid-scan; skip
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
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
// {mtime,size}. A missing/empty root yields an empty map. Bounded in depth and
// total files; the agent is not trusted to be honest about its tree.
func (a *App) scanRemoteDir(serverName, root string) (map[string]fileMeta, error) {
	out := map[string]fileMeta{}
	rootRes, err := a.ListRemoteDir(serverName, root)
	if err != nil {
		return out, nil // not readable yet (e.g. just-created replica) — treat as empty
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
					continue // skip unreadable subdir
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
