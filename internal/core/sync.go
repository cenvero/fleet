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

type fileKind uint8

const (
	fileKindRegular fileKind = iota
	fileKindDirectory
)

type fileMeta struct {
	kind        fileKind
	modUnixNano int64
	size        int64
}

func (m fileMeta) isDir() bool { return m.kind == fileKindDirectory }

// syncPlan abstracts the writer/replica sides so one loop drives both push and
// pull.
type syncPlan struct {
	scanWriter         func() (map[string]fileMeta, error)
	scanReplica        func() (map[string]fileMeta, error)
	copy               func(rel string, meta fileMeta) (int64, error) // writer -> replica
	remove             func(rel string, meta fileMeta) error          // delete on replica
	ensureReplicaDir   func() error
	transientCopyError func(error) bool
}

// syncPending tracks operations that failed after a successful writer scan.
// The writer snapshot may still advance, but these entries force another
// attempt even when the source metadata does not change again.
type syncPending struct {
	copies          map[string]struct{}
	deletes         map[string]fileMeta
	requiredDeletes map[string]bool
}

func newSyncPending() *syncPending {
	return &syncPending{
		copies:          make(map[string]struct{}),
		deletes:         make(map[string]fileMeta),
		requiredDeletes: make(map[string]bool),
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
	server, err := a.GetServer(serverName)
	if err != nil {
		return err
	}
	style := TargetPathStyleForServer(server)
	localDir = filepath.Clean(localDir)
	remoteDir = style.Clean(remoteDir)
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultSyncInterval
	}
	pull := opts.From == SyncFromRemote
	replicaStyle := style
	replicaCaseInsensitive := style.IsWindows()

	if pull {
		if err := ValidateTargetPath(NativePathStyle(), localDir); err != nil {
			return err
		}
		replicaStyle = NativePathStyle()
		replicaCaseInsensitive, err = LocalPathCaseInsensitive(localDir)
		if err != nil {
			return err
		}
	} else {
		if err := ValidateTargetPath(style, remoteDir); err != nil {
			return err
		}
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

	plan := a.makeSyncPlan(serverName, localDir, remoteDir, opts, pull, style)

	prev := map[string]fileMeta{}
	pending := newSyncPending()
	first := true
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		writer, err := plan.scanWriter()
		if err == nil {
			err = validateTreeForStyleCase(writer, replicaStyle, replicaCaseInsensitive)
		}
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

func (a *App) makeSyncPlan(serverName, localDir, remoteDir string, opts SyncOptions, pull bool, style TargetPathStyle) syncPlan {
	xfer := FileTransferOptions{Parallel: opts.Parallel}
	if pull {
		return syncPlan{
			scanWriter:  func() (map[string]fileMeta, error) { return a.scanRemoteDir(serverName, remoteDir) },
			scanReplica: func() (map[string]fileMeta, error) { return scanLocalDir(localDir) },
			copy: func(rel string, meta fileMeta) (int64, error) {
				localRel := filepath.FromSlash(rel)
				if _, err := SafeLocalJoin(localDir, localRel); err != nil {
					return 0, err
				}
				if meta.isDir() {
					root, err := openVerifiedLocalDir(localDir, 0o750)
					if err != nil {
						return 0, err
					}
					defer root.Close()
					return 0, root.MkdirAll(localRel, 0o750)
				}
				remotePath := style.Join(remoteDir, rel)
				confined := xfer
				confined.localRoot = localDir
				confined.localRel = rel
				res, err := a.DownloadFile(serverName, remotePath, "", confined, nil)
				return res.Entry.Size, err
			},
			remove: func(rel string, _ fileMeta) error {
				localRel := filepath.FromSlash(rel)
				if _, err := SafeLocalJoin(localDir, localRel); err != nil {
					return err
				}
				return RemoveLocalUnder(localDir, localRel, false)
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
	return syncPlan{
		scanWriter:  func() (map[string]fileMeta, error) { return scanLocalDir(localDir) },
		scanReplica: func() (map[string]fileMeta, error) { return a.scanRemoteDir(serverName, remoteDir) },
		copy: func(rel string, meta fileMeta) (int64, error) {
			remotePath := style.Join(remoteDir, rel)
			if meta.isDir() {
				return 0, a.RemoteMkdir(serverName, remotePath)
			}
			res, err := a.UploadFile(serverName, filepath.Join(localDir, filepath.FromSlash(rel)), remotePath, xfer, nil)
			return res.Size, err
		},
		remove: func(rel string, _ fileMeta) error {
			return a.RemoteDelete(serverName, style.Join(remoteDir, rel), false)
		},
		ensureReplicaDir:   func() error { return a.RemoteMkdir(serverName, remoteDir) },
		transientCopyError: func(err error) bool { return errors.Is(err, os.ErrNotExist) },
	}
}

// syncReconcile copies new/changed writer entries to the replica and (unless
// NoDelete) removes replica entries that the writer no longer has. Deletions
// run child-before-parent; directory creation runs parent-before-child; regular
// files transfer only after their parent directories exist. It returns true
// when every operation selected for this pass succeeded.
func syncReconcile(writer, replica, prev map[string]fileMeta, first bool, opts SyncOptions, plan syncPlan, pending *syncPending, events func(SyncEvent)) bool {
	complete := true
	base := prev
	if first {
		base = replica
	}

	// Remove absent entries and type conflicts before creating replacements.
	// A complete writer scan is a prerequisite for calling syncReconcile, so an
	// incomplete scan can never turn an omitted path into a deletion.
	for rel, meta := range writer {
		if pendingMeta, ok := pending.deletes[rel]; ok && pendingMeta.kind == meta.kind {
			delete(pending.deletes, rel)
			delete(pending.requiredDeletes, rel)
		}
	}
	if opts.NoDelete {
		for rel, meta := range pending.deletes {
			if !pending.requiredDeletes[rel] || !syncDeleteRequired(writer, rel, meta) {
				delete(pending.deletes, rel)
				delete(pending.requiredDeletes, rel)
			}
		}
	}
	removeSet := make(map[string]fileMeta, len(pending.deletes))
	requiredSet := make(map[string]bool)
	for rel, meta := range pending.deletes {
		removeSet[rel] = meta
		requiredSet[rel] = pending.requiredDeletes[rel]
	}
	for rel, old := range base {
		current, exists := writer[rel]
		conflict := exists && current.kind != old.kind
		if (!exists && !opts.NoDelete) || conflict {
			removeSet[rel] = old
			requiredSet[rel] = conflict
		}
		if conflict && old.isDir() && !current.isDir() {
			prefix := rel + "/"
			for child, childMeta := range base {
				if strings.HasPrefix(child, prefix) {
					removeSet[child] = childMeta
					requiredSet[child] = true
				}
			}
		}
	}
	for _, rel := range sortedMetaKeys(removeSet, true) {
		meta := removeSet[rel]
		if err := plan.remove(rel, meta); err != nil {
			pending.deletes[rel] = meta
			pending.requiredDeletes[rel] = requiredSet[rel]
			complete = false
			events(SyncEvent{Kind: SyncError, Path: rel, Err: err})
			continue
		}
		delete(pending.deletes, rel)
		delete(pending.requiredDeletes, rel)
		events(SyncEvent{Kind: SyncDelete, Path: rel})
	}

	// Create directories first, then transfer regular files. Type-conflict
	// removals that failed remain pending and block replacing that exact path.
	for _, rel := range sortedMetaKeys(writer, false) {
		meta := writer[rel]
		if _, blocked := pending.deletes[rel]; blocked {
			complete = false
			continue
		}
		_, retry := pending.copies[rel]
		needCopy := retry
		if first {
			r, ok := replica[rel]
			needCopy = needCopy || !ok || r.kind != meta.kind || (!meta.isDir() && r.size != meta.size)
		} else {
			old, ok := prev[rel]
			needCopy = needCopy || !ok || old != meta
		}
		if !needCopy {
			continue
		}
		bytes, err := plan.copy(rel, meta)
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
	return complete
}

// syncDeleteRequired reports whether a no-delete mirror must still remove an
// entry to resolve a file/directory type collision. Descendants of a replica
// directory being replaced by a writer file are required removals too.
func syncDeleteRequired(writer map[string]fileMeta, rel string, replicaMeta fileMeta) bool {
	if current, ok := writer[rel]; ok {
		return current.kind != replicaMeta.kind
	}
	parts := strings.Split(rel, "/")
	for i := len(parts) - 1; i > 0; i-- {
		ancestor := strings.Join(parts[:i], "/")
		if current, ok := writer[ancestor]; ok && !current.isDir() {
			return true
		}
	}
	return false
}

// sortedMetaKeys orders directories parent-before-child and before regular
// files for creation/copy. With deepestFirst it reverses that safety order so
// files and child directories are removed before their parents.
func sortedMetaKeys(entries map[string]fileMeta, deepestFirst bool) []string {
	out := make([]string, 0, len(entries))
	for rel := range entries {
		out = append(out, rel)
	}
	depth := func(rel string) int { return strings.Count(rel, "/") }
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		ad, bd := depth(a), depth(b)
		if ad != bd {
			if deepestFirst {
				return ad > bd
			}
			return ad < bd
		}
		if entries[a].isDir() != entries[b].isDir() {
			if deepestFirst {
				return !entries[a].isDir()
			}
			return entries[a].isDir()
		}
		return a < b
	})
	return out
}

// scanLocalDir returns every regular file and directory under root. Symlinks
// and special files fail the entire scan so callers never operate on a partial
// writer snapshot. Hidden entries are included.
func scanLocalDir(root string) (map[string]fileMeta, error) {
	out := map[string]fileMeta{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink in recursive tree: %s", p)
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", p, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink in recursive tree: %s", p)
		}
		if p == root {
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", root)
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return fmt.Errorf("resolve relative path for %s: %w", p, err)
		}
		key, err := cleanRelativeKey(filepath.ToSlash(rel))
		if err != nil {
			return fmt.Errorf("resolve safe relative path for %s: %w", p, err)
		}
		if info.IsDir() {
			out[key] = fileMeta{kind: fileKindDirectory}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular file in recursive tree: %s", p)
		}
		out[key] = fileMeta{kind: fileKindRegular, modUnixNano: info.ModTime().UnixNano(), size: info.Size()}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// cleanRelativeKey validates the internal slash-separated relative-key form.
// Callers normalize their actual separator first (filepath.ToSlash locally;
// TargetPathStyle.Relative remotely). A remaining backslash is therefore a
// literal POSIX filename character that Windows cannot represent faithfully in
// a mixed-target transfer; reject it instead of silently turning it into a
// directory separator.
func cleanRelativeKey(value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\\\x00\n\r") {
		return "", fmt.Errorf("unsafe or non-portable relative path %q", value)
	}
	if strings.HasPrefix(value, "/") || TargetPathWindows.IsAbs(value) {
		return "", fmt.Errorf("unsafe relative path %q", value)
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", fmt.Errorf("unsafe relative path %q", value)
		}
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe relative path %q", value)
	}
	return clean, nil
}

// validateTreeForStyle verifies that every slash-neutral relative entry can be
// represented injectively on the destination. It runs before destination roots
// or directories are created, preventing partial copies when a POSIX source has
// names that collide or are invalid on a Windows target.
func validateTreeForStyle(entries map[string]fileMeta, style TargetPathStyle) error {
	return validateTreeForStyleCase(entries, style, style.IsWindows())
}

func validateTreeForLocalDestination(entries map[string]fileMeta, destination string) error {
	caseInsensitive, err := LocalPathCaseInsensitive(destination)
	if err != nil {
		return err
	}
	return validateTreeForStyleCase(entries, NativePathStyle(), caseInsensitive)
}

func validateTreeForStyleCase(entries map[string]fileMeta, style TargetPathStyle, caseInsensitive bool) error {
	// Some native POSIX-syntax filesystems (notably default macOS APFS) resolve
	// components case-insensitively. Track implicit prefixes when either the path
	// style or the concrete destination filesystem folds case.
	normalized := make(map[string]string, len(entries))
	metaByKey := make(map[string]fileMeta, len(entries))
	for rel, meta := range entries {
		clean, err := cleanRelativeKey(rel)
		if err != nil {
			return fmt.Errorf("invalid relative path %q: %w", rel, err)
		}
		parts := strings.Split(clean, "/")
		for _, part := range parts {
			if err := ValidateTargetPathComponent(style, part); err != nil {
				return fmt.Errorf("destination cannot represent %q: %w", rel, err)
			}
		}
		for i := range parts {
			prefix := strings.Join(parts[:i+1], "/")
			key := prefix
			if caseInsensitive {
				key = strings.ToLower(key)
			}
			if previous, exists := normalized[key]; exists && previous != prefix {
				return fmt.Errorf("destination path collision between %q and %q", previous, prefix)
			}
			normalized[key] = prefix
		}
		key := clean
		if caseInsensitive {
			key = strings.ToLower(key)
		}
		metaByKey[key] = meta
	}
	for key := range metaByKey {
		parts := strings.Split(key, "/")
		for i := 1; i < len(parts); i++ {
			ancestor := strings.Join(parts[:i], "/")
			if ancestorMeta, exists := metaByKey[ancestor]; exists && !ancestorMeta.isDir() {
				return fmt.Errorf("destination path %q conflicts with file %q", normalized[key], normalized[ancestor])
			}
		}
	}
	return nil
}

// maxRemoteScanDepth and maxRemoteScanFiles bound a recursive remote listing so
// a malicious or malformed agent cannot exhaust the controller's stack/memory
// with a self-referential or enormous directory tree.
const (
	maxRemoteScanDepth = 64
	maxRemoteScanFiles = 1_000_000
)

func requireRemoteRegular(entry proto.FileEntry, remotePath string) error {
	kind, err := remoteEntryKind(entry)
	if err != nil {
		return err
	}
	if kind != fileKindRegular {
		return fmt.Errorf("%s is a directory", remotePath)
	}
	return nil
}

func remoteEntryKind(entry proto.FileEntry) (fileKind, error) {
	if entry.Type == "" {
		return 0, fmt.Errorf("refusing unclassified remote entry %s: agent must be updated to report file types", entry.Path)
	}
	if entry.IsSymlink || entry.Type == proto.FileEntryTypeSymlink {
		return 0, fmt.Errorf("refusing symlink in recursive remote tree: %s", entry.Path)
	}
	switch entry.Type {
	case proto.FileEntryTypeDirectory:
		return fileKindDirectory, nil
	case proto.FileEntryTypeRegular:
		return fileKindRegular, nil
	case proto.FileEntryTypeOther:
		return 0, fmt.Errorf("refusing special file in recursive remote tree: %s", entry.Path)
	default:
		return 0, fmt.Errorf("refusing unknown remote entry type %q for %s", entry.Type, entry.Path)
	}
}

// scanRemoteDir recursively lists a remote directory tree as relpath ->
// metadata. Listing errors, symlinks, and unsafe entries fail the entire scan so
// a partial writer tree can never be mistaken for deletions. Hidden entries are
// always included. The walk is bounded in depth and total entries; the agent is
// not trusted to be honest about its tree.
func (a *App) scanRemoteDir(serverName, root string) (map[string]fileMeta, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return nil, err
	}
	style := TargetPathStyleForServer(server)
	out := map[string]fileMeta{}
	rootRes, err := a.ListRemoteDirHidden(serverName, root, true)
	if err != nil {
		return nil, fmt.Errorf("list remote sync root %s: %w", root, err)
	}
	resolvedRoot := rootRes.Path
	if resolvedRoot == "" {
		resolvedRoot = root
	}
	resolvedRoot = style.Clean(resolvedRoot)

	var visit func(entries []proto.FileEntry, depth int) error
	visit = func(entries []proto.FileEntry, depth int) error {
		if depth > maxRemoteScanDepth {
			return fmt.Errorf("remote directory tree exceeds maximum depth %d", maxRemoteScanDepth)
		}
		for _, e := range entries {
			if len(out) >= maxRemoteScanFiles {
				return fmt.Errorf("remote directory tree exceeds maximum of %d entries", maxRemoteScanFiles)
			}
			rel, err := style.Relative(resolvedRoot, e.Path)
			if err != nil {
				return fmt.Errorf("remote path %q is outside scan root %q: %w", e.Path, resolvedRoot, err)
			}
			key, err := cleanRelativeKey(rel)
			if err != nil {
				return fmt.Errorf("remote path %q is not safely representable: %w", e.Path, err)
			}
			kind, err := remoteEntryKind(e)
			if err != nil {
				return err
			}
			if kind == fileKindDirectory {
				out[key] = fileMeta{kind: fileKindDirectory}
				sub, err := a.ListRemoteDirHidden(serverName, e.Path, true)
				if err != nil {
					return fmt.Errorf("list remote sync directory %s: %w", e.Path, err)
				}
				if err := visit(sub.Entries, depth+1); err != nil {
					return err
				}
				continue
			}
			out[key] = fileMeta{kind: fileKindRegular, modUnixNano: e.ModTime.UnixNano(), size: e.Size}
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
