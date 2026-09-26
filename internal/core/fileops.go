// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cenvero/fleet/pkg/proto"
)

// dirTransferConcurrency bounds how many files a recursive directory transfer
// moves at once (each file still uses its own per-chunk parallelism). Every
// file's streams are channels on the one pooled connection to the server, and
// transferChannelBudget caps their total, so this is a concurrency knob rather
// than a connection count.
const dirTransferConcurrency = 8

func sumSizes(m map[string]fileMeta) int64 {
	var t int64
	for _, meta := range m {
		if !meta.isDir() {
			t += meta.size
		}
	}
	return t
}

// rejectDotComponents refuses a remote path naming a "." or ".." component
// before anything cleans it: `rm -r /srv/app/../..` must be an error, never a
// quiet `rm -r /`. Agents enforce the same rule for every mutating operation;
// checking here as well gives a clear error before any connection is made.
func rejectDotComponents(style TargetPathStyle, p string) error {
	isSep := func(r rune) bool { return r == '/' }
	if style.IsWindows() {
		isSep = func(r rune) bool { return r == '/' || r == '\\' }
	}
	for _, part := range strings.FieldsFunc(p, isSep) {
		if part == "." || part == ".." {
			return fmt.Errorf("invalid path %q: \".\" and \"..\" components are not allowed; give the full path", p)
		}
	}
	return nil
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

// forEachDirLevel runs fn for every directory in dirs, parents strictly before
// children, with the directories of one depth handled concurrently. Creating
// a tree used to cost one serial round trip per directory.
func forEachDirLevel(dirs []string, fn func(rel string) error) error {
	byDepth := map[int][]string{}
	var depths []int
	for _, rel := range dirs {
		d := strings.Count(rel, "/")
		if _, ok := byDepth[d]; !ok {
			depths = append(depths, d)
		}
		byDepth[d] = append(byDepth[d], rel)
	}
	sort.Ints(depths)
	for _, d := range depths {
		level := byDepth[d]
		sem := make(chan struct{}, dirTransferConcurrency)
		var wg sync.WaitGroup
		var errs firstError
		for _, rel := range level {
			wg.Add(1)
			sem <- struct{}{}
			go func(rel string) {
				defer wg.Done()
				defer func() { <-sem }()
				if err := fn(rel); err != nil {
					errs.set(err)
				}
			}(rel)
		}
		wg.Wait()
		if err := errs.get(); err != nil {
			return err
		}
	}
	return nil
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
		return proto.FileStatResult{}, remoteErr(resp.Error)
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

// catReadAhead is how many chunks CatRemoteFile requests at once while it
// still expects more data, on as many pooled channels.
const catReadAhead = 4

// CatRemoteFile streams a remote file to w, verifying each chunk's SHA-256. It
// returns the number of bytes written. Intended for viewing (it backs
// `fleet file cat` and the editors); use DownloadFile for large files. Chunks
// travel as binary frames where the agent supports them and are sized to the
// SSH channel window; a few are requested ahead so a long link is not idle
// between chunks.
func (a *App) CatRemoteFile(serverName, remotePath string, w io.Writer) (int64, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return 0, err
	}
	conn, err := a.openTransferConn(server, 1)
	if err != nil {
		return 0, err
	}
	defer conn.closeFn()
	const chunk = int64(DefaultChunkSizeBytes)
	stat, first, err := statAndFirstChunk(conn, remotePath, chunk)
	if err != nil {
		return 0, err
	}
	if err := requireRemoteRegular(stat.Entry, remotePath); err != nil {
		return 0, err
	}
	known := max(stat.Entry.Size, 0)
	if known > chunk {
		conn.grow(catReadAhead)
	}
	// Bound the number of round-trips too: a malicious agent could dribble back
	// tiny (even 1-byte) chunks to stay under the byte ceiling for a very long
	// time. Allow enough iterations to stream maxCatRemoteBytes at the
	// requested chunk size, plus generous slack, then refuse.
	maxIters := int(maxCatRemoteBytes/chunk) + 16
	var offset int64
	iter := 0
	// consume verifies and writes one reply; it reports whether the stream is
	// complete and whether the reply was a full chunk (so read-ahead replies
	// that follow it are still aligned).
	consume := func(res proto.FileReadResult) (done, full bool, err error) {
		if len(res.Data) > 0 {
			// Refuse once the total would exceed the ceiling, before writing
			// more: EOF is agent-controlled and cannot be trusted to arrive.
			if offset+int64(len(res.Data)) > maxCatRemoteBytes {
				return false, false, fmt.Errorf("aborting remote read of %q: exceeds %d-byte limit without EOF (possible misbehaving agent)", remotePath, maxCatRemoteBytes)
			}
			if res.SHA256 != "" && sha256Hex(res.Data) != res.SHA256 {
				return false, false, fmt.Errorf("checksum mismatch at offset %d", offset)
			}
			if _, werr := w.Write(res.Data); werr != nil {
				return false, false, werr
			}
			offset += int64(len(res.Data))
		}
		if res.EOF || len(res.Data) == 0 {
			return true, false, nil
		}
		return false, int64(len(res.Data)) == chunk, nil
	}
	if first != nil {
		iter++
		done, _, err := consume(*first)
		if err != nil || done {
			return offset, err
		}
	}
	for {
		// Read ahead only within the size the agent reported; past it (a file
		// that grew) continue one chunk at a time until EOF.
		n := 1
		if offset < known {
			n = int(min(int64(conn.slots()), (known-offset+chunk-1)/chunk))
		}
		results := make([]proto.FileReadResult, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for k := range n {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				results[k], errs[k] = decodeResult[proto.FileReadResult](conn.send(k, proto.Envelope{
					Action: proto.ActionFileRead,
					Payload: proto.FileReadPayload{
						Path: remotePath, Offset: offset + int64(k)*chunk, Length: chunk,
						Binary: conn.binaryFrames,
					},
				}))
			}(k)
		}
		wg.Wait()
		for k := range n {
			if iter >= maxIters {
				return offset, fmt.Errorf("aborting remote read of %q after %d chunks without EOF (possible misbehaving agent)", remotePath, iter)
			}
			iter++
			if errs[k] != nil {
				return offset, errs[k]
			}
			done, full, err := consume(results[k])
			if err != nil || done {
				return offset, err
			}
			if !full {
				break // a short reply shifts every later offset; re-plan from here
			}
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

// UploadDir recursively uploads every regular file under localDir into
// remoteDir and preserves empty directories. Returns the number of files
// uploaded (directories are not counted).
func (a *App) UploadDir(serverName, localDir, remoteDir string, opts FileTransferOptions, progress ProgressFunc) (int, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return 0, err
	}
	style := TargetPathStyleForServer(server)
	if err := rejectDotComponents(style, remoteDir); err != nil {
		return 0, err
	}
	localDir = filepath.Clean(localDir)
	remoteDir = style.Clean(remoteDir)
	if err := ValidateTargetPath(style, remoteDir); err != nil {
		return 0, err
	}
	entries, err := scanLocalDir(localDir)
	if err != nil {
		return 0, err
	}
	if err := validateTreeForStyle(entries, style); err != nil {
		return 0, err
	}
	if err := a.RemoteMkdir(serverName, remoteDir); err != nil {
		return 0, err
	}
	if err := forEachDirLevel(sortedDirKeys(entries), func(rel string) error {
		if err := a.RemoteMkdir(serverName, style.Join(remoteDir, rel)); err != nil {
			return fmt.Errorf("create remote directory %s: %w", rel, err)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	files := sortedFileKeys(entries)
	return runParallelTransfers(files, sumSizes(entries), progress, func(rel string, fp ProgressFunc) error {
		remotePath := style.Join(remoteDir, rel)
		_, err := a.UploadFile(serverName, filepath.Join(localDir, filepath.FromSlash(rel)), remotePath, opts, fp)
		return err
	})
}

// DownloadDir recursively downloads every regular file under remoteDir into
// localDir and preserves empty directories. Remote-provided names are vetted
// with SafeLocalJoin and created through os.Root so a compromised agent cannot
// escape localDir. Returns the number of files downloaded (directories are not
// counted).
func (a *App) DownloadDir(serverName, remoteDir, localDir string, opts FileTransferOptions, progress ProgressFunc) (int, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return 0, err
	}
	style := TargetPathStyleForServer(server)
	remoteDir = style.Clean(remoteDir)
	localDir = filepath.Clean(localDir)
	if err := ValidateTargetPath(NativePathStyle(), localDir); err != nil {
		return 0, err
	}
	entries, err := a.scanRemoteDir(serverName, remoteDir)
	if err != nil {
		return 0, err
	}
	if err := validateTreeForLocalDestination(entries, localDir); err != nil {
		return 0, err
	}
	root, err := openVerifiedLocalDir(localDir, 0o750)
	if err != nil {
		return 0, err
	}
	for _, rel := range sortedDirKeys(entries) {
		if err := root.MkdirAll(filepath.FromSlash(rel), 0o750); err != nil {
			_ = root.Close()
			return 0, fmt.Errorf("create local directory %s: %w", rel, err)
		}
	}
	if err := root.Close(); err != nil {
		return 0, err
	}
	files := sortedFileKeys(entries)
	return runParallelTransfers(files, sumSizes(entries), progress, func(rel string, fp ProgressFunc) error {
		localRel := filepath.FromSlash(rel)
		if _, err := SafeLocalJoin(localDir, localRel); err != nil {
			return err
		}
		remotePath := style.Join(remoteDir, rel)
		confined := opts
		confined.localRoot = localDir
		confined.localRel = rel
		_, downloadErr := a.DownloadFile(serverName, remotePath, "", confined, fp)
		return downloadErr
	})
}

func sortedFileKeys(m map[string]fileMeta) []string {
	out := make([]string, 0, len(m))
	for k, meta := range m {
		if !meta.isDir() {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func sortedDirKeys(m map[string]fileMeta) []string {
	out := make([]string, 0, len(m))
	for k, meta := range m {
		if meta.isDir() {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		leftDepth := strings.Count(out[i], "/")
		rightDepth := strings.Count(out[j], "/")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return out[i] < out[j]
	})
	return out
}
