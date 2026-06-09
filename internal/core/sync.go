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
)

// DefaultSyncInterval is how often the local directory is re-scanned for changes.
const DefaultSyncInterval = time.Second

// SyncOptions configures a live directory sync.
type SyncOptions struct {
	Interval time.Duration // re-scan interval (default 1s)
	Delete   bool          // remove remote files that were deleted locally
	Parallel int           // parallel streams per file upload
}

// SyncEventKind classifies a sync event.
type SyncEventKind string

const (
	SyncUpload SyncEventKind = "upload"
	SyncDelete SyncEventKind = "delete"
	SyncError  SyncEventKind = "error"
	SyncReady  SyncEventKind = "ready" // initial scan complete, now watching
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

// SyncDir keeps localDir mirrored to remoteDir on serverName: it pushes the full
// directory once, then re-scans every Interval and uploads new/changed files
// (and deletes remote files removed locally when Delete is set). It is one-way
// (local → remote) and runs until ctx is cancelled, returning ctx.Err().
func (a *App) SyncDir(ctx context.Context, serverName, localDir, remoteDir string, opts SyncOptions, events func(SyncEvent)) error {
	if events == nil {
		events = func(SyncEvent) {}
	}
	info, err := os.Stat(localDir)
	if err != nil {
		return fmt.Errorf("local directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", localDir)
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

	// Ensure the remote base directory exists (best effort; created per-file too).
	_ = a.RemoteMkdir(serverName, remoteDir)
	createdDirs := map[string]bool{remoteDir: true}

	snapshot := map[string]fileMeta{}
	first := true

	_ = a.AuditLog.Append(logs.AuditEntry{
		Action:   "file.sync.start",
		Target:   serverName,
		Operator: a.operator(),
		Details:  fmt.Sprintf("%s -> %s", localDir, remoteDir),
	})

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		current, scanErr := scanLocalDir(localDir)
		if scanErr != nil {
			events(SyncEvent{Kind: SyncError, Err: scanErr})
		} else {
			a.syncReconcile(serverName, localDir, remoteDir, opts, snapshot, current, createdDirs, events)
			snapshot = current
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
				Details:  fmt.Sprintf("%s -> %s", localDir, remoteDir),
			})
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// syncReconcile uploads files that are new or changed since the previous scan,
// and (optionally) deletes remote files that disappeared locally.
func (a *App) syncReconcile(serverName, localDir, remoteDir string, opts SyncOptions, prev, current map[string]fileMeta, createdDirs map[string]bool, events func(SyncEvent)) {
	// Deterministic order makes output and tests stable.
	rels := make([]string, 0, len(current))
	for rel := range current {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	for _, rel := range rels {
		meta := current[rel]
		if old, ok := prev[rel]; ok && old == meta {
			continue // unchanged
		}
		remotePath := path.Join(remoteDir, filepath.ToSlash(rel))
		a.ensureRemoteParent(serverName, remotePath, createdDirs)
		res, err := a.UploadFile(serverName, filepath.Join(localDir, rel), remotePath, FileTransferOptions{Parallel: opts.Parallel}, nil)
		if err != nil {
			events(SyncEvent{Kind: SyncError, Path: rel, Err: err})
			continue
		}
		events(SyncEvent{Kind: SyncUpload, Path: rel, Bytes: res.Size})
	}

	if opts.Delete {
		gone := make([]string, 0)
		for rel := range prev {
			if _, ok := current[rel]; !ok {
				gone = append(gone, rel)
			}
		}
		sort.Strings(gone)
		for _, rel := range gone {
			remotePath := path.Join(remoteDir, filepath.ToSlash(rel))
			if err := a.RemoteDelete(serverName, remotePath, false); err != nil {
				events(SyncEvent{Kind: SyncError, Path: rel, Err: err})
				continue
			}
			events(SyncEvent{Kind: SyncDelete, Path: rel})
		}
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
// Directories and symlinks are skipped (symlinks are not followed). Hidden files
// are included; VCS metadata (.git) is skipped to avoid syncing repo internals.
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

// SyncTargetSummary is a small helper for the CLI banner.
func SyncTargetSummary(server, localDir, remoteDir string) string {
	return fmt.Sprintf("%s → %s:%s", localDir, server, strings.TrimRight(remoteDir, "/"))
}
