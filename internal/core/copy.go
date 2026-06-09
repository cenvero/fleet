// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/pkg/proto"
)

// ListRemoteDirHidden lists a directory on a managed server, optionally including
// hidden (dot) entries. ListRemoteDir calls it with showHidden=false.
func (a *App) ListRemoteDirHidden(serverName, remotePath string, showHidden bool) (proto.FileListResult, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return proto.FileListResult{}, err
	}
	if remotePath == "" {
		remotePath = firstNonEmptyString(a.effectiveFileTransferDefaults(server).RemoteDir, "/")
	}
	resp, err := a.callRPC(server, proto.Envelope{
		Action:  proto.ActionFileList,
		Payload: proto.FileListPayload{Path: remotePath, ShowHidden: showHidden},
	})
	if err != nil {
		return proto.FileListResult{}, err
	}
	if resp.Error != nil {
		return proto.FileListResult{}, fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	return proto.DecodePayload[proto.FileListResult](resp.Payload)
}

// CopyFile copies a single file from one server to another by relaying through a
// controller-side temp file: download src -> temp -> upload to dst. Works for any
// server mode. Progress is reported as a single 0..100% bar across both legs.
func (a *App) CopyFile(srcServer, srcPath, dstServer, dstPath string, opts FileTransferOptions, progress ProgressFunc) (proto.FileFinalizeResult, error) {
	stat, err := a.StatRemoteFile(srcServer, srcPath)
	if err != nil {
		return proto.FileFinalizeResult{}, fmt.Errorf("stat source: %w", err)
	}
	size := stat.Entry.Size
	total := size * 2
	if total == 0 {
		total = 1
	}

	tmp, err := os.CreateTemp("", "fleet-relay-*")
	if err != nil {
		return proto.FileFinalizeResult{}, err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := a.DownloadFile(srcServer, srcPath, tmpPath, opts, relayProgress(progress, 0, total)); err != nil {
		return proto.FileFinalizeResult{}, fmt.Errorf("relay download: %w", err)
	}
	res, err := a.UploadFile(dstServer, tmpPath, dstPath, opts, relayProgress(progress, size, total))
	if err != nil {
		return res, fmt.Errorf("relay upload: %w", err)
	}
	if progress != nil {
		progress(ProgressUpdate{BytesDone: total, TotalBytes: total, Done: true})
	}
	_ = a.AuditLog.Append(logs.AuditEntry{
		Action:   "file.copy",
		Target:   srcServer + " -> " + dstServer,
		Operator: a.operator(),
		Details:  fmt.Sprintf("%s:%s -> %s:%s (%d bytes)", srcServer, srcPath, dstServer, dstPath, size),
	})
	return res, nil
}

// relayProgress shifts a leg's byte counter into the combined transfer total.
func relayProgress(progress ProgressFunc, base, total int64) ProgressFunc {
	if progress == nil {
		return nil
	}
	return func(u ProgressUpdate) {
		progress(ProgressUpdate{
			BytesDone:     base + u.BytesDone,
			TotalBytes:    total,
			RatePerSec:    u.RatePerSec,
			ActiveStreams: u.ActiveStreams,
		})
	}
}

// CopyDir recursively copies a directory tree from one server to another.
// Returns the number of files copied.
func (a *App) CopyDir(srcServer, srcPath, dstServer, dstPath string, opts FileTransferOptions, progress ProgressFunc) (int, error) {
	srcPath = path.Clean(srcPath)
	dstPath = path.Clean(dstPath)
	files, err := a.scanRemoteDir(srcServer, srcPath)
	if err != nil {
		return 0, err
	}
	_ = a.RemoteMkdir(dstServer, dstPath)
	created := map[string]bool{dstPath: true}
	var cmu sync.Mutex
	return runParallelTransfers(sortedRelKeys(files), sumSizes(files), progress, func(rel string, fp ProgressFunc) error {
		srcF := path.Join(srcPath, filepath.ToSlash(rel))
		dstF := path.Join(dstPath, filepath.ToSlash(rel))
		cmu.Lock()
		a.ensureRemoteParent(dstServer, dstF, created)
		cmu.Unlock()
		_, err := a.CopyFile(srcServer, srcF, dstServer, dstF, opts, fp)
		return err
	})
}

// MoveFile moves a file between servers. Within one server it is an efficient
// rename; across servers it is copy-then-delete-source.
func (a *App) MoveFile(srcServer, srcPath, dstServer, dstPath string, opts FileTransferOptions, progress ProgressFunc) error {
	if srcServer == dstServer {
		return a.RemoteRename(srcServer, srcPath, dstPath)
	}
	if _, err := a.CopyFile(srcServer, srcPath, dstServer, dstPath, opts, progress); err != nil {
		return err
	}
	if err := a.RemoteDelete(srcServer, srcPath, false); err != nil {
		return fmt.Errorf("remove source after move: %w", err)
	}
	a.auditMove(srcServer, srcPath, dstServer, dstPath)
	return nil
}

// MoveDir moves a directory tree between servers (rename within one server,
// otherwise copy-then-recursive-delete). Returns files moved (0 for a rename).
func (a *App) MoveDir(srcServer, srcPath, dstServer, dstPath string, opts FileTransferOptions, progress ProgressFunc) (int, error) {
	if srcServer == dstServer {
		return 0, a.RemoteRename(srcServer, srcPath, dstPath)
	}
	n, err := a.CopyDir(srcServer, srcPath, dstServer, dstPath, opts, progress)
	if err != nil {
		return n, err
	}
	if err := a.RemoteDelete(srcServer, srcPath, true); err != nil {
		return n, fmt.Errorf("remove source after move: %w", err)
	}
	a.auditMove(srcServer, srcPath, dstServer, dstPath)
	return n, nil
}

func (a *App) auditMove(srcServer, srcPath, dstServer, dstPath string) {
	_ = a.AuditLog.Append(logs.AuditEntry{
		Action:   "file.move",
		Target:   srcServer + " -> " + dstServer,
		Operator: a.operator(),
		Details:  fmt.Sprintf("%s:%s -> %s:%s", srcServer, srcPath, dstServer, dstPath),
	})
}

// EstimateRemoteTree returns the file count and total bytes under a remote path
// (bounded by scanRemoteDir's depth/file caps). Used for transfer confirmations.
func (a *App) EstimateRemoteTree(serverName, remotePath string) (files int, bytes int64, err error) {
	m, err := a.scanRemoteDir(serverName, path.Clean(remotePath))
	if err != nil {
		return 0, 0, err
	}
	for _, meta := range m {
		files++
		bytes += meta.size
	}
	return files, bytes, nil
}

// EstimateLocalTree returns the file count and total bytes under a local dir.
func EstimateLocalTree(dir string) (files int, bytes int64, err error) {
	m, err := scanLocalDir(dir)
	if err != nil {
		return 0, 0, err
	}
	for _, meta := range m {
		files++
		bytes += meta.size
	}
	return files, bytes, nil
}
