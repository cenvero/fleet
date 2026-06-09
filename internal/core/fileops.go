// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/cenvero/fleet/pkg/proto"
)

// StatRemoteFile returns metadata for a single remote path.
func (a *App) StatRemoteFile(serverName, remotePath string) (proto.FileStatResult, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return proto.FileStatResult{}, err
	}
	resp, err := a.callRPC(server, proto.Envelope{
		Action:  proto.ActionFileStat,
		Payload: proto.FileStatPayload{Path: remotePath},
	})
	if err != nil {
		return proto.FileStatResult{}, err
	}
	if resp.Error != nil {
		return proto.FileStatResult{}, fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	return proto.DecodePayload[proto.FileStatResult](resp.Payload)
}

// CatRemoteFile streams a remote file to w, verifying each chunk's SHA-256. It
// returns the number of bytes written. Sequential (single channel) — intended
// for viewing, not bulk transfer; use DownloadFile for large files.
func (a *App) CatRemoteFile(serverName, remotePath string, w io.Writer) (int64, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return 0, err
	}
	var offset int64
	for {
		resp, err := a.callRPC(server, proto.Envelope{
			Action:  proto.ActionFileRead,
			Payload: proto.FileReadPayload{Path: remotePath, Offset: offset, Length: proto.MaxRawChunkBytes},
		})
		if err != nil {
			return offset, err
		}
		if resp.Error != nil {
			return offset, fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
		}
		res, err := proto.DecodePayload[proto.FileReadResult](resp.Payload)
		if err != nil {
			return offset, err
		}
		if len(res.Data) > 0 {
			if res.SHA256 != "" {
				sum := sha256.Sum256(res.Data)
				if hex.EncodeToString(sum[:]) != res.SHA256 {
					return offset, fmt.Errorf("checksum mismatch at offset %d", offset)
				}
			}
			if _, werr := w.Write(res.Data); werr != nil {
				return offset, werr
			}
			offset += int64(len(res.Data))
		}
		if res.EOF || len(res.Data) == 0 {
			return offset, nil
		}
	}
}

// TailRemoteFile returns the last tailLines lines of a remote text file
// (optionally filtered by search), reusing the agent's path-validated,
// memory-bounded log reader.
func (a *App) TailRemoteFile(serverName, remotePath string, tailLines int, search string) (proto.LogReadResult, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return proto.LogReadResult{}, err
	}
	resp, err := a.callRPC(server, proto.Envelope{
		Action:  "log.read",
		Payload: proto.LogReadPayload{Path: remotePath, TailLines: tailLines, Search: search},
	})
	if err != nil {
		return proto.LogReadResult{}, err
	}
	if resp.Error != nil {
		return proto.LogReadResult{}, fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	return proto.DecodePayload[proto.LogReadResult](resp.Payload)
}

// UploadDir recursively uploads every file under localDir into remoteDir,
// preserving the tree. Returns the number of files uploaded.
func (a *App) UploadDir(serverName, localDir, remoteDir string, opts FileTransferOptions, progress ProgressFunc) (int, error) {
	localDir = filepath.Clean(localDir)
	remoteDir = path.Clean(remoteDir)
	files, err := scanLocalDir(localDir)
	if err != nil {
		return 0, err
	}
	created := map[string]bool{remoteDir: true}
	_ = a.RemoteMkdir(serverName, remoteDir)
	count := 0
	for _, rel := range sortedRelKeys(files) {
		remotePath := path.Join(remoteDir, filepath.ToSlash(rel))
		a.ensureRemoteParent(serverName, remotePath, created)
		if _, err := a.UploadFile(serverName, filepath.Join(localDir, rel), remotePath, opts, progress); err != nil {
			return count, fmt.Errorf("%s: %w", rel, err)
		}
		count++
	}
	return count, nil
}

// DownloadDir recursively downloads every file under remoteDir into localDir.
// Remote-provided names are vetted with SafeLocalJoin so a compromised agent
// cannot escape localDir. Returns the number of files downloaded.
func (a *App) DownloadDir(serverName, remoteDir, localDir string, opts FileTransferOptions, progress ProgressFunc) (int, error) {
	remoteDir = path.Clean(remoteDir)
	files, err := a.scanRemoteDir(serverName, remoteDir)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(localDir, 0o750); err != nil {
		return 0, err
	}
	count := 0
	for _, rel := range sortedRelKeys(files) {
		localPath, err := SafeLocalJoin(localDir, rel)
		if err != nil {
			return count, err
		}
		remotePath := path.Join(remoteDir, filepath.ToSlash(rel))
		if _, err := a.DownloadFile(serverName, remotePath, localPath, opts, progress); err != nil {
			return count, fmt.Errorf("%s: %w", rel, err)
		}
		count++
	}
	return count, nil
}

func sortedRelKeys(m map[string]fileMeta) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
