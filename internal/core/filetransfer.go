// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
)

const (
	// DefaultParallelStreams is the fallback number of concurrent fleet-rpc
	// channels a direct-mode transfer opens when nothing else is configured.
	DefaultParallelStreams = 4
	// DefaultChunkSizeBytes is the fallback raw chunk size (pre-base64).
	DefaultChunkSizeBytes = 4 * 1024 * 1024 // 4 MiB
)

// transferRetryDelay is the backoff between per-chunk retry attempts. It
// mirrors the SSH reconnect delay in production and is lowered by tests.
var transferRetryDelay = sshReconnectDelay

// FileTransferOptions overrides the resolved per-server/global defaults for a
// single transfer. Zero fields inherit the resolved default.
type FileTransferOptions struct {
	Parallel  int
	ChunkSize int64
	RemoteDir string
}

// ProgressUpdate is emitted periodically during a transfer.
type ProgressUpdate struct {
	BytesDone     int64   `json:"bytes_done"`
	TotalBytes    int64   `json:"total_bytes"`
	RatePerSec    float64 `json:"rate_per_sec"`
	ActiveStreams int     `json:"active_streams"`
	Done          bool    `json:"done"`
	Err           string  `json:"error,omitempty"`
}

// ProgressFunc receives transfer progress. It may be nil. It can be called
// concurrently from worker goroutines, so implementations must be safe for that.
type ProgressFunc func(ProgressUpdate)

// effectiveFileTransferDefaults merges per-server overrides over the global
// runtime defaults over the hard-coded engine defaults. Chunk size is clamped
// to proto.MaxRawChunkBytes.
func (a *App) effectiveFileTransferDefaults(server ServerRecord) FileTransferDefaults {
	global := a.Config.Runtime.FileTransfer
	out := FileTransferDefaults{
		RemoteDir:       firstNonEmptyString(server.FileTransfer.RemoteDir, global.RemoteDir),
		ParallelStreams: firstPositiveInt(server.FileTransfer.ParallelStreams, global.ParallelStreams, DefaultParallelStreams),
		ChunkSizeBytes:  firstPositiveInt64(server.FileTransfer.ChunkSizeBytes, global.ChunkSizeBytes, DefaultChunkSizeBytes),
	}
	if out.ChunkSizeBytes > proto.MaxRawChunkBytes {
		out.ChunkSizeBytes = proto.MaxRawChunkBytes
	}
	return out
}

// FileTransferDefaultsFor returns the merged (per-server over global over
// engine) file-transfer defaults for a server. Used by the CLI and web UI to
// display effective settings.
func (a *App) FileTransferDefaultsFor(serverName string) (FileTransferDefaults, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return FileTransferDefaults{}, err
	}
	return a.effectiveFileTransferDefaults(server), nil
}

func (a *App) resolveTransferOptions(server ServerRecord, opts FileTransferOptions) FileTransferOptions {
	d := a.effectiveFileTransferDefaults(server)
	resolved := FileTransferOptions{
		Parallel:  firstPositiveInt(opts.Parallel, d.ParallelStreams),
		ChunkSize: firstPositiveInt64(opts.ChunkSize, d.ChunkSizeBytes),
		RemoteDir: firstNonEmptyString(opts.RemoteDir, d.RemoteDir),
	}
	if resolved.ChunkSize > proto.MaxRawChunkBytes {
		resolved.ChunkSize = proto.MaxRawChunkBytes
	}
	if server.Mode == transport.ModeReverse {
		// The reverse hub holds a single mutex-serialized channel — parallel
		// streams are impossible without protocol multiplexing. Transfers stay
		// chunked + resumable + checksummed, just single-stream.
		resolved.Parallel = 1
	}
	return resolved
}

// ---- simple lightweight RPC browsing (one-shot, no channel pool) ----

// ListRemoteDir lists a directory on a managed server.
func (a *App) ListRemoteDir(serverName, remotePath string) (proto.FileListResult, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return proto.FileListResult{}, err
	}
	if remotePath == "" {
		remotePath = firstNonEmptyString(a.effectiveFileTransferDefaults(server).RemoteDir, "/")
	}
	resp, err := a.callRPC(server, proto.Envelope{
		Action:  proto.ActionFileList,
		Payload: proto.FileListPayload{Path: remotePath},
	})
	if err != nil {
		return proto.FileListResult{}, err
	}
	if resp.Error != nil {
		return proto.FileListResult{}, fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	return proto.DecodePayload[proto.FileListResult](resp.Payload)
}

// RemoteMkdir creates a directory on a managed server.
func (a *App) RemoteMkdir(serverName, remotePath string) error {
	return a.simpleFileOp(serverName, proto.ActionFileMkdir, proto.FileMkdirPayload{Path: remotePath}, "file.mkdir", remotePath)
}

// RemoteDelete removes a file or directory on a managed server.
func (a *App) RemoteDelete(serverName, remotePath string, recursive bool) error {
	return a.simpleFileOp(serverName, proto.ActionFileDelete, proto.FileDeletePayload{Path: remotePath, Recursive: recursive}, "file.delete", remotePath)
}

// RemoteRename renames/moves a path on a managed server.
func (a *App) RemoteRename(serverName, from, to string) error {
	return a.simpleFileOp(serverName, proto.ActionFileRename, proto.FileRenamePayload{From: from, To: to}, "file.rename", from+" -> "+to)
}

func (a *App) simpleFileOp(serverName, action string, payload any, auditAction, target string) error {
	server, err := a.GetServer(serverName)
	if err != nil {
		return err
	}
	resp, err := a.callRPC(server, proto.Envelope{Action: action, Payload: payload})
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	_ = a.AuditLog.Append(logs.AuditEntry{
		Action:   auditAction,
		Target:   serverName,
		Operator: a.operator(),
		Details:  target,
	})
	return nil
}

// ---- chunked / parallel / resumable transfers ----

type chunkSpec struct {
	offset int64
	length int64
}

func buildChunks(total, chunkSize int64) []chunkSpec {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSizeBytes
	}
	chunks := make([]chunkSpec, 0, total/chunkSize+1)
	for off := int64(0); off < total; off += chunkSize {
		length := chunkSize
		if off+length > total {
			length = total - off
		}
		chunks = append(chunks, chunkSpec{offset: off, length: length})
	}
	return chunks
}

type senderFunc func(proto.Envelope) (proto.Envelope, error)

// transferConn abstracts the RPC surface for a transfer. For direct mode it
// wraps a pool of N fleet-rpc channels on a single SSH client; for reverse mode
// it is a single serialized caller over the reverse hub. reopen mints a
// replacement sender when a channel dies (direct mode); for reverse it returns
// the existing caller.
type transferConn struct {
	senders []senderFunc
	reopen  func() (senderFunc, error)
	closeFn func()
}

func (a *App) openTransferConn(server ServerRecord, parallel int) (*transferConn, error) {
	if server.Mode == transport.ModeReverse {
		var mu sync.Mutex
		send := func(env proto.Envelope) (proto.Envelope, error) {
			mu.Lock()
			defer mu.Unlock()
			return a.callRPC(server, env)
		}
		return &transferConn{
			senders: []senderFunc{send},
			reopen:  func() (senderFunc, error) { return send, nil },
			closeFn: func() {},
		}, nil
	}

	root, _, err := a.openDirectSession(server, false)
	if err != nil {
		return nil, err
	}
	pool := []*transport.Session{root}
	for i := 1; i < parallel; i++ {
		child, err := root.OpenChannelSession()
		if err != nil {
			break // fewer channels than requested is fine; proceed with what we have
		}
		pool = append(pool, child)
	}
	senders := make([]senderFunc, 0, len(pool))
	for _, s := range pool {
		sess := s
		senders = append(senders, func(env proto.Envelope) (proto.Envelope, error) {
			return sess.Call(context.Background(), env)
		})
	}
	// Channels minted on retry are tracked so they are closed with the rest;
	// otherwise they leaked until the whole SSH client was torn down.
	var extraMu sync.Mutex
	var extra []*transport.Session
	return &transferConn{
		senders: senders,
		reopen: func() (senderFunc, error) {
			child, err := root.OpenChannelSession()
			if err != nil {
				return nil, err
			}
			extraMu.Lock()
			extra = append(extra, child)
			extraMu.Unlock()
			return func(env proto.Envelope) (proto.Envelope, error) {
				return child.Call(context.Background(), env)
			}, nil
		},
		closeFn: func() {
			for i := 1; i < len(pool); i++ {
				_ = pool[i].Close()
			}
			extraMu.Lock()
			for _, c := range extra {
				_ = c.Close()
			}
			extraMu.Unlock()
			_ = root.Close()
		},
	}, nil
}

// callRetry runs a request on sender index idx with bounded retries, minting a
// fresh channel between attempts. It must not run concurrently with the worker
// pool reading the same sender index — only used for control calls (open_write,
// probe, finalize, stat) which bracket the worker phase.
func (c *transferConn) callRetry(idx int, action string, payload any) (proto.Envelope, error) {
	send := c.senders[idx]
	var resp proto.Envelope
	var err error
	for attempt := 0; attempt <= sshMaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(transferRetryDelay)
			if ns, rerr := c.reopen(); rerr == nil {
				send = ns
				c.senders[idx] = ns
			}
		}
		resp, err = send(proto.Envelope{Action: action, Payload: payload})
		if err == nil {
			return resp, nil
		}
	}
	return resp, err
}

func decodeResult[R any](resp proto.Envelope, err error) (R, error) {
	var zero R
	if err != nil {
		return zero, err
	}
	if resp.Error != nil {
		return zero, fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	return proto.DecodePayload[R](resp.Payload)
}

// UploadFile uploads localPath to remotePath on serverName, chunked, parallel
// (direct mode), checksummed, and resumable. remotePath may be empty (use the
// default remote dir + local base name) or end with "/" (treat as a directory).
func (a *App) UploadFile(serverName, localPath, remotePath string, opts FileTransferOptions, progress ProgressFunc) (proto.FileFinalizeResult, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return proto.FileFinalizeResult{}, err
	}
	resolved := a.resolveTransferOptions(server, opts)
	target, err := resolveUploadRemotePath(resolved.RemoteDir, remotePath, localPath)
	if err != nil {
		return proto.FileFinalizeResult{}, err
	}

	lf, err := os.Open(localPath) // #nosec G304 -- operator-supplied local path
	if err != nil {
		return proto.FileFinalizeResult{}, fmt.Errorf("open local file: %w", err)
	}
	defer lf.Close()
	info, err := lf.Stat()
	if err != nil {
		return proto.FileFinalizeResult{}, err
	}
	if info.IsDir() {
		return proto.FileFinalizeResult{}, fmt.Errorf("%s is a directory; recursive upload is not supported", localPath)
	}
	totalSize := info.Size()
	wholeSum, err := streamSHA256(lf)
	if err != nil {
		return proto.FileFinalizeResult{}, err
	}

	chunks := buildChunks(totalSize, resolved.ChunkSize)
	transferID := transferIDFor(target, totalSize, wholeSum)

	conn, err := a.openTransferConn(server, resolved.Parallel)
	if err != nil {
		return proto.FileFinalizeResult{}, err
	}
	defer conn.closeFn()

	ow, err := decodeResult[proto.FileOpenWriteResult](conn.callRetry(0, proto.ActionFileOpenWrite, proto.FileOpenWritePayload{
		Path:       target,
		TotalSize:  totalSize,
		Mode:       uint32(info.Mode().Perm()),
		TransferID: transferID,
	}))
	if err != nil {
		return proto.FileFinalizeResult{}, fmt.Errorf("open remote file: %w", err)
	}

	done := resumeUploadChunks(conn, target, transferID, chunks, lf, ow.ResumeOffset)

	var (
		mu        sync.Mutex
		bytesDone int64
		firstErr  error
		active    int
	)
	for i := range chunks {
		if done[i] {
			bytesDone += chunks[i].length
		}
	}
	start := time.Now()
	emit := func(final bool, e error) {
		if progress == nil {
			return
		}
		mu.Lock()
		bd, act := bytesDone, active
		mu.Unlock()
		var rate float64
		if elapsed := time.Since(start).Seconds(); elapsed > 0 {
			rate = float64(bd) / elapsed
		}
		upd := ProgressUpdate{BytesDone: bd, TotalBytes: totalSize, RatePerSec: rate, ActiveStreams: act, Done: final}
		if e != nil {
			upd.Err = e.Error()
		}
		progress(upd)
	}
	emit(false, nil)

	jobs := make(chan int, len(chunks))
	for i := range chunks {
		if !done[i] {
			jobs <- i
		}
	}
	close(jobs)

	recordErr := func(e error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = e
		}
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for w := 0; w < len(conn.senders); w++ {
		wg.Add(1)
		go func(send senderFunc) {
			defer wg.Done()
			for idx := range jobs {
				mu.Lock()
				if firstErr != nil {
					mu.Unlock()
					return
				}
				active++
				mu.Unlock()

				c := chunks[idx]
				buf := make([]byte, c.length)
				if _, err := lf.ReadAt(buf, c.offset); err != nil && err != io.EOF {
					recordErr(fmt.Errorf("read local chunk: %w", err))
					return
				}
				sum := sha256Hex(buf)

				var werr error
				for attempt := 0; attempt <= sshMaxRetries; attempt++ {
					if attempt > 0 {
						time.Sleep(transferRetryDelay)
						if ns, rerr := conn.reopen(); rerr == nil {
							send = ns
						}
					}
					_, werr = decodeResult[proto.FileWriteResult](send(proto.Envelope{
						Action: proto.ActionFileWrite,
						Payload: proto.FileWritePayload{
							TransferID: transferID,
							Path:       target,
							Offset:     c.offset,
							Data:       buf,
							SHA256:     sum,
						},
					}))
					if werr == nil {
						break
					}
				}
				mu.Lock()
				active--
				if werr != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("write chunk at %d: %w", c.offset, werr)
					}
					mu.Unlock()
					return
				}
				bytesDone += c.length
				mu.Unlock()
				emit(false, nil)
			}
		}(conn.senders[w])
	}
	wg.Wait()

	if firstErr != nil {
		emit(false, firstErr)
		return proto.FileFinalizeResult{}, firstErr
	}

	result, err := decodeResult[proto.FileFinalizeResult](conn.callRetry(0, proto.ActionFileFinalize, proto.FileFinalizePayload{
		TransferID:  transferID,
		Path:        target,
		Mode:        uint32(info.Mode().Perm()),
		WholeSHA256: wholeSum,
		TotalSize:   totalSize,
	}))
	if err != nil {
		emit(false, err)
		return proto.FileFinalizeResult{}, fmt.Errorf("finalize remote file: %w", err)
	}
	emit(true, nil)

	_ = a.AuditLog.Append(logs.AuditEntry{
		Action:   "file.upload",
		Target:   serverName,
		Operator: a.operator(),
		Details:  fmt.Sprintf("%s -> %s (%d bytes, sha256=%s)", localPath, target, totalSize, wholeSum),
	})
	return result, nil
}

// resumeUploadChunks returns the set of chunk indices already present and
// verified in the remote temp file, so they can be skipped. It re-checksums the
// existing prefix via file.probe — never trusting on-disk size alone.
func resumeUploadChunks(conn *transferConn, target, transferID string, chunks []chunkSpec, lf *os.File, resumeOffset int64) map[int]bool {
	done := make(map[int]bool)
	if resumeOffset <= 0 {
		return done
	}
	var ranges []proto.FileRange
	expected := make(map[int64]string)
	candidate := make([]int, 0)
	for i, c := range chunks {
		if c.offset+c.length > resumeOffset {
			continue
		}
		buf := make([]byte, c.length)
		if _, err := lf.ReadAt(buf, c.offset); err != nil && err != io.EOF {
			return done
		}
		expected[c.offset] = sha256Hex(buf)
		ranges = append(ranges, proto.FileRange{Offset: c.offset, Length: c.length})
		candidate = append(candidate, i)
	}
	if len(ranges) == 0 {
		return done
	}
	res, err := decodeResult[proto.FileProbeResult](conn.callRetry(0, proto.ActionFileProbe, proto.FileProbePayload{
		Path:       target,
		TransferID: transferID,
		Ranges:     ranges,
	}))
	if err != nil {
		return done // probe failed — safest to re-send everything
	}
	got := make(map[int64]string, len(res.RangeChecksums))
	for _, rc := range res.RangeChecksums {
		got[rc.Offset] = rc.SHA256
	}
	for _, i := range candidate {
		c := chunks[i]
		if remote, ok := got[c.offset]; ok && remote == expected[c.offset] {
			done[i] = true
		}
	}
	return done
}

// DownloadFile downloads remotePath from serverName into localPath, chunked,
// parallel (direct mode), checksummed, and resumable.
func (a *App) DownloadFile(serverName, remotePath, localPath string, opts FileTransferOptions, progress ProgressFunc) (proto.FileStatResult, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return proto.FileStatResult{}, err
	}
	resolved := a.resolveTransferOptions(server, opts)
	if localPath == "" {
		localPath = filepath.Base(remotePath)
	} else if info, err := os.Stat(localPath); err == nil && info.IsDir() {
		localPath = filepath.Join(localPath, path.Base(remotePath))
	}

	conn, err := a.openTransferConn(server, resolved.Parallel)
	if err != nil {
		return proto.FileStatResult{}, err
	}
	defer conn.closeFn()

	stat, err := decodeResult[proto.FileStatResult](conn.callRetry(0, proto.ActionFileStat, proto.FileStatPayload{Path: remotePath}))
	if err != nil {
		return proto.FileStatResult{}, fmt.Errorf("stat remote file: %w", err)
	}
	if stat.Entry.IsDir {
		return proto.FileStatResult{}, fmt.Errorf("%s is a directory; recursive download is not supported", remotePath)
	}
	totalSize := stat.Entry.Size

	if err := os.MkdirAll(filepath.Dir(localPath), 0o750); err != nil {
		return proto.FileStatResult{}, err
	}
	dest, err := os.OpenFile(localPath, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- operator-supplied local path
	if err != nil {
		return proto.FileStatResult{}, fmt.Errorf("open local destination: %w", err)
	}
	defer dest.Close()

	chunks := buildChunks(totalSize, resolved.ChunkSize)
	done := resumeDownloadChunks(conn, remotePath, chunks, dest)

	var (
		mu        sync.Mutex
		bytesDone int64
		firstErr  error
		active    int
	)
	for i := range chunks {
		if done[i] {
			bytesDone += chunks[i].length
		}
	}
	start := time.Now()
	emit := func(final bool, e error) {
		if progress == nil {
			return
		}
		mu.Lock()
		bd, act := bytesDone, active
		mu.Unlock()
		var rate float64
		if elapsed := time.Since(start).Seconds(); elapsed > 0 {
			rate = float64(bd) / elapsed
		}
		upd := ProgressUpdate{BytesDone: bd, TotalBytes: totalSize, RatePerSec: rate, ActiveStreams: act, Done: final}
		if e != nil {
			upd.Err = e.Error()
		}
		progress(upd)
	}
	emit(false, nil)

	jobs := make(chan int, len(chunks))
	for i := range chunks {
		if !done[i] {
			jobs <- i
		}
	}
	close(jobs)

	var wg sync.WaitGroup
	for w := 0; w < len(conn.senders); w++ {
		wg.Add(1)
		go func(send senderFunc) {
			defer wg.Done()
			for idx := range jobs {
				mu.Lock()
				if firstErr != nil {
					mu.Unlock()
					return
				}
				active++
				mu.Unlock()

				c := chunks[idx]
				var (
					res  proto.FileReadResult
					rerr error
				)
				for attempt := 0; attempt <= sshMaxRetries; attempt++ {
					if attempt > 0 {
						time.Sleep(transferRetryDelay)
						if ns, e := conn.reopen(); e == nil {
							send = ns
						}
					}
					res, rerr = decodeResult[proto.FileReadResult](send(proto.Envelope{
						Action:  proto.ActionFileRead,
						Payload: proto.FileReadPayload{Path: remotePath, Offset: c.offset, Length: c.length},
					}))
					if rerr == nil && sha256Hex(res.Data) != res.SHA256 {
						rerr = fmt.Errorf("chunk checksum mismatch at %d", c.offset)
					}
					if rerr == nil {
						break
					}
				}
				if rerr == nil {
					if _, err := dest.WriteAt(res.Data, c.offset); err != nil {
						rerr = fmt.Errorf("write local chunk: %w", err)
					}
				}
				mu.Lock()
				active--
				if rerr != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("read chunk at %d: %w", c.offset, rerr)
					}
					mu.Unlock()
					return
				}
				bytesDone += c.length
				mu.Unlock()
				emit(false, nil)
			}
		}(conn.senders[w])
	}
	wg.Wait()

	if firstErr != nil {
		emit(false, firstErr)
		return proto.FileStatResult{}, firstErr
	}
	// Trim any stale bytes from a previously larger local file.
	if err := dest.Truncate(totalSize); err != nil {
		return proto.FileStatResult{}, err
	}
	if err := dest.Sync(); err != nil {
		return proto.FileStatResult{}, err
	}
	emit(true, nil)

	_ = a.AuditLog.Append(logs.AuditEntry{
		Action:   "file.download",
		Target:   serverName,
		Operator: a.operator(),
		Details:  fmt.Sprintf("%s:%s -> %s (%d bytes)", serverName, remotePath, localPath, totalSize),
	})
	return stat, nil
}

// resumeDownloadChunks verifies which local chunks already match the remote
// source (via a remote probe of the same ranges) so they can be skipped.
func resumeDownloadChunks(conn *transferConn, remotePath string, chunks []chunkSpec, dest *os.File) map[int]bool {
	done := make(map[int]bool)
	info, err := dest.Stat()
	if err != nil || info.Size() <= 0 {
		return done
	}
	localSize := info.Size()
	var ranges []proto.FileRange
	expected := make(map[int64]string)
	candidate := make([]int, 0)
	for i, c := range chunks {
		if c.offset+c.length > localSize {
			continue
		}
		buf := make([]byte, c.length)
		if _, err := dest.ReadAt(buf, c.offset); err != nil && err != io.EOF {
			return done
		}
		expected[c.offset] = sha256Hex(buf)
		ranges = append(ranges, proto.FileRange{Offset: c.offset, Length: c.length})
		candidate = append(candidate, i)
	}
	if len(ranges) == 0 {
		return done
	}
	res, err := decodeResult[proto.FileProbeResult](conn.callRetry(0, proto.ActionFileProbe, proto.FileProbePayload{
		Path:   remotePath,
		Ranges: ranges,
	}))
	if err != nil {
		return done
	}
	got := make(map[int64]string, len(res.RangeChecksums))
	for _, rc := range res.RangeChecksums {
		got[rc.Offset] = rc.SHA256
	}
	for _, i := range candidate {
		c := chunks[i]
		if remote, ok := got[c.offset]; ok && remote == expected[c.offset] {
			done[i] = true
		}
	}
	return done
}

// ---- small helpers ----

func resolveUploadRemotePath(remoteDir, remotePath, localPath string) (string, error) {
	base := filepath.Base(localPath)
	switch {
	case remotePath == "":
		if remoteDir == "" {
			return "", fmt.Errorf("remote path required: no default remote dir is configured (set one with 'fleet file defaults set' or pass an explicit remote path)")
		}
		return path.Join(remoteDir, base), nil
	case strings.HasSuffix(remotePath, "/"):
		return path.Join(remotePath, base), nil
	default:
		return remotePath, nil
	}
}

func transferIDFor(remotePath string, size int64, wholeSum string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s|%d|%s", remotePath, size, wholeSum))
	return hex.EncodeToString(sum[:16])
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func streamSHA256(r io.ReadSeeker) (string, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstPositiveInt(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

func firstPositiveInt64(values ...int64) int64 {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}
