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
	"sync"
	"sync/atomic"

	"github.com/cenvero/fleet/pkg/proto"
)

// dirTransferConcurrency bounds how many files a recursive directory transfer
// moves at once (each file still uses its own per-chunk parallelism).
const dirTransferConcurrency = 4

func sumSizes(m map[string]fileMeta) int64 {
	var t int64
	for _, meta := range m {
		t += meta.size
	}
	return t
}

// runParallelTransfers transfers rels with bounded concurrency, aggregating byte
// progress across the in-flight files. xfer(rel, fileProgress) moves one file.
// Returns the count transferred and the first error (stops scheduling on error).
func runParallelTransfers(rels []string, totalBytes int64, progress ProgressFunc, xfer func(rel string, fp ProgressFunc) error) (int, error) {
	sem := make(chan struct{}, dirTransferConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	var doneBytes, doneCount, active int64
	emit := func() {
		if progress != nil {
			progress(ProgressUpdate{
				BytesDone:     atomic.LoadInt64(&doneBytes),
				TotalBytes:    totalBytes,
				ActiveStreams: int(atomic.LoadInt64(&active)),
			})
		}
	}
	for _, rel := range rels {
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		atomic.AddInt64(&active, 1)
		go func(rel string) {
			defer wg.Done()
			defer func() { atomic.AddInt64(&active, -1); <-sem }()
			// UploadFile/DownloadFile invoke fp concurrently from each per-file
			// sender goroutine, so the running total must be tracked atomically.
			var fileLast atomic.Int64
			fp := func(u ProgressUpdate) {
				atomic.AddInt64(&doneBytes, u.BytesDone-fileLast.Swap(u.BytesDone))
				emit()
			}
			if err := xfer(rel, fp); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", rel, err)
				}
				mu.Unlock()
				return
			}
			atomic.AddInt64(&doneCount, 1)
			emit()
		}(rel)
	}
	wg.Wait()
	if progress != nil {
		progress(ProgressUpdate{BytesDone: atomic.LoadInt64(&doneBytes), TotalBytes: totalBytes, Done: firstErr == nil})
	}
	return int(atomic.LoadInt64(&doneCount)), firstErr
}

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

// maxCatRemoteBytes bounds the total bytes CatRemoteFile will stream from one
// remote file. EOF is agent-controlled, so a malicious agent could return
// non-empty data forever without ever setting EOF; this ceiling (and the
// derived iteration cap below) forces the loop to terminate. 4 GiB is far
// larger than anything this "view a file" path is meant for — use DownloadFile,
// which is size-driven by the file's stat, for bulk transfer. A var (not a
// const) only so tests can lower it; production code never reassigns it.
var maxCatRemoteBytes = int64(4) * 1024 * 1024 * 1024

// CatRemoteFile streams a remote file to w, verifying each chunk's SHA-256. It
// returns the number of bytes written. Sequential (single channel) — intended
// for viewing, not bulk transfer; use DownloadFile for large files.
func (a *App) CatRemoteFile(serverName, remotePath string, w io.Writer) (int64, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return 0, err
	}
	// Bound the number of round-trips too: a malicious agent could dribble back
	// tiny (even 1-byte) chunks to stay under the byte ceiling for a very long
	// time. Allow enough iterations to stream maxCatRemoteBytes at the protocol's
	// max chunk size, plus generous slack, then refuse.
	maxIters := int(maxCatRemoteBytes/proto.MaxRawChunkBytes) + 16
	var offset int64
	for iter := 0; ; iter++ {
		if iter >= maxIters {
			return offset, fmt.Errorf("aborting remote read of %q after %d chunks without EOF (possible misbehaving agent)", remotePath, iter)
		}
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
			// Refuse once the total would exceed the ceiling, before writing more:
			// EOF is agent-controlled and cannot be trusted to ever arrive.
			if offset+int64(len(res.Data)) > maxCatRemoteBytes {
				return offset, fmt.Errorf("aborting remote read of %q: exceeds %d-byte limit without EOF (possible misbehaving agent)", remotePath, maxCatRemoteBytes)
			}
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
	var cmu sync.Mutex
	_ = a.RemoteMkdir(serverName, remoteDir)
	return runParallelTransfers(sortedRelKeys(files), sumSizes(files), progress, func(rel string, fp ProgressFunc) error {
		remotePath := path.Join(remoteDir, filepath.ToSlash(rel))
		cmu.Lock()
		a.ensureRemoteParent(serverName, remotePath, created)
		cmu.Unlock()
		_, err := a.UploadFile(serverName, filepath.Join(localDir, rel), remotePath, opts, fp)
		return err
	})
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
	return runParallelTransfers(sortedRelKeys(files), sumSizes(files), progress, func(rel string, fp ProgressFunc) error {
		localPath, err := SafeLocalJoin(localDir, rel)
		if err != nil {
			return err
		}
		remotePath := path.Join(remoteDir, filepath.ToSlash(rel))
		_, err = a.DownloadFile(serverName, remotePath, localPath, opts, fp)
		return err
	})
}

func sortedRelKeys(m map[string]fileMeta) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
