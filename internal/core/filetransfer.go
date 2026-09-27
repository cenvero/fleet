// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
)

const (
	// sshChannelWindowBytes mirrors the flow-control window golang.org/x/crypto
	// gives every SSH channel (64 packets x 32 KiB). It is not configurable, and
	// it is the ceiling on how many bytes one channel can have in flight before
	// it must stop and wait for the peer to acknowledge. Transfer defaults are
	// chosen against it.
	sshChannelWindowBytes = 2 * 1024 * 1024 // 2 MiB

	// transferChunkHeadroom is the part of the channel window a chunk leaves
	// free for its envelope. A chunk must fit in the window together with its
	// JSON header and frame lengths; if it overshoots by even a few bytes the
	// sender stalls until the receiver returns window credit, which
	// golang.org/x/crypto only does in batches of at least 96 KiB — a whole
	// extra round trip per chunk (measured at 100 ms RTT: 227 ms per 2 MiB chunk
	// versus 116 ms per 1920 KiB chunk).
	transferChunkHeadroom = 128 * 1024

	// DefaultParallelStreams is the number of concurrent fleet-rpc channels a
	// transfer uses when nothing else is configured.
	//
	// This is the main lever on a long link. Each channel has its own 2 MiB
	// window, so bytes in flight is streams x window and throughput on a
	// latency-bound path is roughly (streams x 2 MiB) / round-trip time. At the
	// previous default of 4 that capped a 100 ms path near 80 MB/s no matter how
	// much bandwidth was available; 8 channels double the ceiling for the same
	// total buffer memory, because the chunk size below halved.
	DefaultParallelStreams = 8

	// DefaultChunkSizeBytes is the raw chunk size for one file.read/file.write:
	// the channel window minus transferChunkHeadroom.
	//
	// A chunk larger than the window cannot be written in one go: the sender
	// fills the window, stalls, and waits for the peer to drain it, so an
	// oversized chunk buys no extra bytes in flight and only adds latency and
	// per-worker memory. Chunks travel as raw binary frames, so this is also the
	// true wire size.
	DefaultChunkSizeBytes = sshChannelWindowBytes - transferChunkHeadroom

	// maxEffectiveChunkBytes caps configured chunk sizes at transfer time for
	// the same reason: anything larger only stalls on the window. Larger
	// configured values (including the previous 2 MiB default, and the 8 MiB
	// the protocol allows) are still accepted and simply clamped.
	maxEffectiveChunkBytes = DefaultChunkSizeBytes

	// maxTransferChunks caps the number of chunks a single transfer may plan.
	// The chunk plan is sized from a size the *remote agent reports* (download)
	// or the local file (upload); without a ceiling a malicious/buggy agent that
	// reports an absurd size would make buildChunks allocate an unbounded slice
	// and OOM the controller. At the default chunk size this still permits a
	// ~480 GiB transfer, which is far beyond any realistic single-file move.
	maxTransferChunks = 1 << 18 // 262144
	// maxTransferFileBytes is a hard ceiling on a single transfer's size,
	// independent of chunk size, so a tiny chunk size can't be combined with a
	// huge reported size to slip past maxTransferChunks. 8 TiB.
	maxTransferFileBytes = int64(8) * 1024 * 1024 * 1024 * 1024
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

	// localRoot/localRel are set only by recursive download and sync-pull
	// primitives. They confine remote-derived relative paths beneath a single
	// descriptor-backed local root.
	localRoot string
	localRel  string
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
// to proto.MaxRawChunkBytes; the engine additionally clamps it to the channel
// window when a transfer runs (see resolveTransferOptions).
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

// clampChunkSize bounds a configured chunk size to what fits in one SSH channel
// window with its envelope.
func clampChunkSize(size int64) int64 {
	if size <= 0 {
		return DefaultChunkSizeBytes
	}
	return min(size, maxEffectiveChunkBytes)
}

func (a *App) resolveTransferOptions(server ServerRecord, opts FileTransferOptions) FileTransferOptions {
	d := a.effectiveFileTransferDefaults(server)
	resolved := FileTransferOptions{
		Parallel:  firstPositiveInt(opts.Parallel, d.ParallelStreams),
		ChunkSize: clampChunkSize(firstPositiveInt64(opts.ChunkSize, d.ChunkSizeBytes)),
		RemoteDir: firstNonEmptyString(opts.RemoteDir, d.RemoteDir),
	}
	// A recursive directory transfer runs dirTransferConcurrency files at once
	// and each file may use `Parallel` channels, so the peak channel count is
	// the product. `parallel_streams` is operator-configurable with no upper
	// bound, so keep one file's share bounded; transferChannelBudget bounds the
	// total on the shared connection.
	if maxParallel := transport.MaxChannelsPerConn / dirTransferConcurrency; resolved.Parallel > maxParallel {
		resolved.Parallel = maxParallel
	}
	if server.Mode == transport.ModeReverse && !serverSupportsReverseMultiplex(server) {
		// An agent that cannot accept controller-opened channels leaves the hub
		// with one mutex-serialised channel, so parallel streams are impossible.
		// Transfers stay chunked, resumable and checksummed — just single-stream.
		resolved.Parallel = 1
	}
	return resolved
}

// serverSupportsReverseMultiplex reports whether a reverse agent accepts extra
// fleet-rpc channels opened by the controller, which is what allows a reverse
// transfer to run more than one stream.
func serverSupportsReverseMultiplex(server ServerRecord) bool {
	return slices.Contains(server.Capabilities, proto.CapabilityReverseMultiplex)
}

// ---- simple lightweight RPC browsing (one-shot, no channel pool) ----

// ListRemoteDir lists a directory on a managed server (hidden entries excluded).
func (a *App) ListRemoteDir(serverName, remotePath string) (proto.FileListResult, error) {
	return a.ListRemoteDirHidden(serverName, remotePath, false)
}

// RemoteMkdir creates a directory on a managed server.
func (a *App) RemoteMkdir(serverName, remotePath string) error {
	if err := a.validateRemoteTargetPath(serverName, remotePath); err != nil {
		return err
	}
	return a.simpleFileOp(serverName, proto.ActionFileMkdir, proto.FileMkdirPayload{Path: remotePath}, "file.mkdir", remotePath)
}

// RemoteDelete removes a file or directory on a managed server.
func (a *App) RemoteDelete(serverName, remotePath string, recursive bool) error {
	server, err := a.GetServer(serverName)
	if err != nil {
		return err
	}
	if err := rejectDotComponents(TargetPathStyleForServer(server), remotePath); err != nil {
		return err
	}
	target := remotePath
	if recursive {
		target += " recursive=true"
	}
	return a.simpleFileOp(serverName, proto.ActionFileDelete, proto.FileDeletePayload{Path: remotePath, Recursive: recursive}, "file.delete", target)
}

// RemoteRename renames/moves a path on a managed server.
func (a *App) RemoteRename(serverName, from, to string) error {
	server, err := a.GetServer(serverName)
	if err != nil {
		return err
	}
	if err := rejectDotComponents(TargetPathStyleForServer(server), from); err != nil {
		return err
	}
	if err := a.validateRemoteTargetPath(serverName, to); err != nil {
		return err
	}
	return a.simpleFileOp(serverName, proto.ActionFileRename, proto.FileRenamePayload{From: from, To: to}, "file.rename", from+" -> "+to)
}

func (a *App) validateRemoteTargetPath(serverName, target string) error {
	server, err := a.GetServer(serverName)
	if err != nil {
		return err
	}
	style := TargetPathStyleForServer(server)
	if err := rejectDotComponents(style, target); err != nil {
		return err
	}
	return ValidateTargetPath(style, target)
}

func (a *App) simpleFileOp(serverName, action string, payload any, auditAction, target string) error {
	server, err := a.GetServer(serverName)
	if err != nil {
		return err
	}
	resp, err := a.callRPC(server, proto.Envelope{Action: action, Payload: payload})
	if err == nil && resp.Error != nil {
		err = remoteErr(resp.Error)
	}
	if err != nil {
		// Attempts that fail (refused by the agent's file root, missing path,
		// unreachable) are audited too, not only successes.
		_ = a.AuditLog.Append(logs.AuditEntry{
			Action:   auditAction + ".failed",
			Target:   serverName,
			Operator: a.operator(),
			Details:  target + " error=" + err.Error(),
		})
		return err
	}
	_ = a.AuditLog.Append(logs.AuditEntry{
		Action:   auditAction,
		Target:   serverName,
		Operator: a.operator(),
		Details:  target,
	})
	return nil
}

// ---- errors ----

// remoteError is an error the agent reported for an RPC. Its text is the
// historical "code: message" form; the code stays inspectable so callers can
// react to specific conditions (an unsupported action on an older agent, a
// destination that turned out to be a directory).
type remoteError struct {
	Code    string
	Message string
}

func (e *remoteError) Error() string { return e.Code + ": " + e.Message }

func remoteErr(e *proto.Error) error {
	if e == nil {
		return nil
	}
	return &remoteError{Code: e.Code, Message: e.Message}
}

// remoteErrorCode returns the agent's error code for err, or "".
func remoteErrorCode(err error) string {
	var re *remoteError
	if errors.As(err, &re) {
		return re.Code
	}
	return ""
}

const (
	errCodeUnsupportedAction = "unsupported_action"
	errCodeTargetIsDirectory = "target_is_directory"
	errCodeTransferBusy      = "transfer_busy"
	errCodeRenameFailed      = "rename_failed"
)

// transferBusyWait bounds how long an upload waits for a still-running
// finalize of the same transfer (see errCodeTransferBusy).
const (
	transferBusyWait = 2 * time.Minute
	transferBusyPoll = 250 * time.Millisecond
)

// ---- chunked / parallel / resumable transfers ----

type chunkSpec struct {
	offset int64
	length int64
}

// validateTransferSize bounds a transfer planned from total bytes at the given
// chunk size, rejecting an absurd (e.g. agent-reported) size before buildChunks
// is allowed to allocate a chunk plan from it. It guards against OOM from a
// malicious or buggy peer that reports a gigantic file size.
func validateTransferSize(total, chunkSize int64) error {
	if total < 0 {
		return fmt.Errorf("invalid negative file size %d", total)
	}
	if total > maxTransferFileBytes {
		return fmt.Errorf("file size %d exceeds the maximum supported single-file transfer size (%d bytes)", total, maxTransferFileBytes)
	}
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSizeBytes
	}
	// Number of chunks == ceil(total/chunkSize); compute without overflow.
	if total > 0 && (total-1)/chunkSize+1 > maxTransferChunks {
		return fmt.Errorf("file would require more than %d chunks at chunk size %d; refusing to plan an unbounded transfer", maxTransferChunks, chunkSize)
	}
	return nil
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

// chunkListDigest is proto.ChunkListDigest over a chunk plan and its digests.
func chunkListDigest(total int64, chunks []chunkSpec, digests []string) (string, error) {
	list := make([]proto.FileRangeChecksum, len(chunks))
	for i, c := range chunks {
		list[i] = proto.FileRangeChecksum{Offset: c.offset, Length: c.length, SHA256: digests[i]}
	}
	return proto.ChunkListDigest(total, list)
}

// serverSupportsBinaryFrames reports whether an agent has advertised that it can
// carry file chunks as raw binary frames rather than base64 inside JSON. The
// capability list is recorded from the agent's hello on every connect, so a
// fleet with mixed agent versions transparently uses the best encoding each one
// understands.
func serverSupportsBinaryFrames(server ServerRecord) bool {
	return slices.Contains(server.Capabilities, proto.CapabilityBinaryFrames)
}

// transferConn is the RPC surface for one transfer: a set of worker slots, each
// able to carry one RPC at a time. In direct mode every slot is a channel leased
// from the pooled SSH connection (see sessionpool_transfer.go); in reverse mode
// every slot calls through the reverse hub, which leases its own channel per
// call (or, for agents that cannot multiplex, a single serialised slot).
type transferConn struct {
	send    func(slot int, env proto.Envelope) (proto.Envelope, error)
	slots   func() int
	grow    func(n int)
	closeFn func()
	caps    []string
	// binaryFrames records whether the peer can carry chunks as raw binary
	// frames. Direct mode takes it from the pooled connection's live hello, so
	// the very first transfer to a freshly added server gets the fast encoding.
	binaryFrames bool
}

func (c *transferConn) supports(capability string) bool {
	return slices.Contains(c.caps, capability)
}

// openTransferConn prepares want worker slots to server. Callers size want to
// the work (never more slots than chunks), and may grow it later.
func (a *App) openTransferConn(server ServerRecord, want int) (*transferConn, error) {
	want = max(want, 1)
	if server.Mode == transport.ModeReverse {
		// The reverse hub has no per-transfer hello, so rely on the
		// capabilities recorded when the agent connected. Every optional RPC
		// still falls back if an agent turns out not to implement it.
		conn := &transferConn{
			caps:         server.Capabilities,
			binaryFrames: serverSupportsBinaryFrames(server),
			closeFn:      func() {},
		}
		if serverSupportsReverseMultiplex(server) {
			// Each worker calls independently; the hub leases a distinct
			// channel per concurrent call, so no serialisation is needed.
			var mu sync.Mutex
			n := want
			conn.send = func(_ int, env proto.Envelope) (proto.Envelope, error) { return a.callRPC(server, env) }
			conn.slots = func() int { mu.Lock(); defer mu.Unlock(); return n }
			conn.grow = func(m int) { mu.Lock(); n = max(n, m); mu.Unlock() }
			return conn, nil
		}
		// Older agent: one shared channel, so serialise explicitly.
		var mu sync.Mutex
		conn.send = func(_ int, env proto.Envelope) (proto.Envelope, error) {
			mu.Lock()
			defer mu.Unlock()
			return a.callRPC(server, env)
		}
		conn.slots = func() int { return 1 }
		conn.grow = func(int) {}
		return conn, nil
	}
	tc, err := a.leaseTransferChannels(server, want)
	if err != nil {
		return nil, err
	}
	return &transferConn{
		send:         tc.call,
		slots:        tc.size,
		grow:         tc.grow,
		closeFn:      tc.close,
		caps:         tc.caps,
		binaryFrames: tc.supports(proto.CapabilityBinaryFrames),
	}, nil
}

// callRetry runs a request on slot with bounded retries. A broken channel is
// replaced by the slot between attempts. Only for calls that bracket the worker
// phase (open_write, probe, finalize, stat) or run on a slot no worker uses.
func (c *transferConn) callRetry(slot int, env proto.Envelope) (proto.Envelope, error) {
	var resp proto.Envelope
	var err error
	for attempt := 0; attempt <= sshMaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(transferRetryDelay)
		}
		resp, err = c.send(slot, env)
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
		return zero, remoteErr(resp.Error)
	}
	out, derr := proto.DecodePayload[R](resp.Payload)
	if derr != nil {
		return zero, derr
	}
	// A chunk returned as a binary frame travels beside the JSON, not in it.
	proto.AttachBinary(&out, resp)
	return out, nil
}

// ---- buffers ----

// chunkBufPool recycles default-sized chunk buffers across workers and
// transfers. Allocating one per worker per transfer cost 16 MiB of garbage for
// every upload — even a 4 KiB one.
var chunkBufPool = sync.Pool{New: func() any {
	b := make([]byte, DefaultChunkSizeBytes)
	return &b
}}

// smallBufThreshold is below which a buffer is simply allocated to size.
const smallBufThreshold = 64 * 1024

// getChunkBuf returns a buffer of length n and a function that recycles it.
func getChunkBuf(n int64) ([]byte, func()) {
	if n > smallBufThreshold && n <= DefaultChunkSizeBytes {
		bufp := chunkBufPool.Get().(*[]byte)
		return (*bufp)[:n], func() { chunkBufPool.Put(bufp) }
	}
	return make([]byte, n), func() {}
}

// hashParallelism bounds concurrent local hashing for resume checks.
func hashParallelism(n int) int {
	return max(1, min(n, runtime.GOMAXPROCS(0), 8))
}

// hashFileChunks computes the SHA-256 of each chunk of f in parallel. A slot is
// left empty when its chunk cannot be read.
func hashFileChunks(f io.ReaderAt, chunks []chunkSpec, idx []int) map[int]string {
	out := make(map[int]string, len(idx))
	var mu sync.Mutex
	next := make(chan int)
	var wg sync.WaitGroup
	for range hashParallelism(len(idx)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 1<<20)
			for i := range next {
				c := chunks[i]
				h := sha256.New()
				n, err := io.CopyBuffer(h, io.NewSectionReader(f, c.offset, c.length), buf)
				if err != nil || n != c.length {
					continue
				}
				sum := hex.EncodeToString(h.Sum(nil))
				mu.Lock()
				out[i] = sum
				mu.Unlock()
			}
		}()
	}
	for _, i := range idx {
		next <- i
	}
	close(next)
	wg.Wait()
	return out
}

// ---- progress ----

type progressTracker struct {
	mu        sync.Mutex
	fn        ProgressFunc
	total     int64
	bytesDone int64
	active    int
	start     time.Time
}

func newProgressTracker(fn ProgressFunc, total int64) *progressTracker {
	return &progressTracker{fn: fn, total: total, start: time.Now()}
}

func (p *progressTracker) add(n int64) {
	p.mu.Lock()
	p.bytesDone += n
	p.mu.Unlock()
}

func (p *progressTracker) setActive(delta int) {
	p.mu.Lock()
	p.active += delta
	p.mu.Unlock()
}

func (p *progressTracker) emit(final bool, e error) {
	if p == nil || p.fn == nil {
		return
	}
	p.mu.Lock()
	bd, act := p.bytesDone, p.active
	p.mu.Unlock()
	var rate float64
	if elapsed := time.Since(p.start).Seconds(); elapsed > 0 {
		rate = float64(bd) / elapsed
	}
	upd := ProgressUpdate{BytesDone: bd, TotalBytes: p.total, RatePerSec: rate, ActiveStreams: act, Done: final}
	if e != nil {
		upd.Err = e.Error()
	}
	p.fn(upd)
}

// firstError records the first error of a worker group.
type firstError struct {
	mu  sync.Mutex
	err error
}

func (f *firstError) set(err error) {
	f.mu.Lock()
	if f.err == nil {
		f.err = err
	}
	f.mu.Unlock()
}

func (f *firstError) get() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// ---- upload ----

// UploadFile uploads localPath to remotePath on serverName, chunked, parallel,
// checksummed, and resumable. remotePath may be empty (use the default remote
// dir + local base name) or end with "/" (treat as a directory). An existing
// remote directory is treated the same way: the file lands inside it, as with
// cp or scp.
func (a *App) UploadFile(serverName, localPath, remotePath string, opts FileTransferOptions, progress ProgressFunc) (proto.FileFinalizeResult, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return proto.FileFinalizeResult{}, err
	}
	resolved := a.resolveTransferOptions(server, opts)
	style := TargetPathStyleForServer(server)
	if err := rejectDotComponents(style, remotePath); err != nil {
		return proto.FileFinalizeResult{}, err
	}
	target, err := resolveUploadRemotePath(style, resolved.RemoteDir, remotePath, localPath)
	if err != nil {
		return proto.FileFinalizeResult{}, err
	}
	if err := ValidateTargetPath(style, target); err != nil {
		return proto.FileFinalizeResult{}, err
	}

	// codeql[go/path-injection] operator-chosen controller path (a CLI/TUI argument, or a web UI path already vetted by cleanLocal and the protected-path guard), not attacker input
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
	if err := validateTransferSize(totalSize, resolved.ChunkSize); err != nil {
		return proto.FileFinalizeResult{}, fmt.Errorf("refusing to upload %s: %w", localPath, err)
	}
	chunks := buildChunks(totalSize, resolved.ChunkSize)

	// Never open more streams than there are chunks to move.
	conn, err := a.openTransferConn(server, min(resolved.Parallel, max(len(chunks), 1)))
	if err != nil {
		return proto.FileFinalizeResult{}, err
	}
	defer conn.closeFn()

	up := &upload{
		app: a, conn: conn, server: server, lf: lf, info: info, localPath: localPath,
		chunks: chunks, progress: newProgressTracker(progress, totalSize),
	}
	if !conn.supports(proto.CapabilityFileChunkDigests) {
		// Older agents only notice a directory destination at the final rename,
		// after the whole file was sent. Look first (one round trip, only for
		// them) so the file goes inside the directory like cp/scp would.
		if st, err := decodeResult[proto.FileStatResult](conn.callRetry(0, proto.Envelope{Action: proto.ActionFileStat, Payload: proto.FileStatPayload{Path: target}})); err == nil && st.Entry.IsDir && !st.Entry.IsSymlink {
			target = style.Join(target, filepath.Base(localPath))
		}
	}
	result, err := up.run(target)
	if remoteErrorCode(err) == errCodeTargetIsDirectory {
		// The destination is an existing directory: the agent refused before
		// creating anything, so retry inside it.
		target = style.Join(target, filepath.Base(localPath))
		if verr := ValidateTargetPath(style, target); verr != nil {
			return proto.FileFinalizeResult{}, verr
		}
		result, err = up.run(target)
	}
	if remoteErrorCode(err) == errCodeRenameFailed {
		// Older agents let a retried upload reopen the temp file of a
		// finalize that was still running (its reply lost with a killed
		// controller); that finalize then renamed the temp away underneath
		// us. Starting over once is cheap next to reporting a spurious
		// failure, and a persistent rename error simply recurs.
		result, err = up.run(target)
	}
	if err != nil {
		up.progress.emit(false, err)
		return proto.FileFinalizeResult{}, err
	}
	up.progress.emit(true, nil)
	_ = a.AuditLog.Append(logs.AuditEntry{
		Action:   "file.upload",
		Target:   serverName,
		Operator: a.operator(),
		Details:  fmt.Sprintf("%s -> %s (%d bytes, sha256=%s)", localPath, target, totalSize, result.SHA256),
	})
	return result, nil
}

type upload struct {
	app       *App
	conn      *transferConn
	server    ServerRecord
	lf        *os.File
	info      os.FileInfo
	localPath string
	chunks    []chunkSpec
	progress  *progressTracker
}

func (u *upload) run(target string) (proto.FileFinalizeResult, error) {
	if len(u.chunks) <= 1 && u.conn.supports(proto.CapabilityFilePut) {
		res, err := u.put(target)
		if remoteErrorCode(err) != errCodeUnsupportedAction {
			return res, err
		}
		// Capabilities recorded for a reverse agent can be stale; fall back.
	}
	return u.chunked(target)
}

// put uploads a file that fits in one chunk with a single round trip.
func (u *upload) put(target string) (proto.FileFinalizeResult, error) {
	size := u.info.Size()
	buf, release := getChunkBuf(size)
	defer release()
	if _, err := u.lf.ReadAt(buf, 0); err != nil && err != io.EOF {
		return proto.FileFinalizeResult{}, fmt.Errorf("read local file: %w", err)
	}
	sum := sha256Hex(buf)
	env := proto.Envelope{Action: proto.ActionFilePut, Payload: &proto.FilePutPayload{
		Path: target, Mode: uint32(u.info.Mode().Perm()), Data: buf, SHA256: sum,
	}}
	if u.conn.binaryFrames {
		env = proto.DetachBinary(env)
	}
	u.progress.setActive(1)
	res, err := decodeResult[proto.FileFinalizeResult](u.conn.callRetry(0, env))
	u.progress.setActive(-1)
	if err != nil {
		return proto.FileFinalizeResult{}, err
	}
	if res.SHA256 != sum || res.Size != size {
		return proto.FileFinalizeResult{}, fmt.Errorf("remote file put verification failed: sent %d bytes sha256=%s, agent stored %d bytes sha256=%s", size, sum, res.Size, res.SHA256)
	}
	u.progress.add(size)
	return res, nil
}

// chunked runs open_write / parallel writes / finalize.
func (u *upload) chunked(target string) (proto.FileFinalizeResult, error) {
	totalSize := u.info.Size()
	chunks := u.chunks
	// The transfer id ties the remote temp file to this transfer across
	// channels and across a resumed run. It is derived from the file's identity
	// rather than its content so it is available immediately: a modified file
	// gets a different mtime and therefore a different temp, and finalize still
	// verifies the assembled bytes, so a stale temp can never be silently
	// accepted.
	transferID := transferIDFor(target, totalSize, fileIdentity(u.info))

	// Hash the exact already-open source descriptor alongside the transfer: the
	// real SHA-256 of the contents is reported to the operator and verified by
	// agents without chunk digests. A single-chunk file needs no second pass.
	var hashJob *wholeFileHashJob
	if len(chunks) > 1 {
		hashJob = startWholeFileHash(u.lf, totalSize)
		defer hashJob.CancelAndWait()
	}

	openReq := proto.Envelope{Action: proto.ActionFileOpenWrite, Payload: proto.FileOpenWritePayload{
		Path:       target,
		TotalSize:  totalSize,
		Mode:       uint32(u.info.Mode().Perm()),
		TransferID: transferID,
	}}
	ow, err := decodeResult[proto.FileOpenWriteResult](u.conn.callRetry(0, openReq))
	// A previous attempt's finalize may still be running on the agent (its
	// reply was lost with the connection); wait for it rather than fail.
	for deadline := time.Now().Add(transferBusyWait); remoteErrorCode(err) == errCodeTransferBusy && time.Now().Before(deadline); {
		time.Sleep(transferBusyPoll)
		ow, err = decodeResult[proto.FileOpenWriteResult](u.conn.callRetry(0, openReq))
	}
	if err != nil {
		if remoteErrorCode(err) == errCodeTargetIsDirectory {
			return proto.FileFinalizeResult{}, err
		}
		return proto.FileFinalizeResult{}, fmt.Errorf("open remote file: %w", err)
	}

	digests := make([]string, len(chunks))
	jobs := make(chan int, len(chunks))
	candidates := resumeCandidates(chunks, ow.ResumeOffset)
	isCandidate := make(map[int]bool, len(candidates))
	for _, i := range candidates {
		isCandidate[i] = true
	}
	for i := range chunks {
		if !isCandidate[i] {
			jobs <- i
		}
	}
	var errs firstError
	var verifyWG sync.WaitGroup
	if len(candidates) == 0 {
		close(jobs)
	} else {
		// Verify the already-sent prefix while new data flows: the agent's
		// recorded chunk checksums (or, failing that, a parallel probe) are
		// compared with local hashes computed in parallel. Only chunks that do
		// not match are queued for sending.
		verifyWG.Add(1)
		go func() {
			defer verifyWG.Done()
			defer close(jobs)
			for _, i := range u.verifyPrefix(target, transferID, candidates, ow.Chunks, digests) {
				jobs <- i
			}
		}()
	}

	workers := u.conn.slots()
	var wg sync.WaitGroup
	for slot := range workers {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			var buf []byte
			release := func() {}
			defer func() { release() }()
			for idx := range jobs {
				if errs.get() != nil {
					continue // drain so the verifier never blocks
				}
				c := chunks[idx]
				if int64(cap(buf)) < c.length {
					release()
					buf, release = getChunkBuf(c.length)
				}
				u.progress.setActive(1)
				err := u.sendChunk(slot, target, transferID, c, buf[:c.length], &digests[idx])
				u.progress.setActive(-1)
				if err != nil {
					errs.set(err)
					continue
				}
				u.progress.add(c.length)
				u.progress.emit(false, nil)
			}
		}(slot)
	}
	wg.Wait()
	verifyWG.Wait()
	if err := errs.get(); err != nil {
		return proto.FileFinalizeResult{}, err
	}

	wholeSum := sha256Hex(nil)
	switch {
	case hashJob != nil:
		// On any network-bound transfer this finished long ago; on a fast
		// local link this is the only place its cost can show up.
		if wholeSum, err = hashJob.Wait(); err != nil {
			return proto.FileFinalizeResult{}, err
		}
	case len(chunks) == 1:
		wholeSum = digests[0]
	}

	final := proto.FileFinalizePayload{
		TransferID:  transferID,
		Path:        target,
		Mode:        uint32(u.info.Mode().Perm()),
		WholeSHA256: wholeSum,
		TotalSize:   totalSize,
	}
	// When the source did not change underneath us, every chunk was read from
	// the same bytes the whole-file hash saw, so the agent can check its chunk
	// records instead of re-reading the file. Otherwise (a file still being
	// appended to, say) the agent re-hashes, exactly as before.
	if now, err := u.lf.Stat(); err == nil && now.Size() == u.info.Size() && now.ModTime().Equal(u.info.ModTime()) {
		if cd, err := chunkListDigest(totalSize, chunks, digests); err == nil {
			final.ChunkDigest = cd
		}
	}
	result, err := decodeResult[proto.FileFinalizeResult](u.conn.callRetry(0, proto.Envelope{Action: proto.ActionFileFinalize, Payload: final}))
	if err != nil {
		return proto.FileFinalizeResult{}, fmt.Errorf("finalize remote file: %w", err)
	}
	if result.ChunkDigest != "" && result.ChunkDigest != final.ChunkDigest {
		return proto.FileFinalizeResult{}, fmt.Errorf("finalize remote file: agent verified chunk digest %s, expected %s", result.ChunkDigest, final.ChunkDigest)
	}
	return result, nil
}

// sendChunk reads, hashes and writes one chunk, retrying on failure.
func (u *upload) sendChunk(slot int, target, transferID string, c chunkSpec, chunk []byte, digest *string) error {
	if _, err := u.lf.ReadAt(chunk, c.offset); err != nil && err != io.EOF {
		return fmt.Errorf("read local chunk: %w", err)
	}
	sum := sha256Hex(chunk)
	*digest = sum
	var werr error
	for attempt := 0; attempt <= sshMaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(transferRetryDelay)
		}
		req := proto.Envelope{
			Action: proto.ActionFileWrite,
			Payload: &proto.FileWritePayload{
				TransferID: transferID,
				Path:       target,
				Offset:     c.offset,
				Data:       chunk,
				SHA256:     sum,
			},
		}
		if u.conn.binaryFrames {
			// Ship the chunk verbatim after the envelope instead of base64
			// inside it: no +33% inflation, no base64 pass, and no
			// multi-megabyte intermediate on either side.
			req = proto.DetachBinary(req)
		}
		_, werr = decodeResult[proto.FileWriteResult](u.conn.send(slot, req))
		if werr == nil {
			return nil
		}
	}
	return fmt.Errorf("write chunk at %d: %w", c.offset, werr)
}

// resumeCandidates returns the chunks lying entirely within an existing remote
// (or local) partial file of the given size.
func resumeCandidates(chunks []chunkSpec, existing int64) []int {
	if existing <= 0 {
		return nil
	}
	var out []int
	for i, c := range chunks {
		if c.offset+c.length <= existing {
			out = append(out, i)
		}
	}
	return out
}

// verifyPrefix decides which already-present chunks of a resumed upload can be
// skipped. It never trusts the remote size alone: a chunk is skipped only when
// the agent vouches for a checksum over exactly that range (a record of a
// verified write, or a fresh hash from a probe) that equals the local chunk's
// hash. It fills digests for skipped chunks and returns the ones to send.
func (u *upload) verifyPrefix(target, transferID string, candidates []int, records []proto.FileRangeChecksum, digests []string) []int {
	local := hashFileChunks(u.lf, u.chunks, candidates)
	remote := make(map[int64]proto.FileRangeChecksum, len(records))
	for _, r := range records {
		remote[r.Offset] = r
	}
	var resend, probe []int
	var ranges []proto.FileRange
	for _, i := range candidates {
		c := u.chunks[i]
		sum, ok := local[i]
		if !ok {
			resend = append(resend, i)
			continue
		}
		if r, ok := remote[c.offset]; ok && r.Length == c.length {
			if r.SHA256 == sum {
				u.markSkipped(i, sum, digests)
			} else {
				resend = append(resend, i)
			}
			continue
		}
		probe = append(probe, i)
		ranges = append(ranges, proto.FileRange{Offset: c.offset, Length: c.length})
	}
	if len(probe) > 0 {
		// Probe on a channel of its own so it runs alongside the workers.
		res, err := decodeResult[proto.FileProbeResult](u.app.callRPC(u.server, proto.Envelope{
			Action:  proto.ActionFileProbe,
			Payload: proto.FileProbePayload{Path: target, TransferID: transferID, Ranges: ranges},
		}))
		got := make(map[int64]proto.FileRangeChecksum)
		if err == nil {
			for _, rc := range res.RangeChecksums {
				got[rc.Offset] = rc
			}
		}
		for _, i := range probe {
			c := u.chunks[i]
			if rc, ok := got[c.offset]; ok && rc.Length == c.length && rc.SHA256 == local[i] {
				u.markSkipped(i, local[i], digests)
			} else {
				resend = append(resend, i)
			}
		}
	}
	return resend
}

func (u *upload) markSkipped(i int, sum string, digests []string) {
	digests[i] = sum
	u.progress.add(u.chunks[i].length)
	u.progress.emit(false, nil)
}

// ---- download ----

// DownloadFile downloads remotePath from serverName into localPath, chunked,
// parallel, checksummed, and resumable.
func (a *App) DownloadFile(serverName, remotePath, localPath string, opts FileTransferOptions, progress ProgressFunc) (proto.FileStatResult, error) {
	stat, _, err := a.downloadFile(serverName, remotePath, localPath, opts, progress)
	return stat, err
}

// downloadFile is DownloadFile that also returns the SHA-256 of the installed
// file.
func (a *App) downloadFile(serverName, remotePath, localPath string, opts FileTransferOptions, progress ProgressFunc) (proto.FileStatResult, string, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return proto.FileStatResult{}, "", err
	}
	resolved := a.resolveTransferOptions(server, opts)
	style := TargetPathStyleForServer(server)
	// Security: localPath is the operator-chosen destination (CLI/TUI argument,
	// or a web UI path vetted by cleanLocalTree and the protected-path guard);
	// recursive callers instead confine localRel beneath localRoot.
	if opts.localRoot != "" {
		if !safeRel(filepath.FromSlash(opts.localRel)) {
			return proto.FileStatResult{}, "", fmt.Errorf("refusing unsafe local destination %q", opts.localRel)
		}
		localPath = filepath.Join(opts.localRoot, filepath.FromSlash(opts.localRel))
	} else if localPath == "" {
		localPath = style.Base(remotePath)
		// codeql[go/path-injection] operator-chosen controller path (a CLI/TUI argument, or a web UI path already vetted by cleanLocal and the protected-path guard), not attacker input
	} else if info, err := os.Lstat(localPath); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		localPath = filepath.Join(localPath, style.Base(remotePath))
	}
	if err := ValidateTargetPath(NativePathStyle(), localPath); err != nil {
		return proto.FileStatResult{}, "", err
	}

	conn, err := a.openTransferConn(server, 1)
	if err != nil {
		return proto.FileStatResult{}, "", err
	}
	defer conn.closeFn()

	stat, first, err := statAndFirstChunk(conn, remotePath, resolved.ChunkSize)
	if err != nil {
		return proto.FileStatResult{}, "", err
	}
	if err := requireRemoteRegular(stat.Entry, remotePath); err != nil {
		return proto.FileStatResult{}, "", err
	}
	totalSize := stat.Entry.Size
	// The size here is reported by the remote agent. Bound it before it sizes a
	// chunk plan, so a malicious/buggy agent can't trigger an unbounded
	// allocation (OOM) by claiming an absurd file size.
	if err := validateTransferSize(totalSize, resolved.ChunkSize); err != nil {
		return proto.FileStatResult{}, "", fmt.Errorf("refusing to download %s: %w", remotePath, err)
	}
	chunks := buildChunks(totalSize, resolved.ChunkSize)
	if first != nil && (len(chunks) == 0 || int64(len(first.Data)) != chunks[0].length || sha256Hex(first.Data) != first.SHA256) {
		first = nil // the file changed between stat and read, or the reply is off: fetch normally
	}

	// Assemble into a stable resumable sidecar, then atomically rename it over
	// the final directory entry. A planted final symlink is replaced, not opened;
	// confined callers additionally keep every intermediate lookup beneath their
	// descriptor-backed local root.
	downloadID := transferIDFor(serverName+":"+remotePath, totalSize, fmt.Sprintf("%d", stat.Entry.ModTime.UnixNano()))
	var atomicDest *AtomicLocalFile
	if opts.localRoot != "" {
		atomicDest, err = OpenAtomicLocalFileUnder(opts.localRoot, opts.localRel, downloadID, 0o600)
	} else {
		atomicDest, err = OpenAtomicLocalFile(localPath, downloadID, 0o600)
	}
	if err != nil {
		return proto.FileStatResult{}, "", fmt.Errorf("open local destination: %w", err)
	}
	defer atomicDest.CloseKeep()
	dest := atomicDest.File()

	d := &download{conn: conn, remotePath: remotePath, dest: dest, chunks: chunks,
		digests: make([]string, len(chunks)), progress: newProgressTracker(progress, totalSize)}
	d.progress.emit(false, nil)
	var hasher *orderedFileHasher
	if len(chunks) > 1 {
		// The whole-file digest is computed from the assembled file as its
		// contiguous prefix completes, overlapping the transfer instead of
		// re-reading everything at the end.
		hasher = newOrderedFileHasher(dest, chunks)
		defer hasher.cancel()
	}
	d.hasher = hasher

	jobs := make(chan int, len(chunks))
	skip := make(map[int]bool)
	if first != nil {
		if _, err := dest.WriteAt(first.Data, 0); err != nil {
			return proto.FileStatResult{}, "", fmt.Errorf("write local chunk: %w", err)
		}
		d.chunkDone(0, first.SHA256)
		skip[0] = true
	}
	var candidates []int
	if info, err := dest.Stat(); err == nil {
		for _, i := range resumeCandidates(chunks, info.Size()) {
			if !skip[i] {
				candidates = append(candidates, i)
				skip[i] = true
			}
		}
	}
	for i := range chunks {
		if !skip[i] {
			jobs <- i
		}
	}
	if pending := len(chunks) - len(skip) + len(candidates); pending > 1 {
		conn.grow(min(resolved.Parallel, pending))
	}
	var verifyWG sync.WaitGroup
	if len(candidates) == 0 {
		close(jobs)
	} else {
		verifyWG.Add(1)
		go func() {
			defer verifyWG.Done()
			defer close(jobs)
			for _, i := range d.verifyPrefix(a, server, candidates) {
				jobs <- i
			}
		}()
	}

	var errs firstError
	var wg sync.WaitGroup
	for slot := range conn.slots() {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			for idx := range jobs {
				if errs.get() != nil {
					continue
				}
				d.progress.setActive(1)
				err := d.fetchChunk(slot, idx, conn.binaryFrames)
				d.progress.setActive(-1)
				if err != nil {
					errs.set(err)
					continue
				}
				d.progress.emit(false, nil)
			}
		}(slot)
	}
	wg.Wait()
	verifyWG.Wait()
	if err := errs.get(); err != nil {
		d.progress.emit(false, err)
		return proto.FileStatResult{}, "", err
	}
	// Trim any stale bytes from a previously larger local file.
	if err := dest.Truncate(totalSize); err != nil {
		return proto.FileStatResult{}, "", err
	}
	if err := dest.Sync(); err != nil {
		return proto.FileStatResult{}, "", err
	}

	// Whole-file integrity: the digest of the fully assembled local file.
	// SECURITY: this digest is computed over bytes supplied by the source agent;
	// it is NOT independent integrity against a compromised source (see the
	// per-chunk note in fetchChunk). Its value is (a) detecting assembly bugs
	// and (b) letting a caller that already knows the expected digest detect a
	// substituted file; it is recorded in the audit trail.
	wholeSum := sha256Hex(nil)
	switch {
	case hasher != nil:
		if wholeSum, err = hasher.wait(); err != nil {
			return proto.FileStatResult{}, "", fmt.Errorf("hash assembled file: %w", err)
		}
	case len(chunks) == 1:
		wholeSum = d.digests[0]
	}
	if fi, serr := dest.Stat(); serr == nil && fi.Size() != totalSize {
		return proto.FileStatResult{}, "", fmt.Errorf("assembled file size %d does not match expected %d", fi.Size(), totalSize)
	}
	if stat.Entry.Mode != 0 {
		if err := dest.Chmod(os.FileMode(stat.Entry.Mode).Perm()); err != nil {
			return proto.FileStatResult{}, "", fmt.Errorf("set downloaded file mode: %w", err)
		}
	}
	if err := atomicDest.Commit(); err != nil {
		return proto.FileStatResult{}, "", fmt.Errorf("install local destination: %w", err)
	}
	d.progress.emit(true, nil)

	_ = a.AuditLog.Append(logs.AuditEntry{
		Action:   "file.download",
		Target:   serverName,
		Operator: a.operator(),
		Details:  fmt.Sprintf("%s:%s -> %s (%d bytes, sha256=%s)", serverName, remotePath, localPath, totalSize, wholeSum),
	})
	return stat, wholeSum, nil
}

// statAndFirstChunk learns what remotePath is. Agents with
// CapabilityFileReadStat answer with the metadata and the first chunk in one
// round trip — a small file is then already downloaded; older agents get a
// plain file.stat.
func statAndFirstChunk(conn *transferConn, remotePath string, chunkSize int64) (proto.FileStatResult, *proto.FileReadResult, error) {
	if conn.supports(proto.CapabilityFileReadStat) {
		res, err := decodeResult[proto.FileReadResult](conn.callRetry(0, proto.Envelope{
			Action: proto.ActionFileRead,
			Payload: proto.FileReadPayload{
				Path: remotePath, Offset: 0, Length: chunkSize, Binary: conn.binaryFrames, Stat: true,
			},
		}))
		if err == nil && res.Entry != nil {
			if res.Offset != 0 {
				return proto.FileStatResult{Entry: *res.Entry}, nil, nil
			}
			return proto.FileStatResult{Entry: *res.Entry}, &res, nil
		}
		if err != nil && remoteErrorCode(err) == "" {
			return proto.FileStatResult{}, nil, fmt.Errorf("stat remote file: %w", err)
		}
		// An agent error (e.g. reading a directory on an agent that ignored
		// Stat) is answered authoritatively by file.stat below.
	}
	stat, err := decodeResult[proto.FileStatResult](conn.callRetry(0, proto.Envelope{Action: proto.ActionFileStat, Payload: proto.FileStatPayload{Path: remotePath}}))
	if err != nil {
		return proto.FileStatResult{}, nil, fmt.Errorf("stat remote file: %w", err)
	}
	return stat, nil, nil
}

type download struct {
	conn       *transferConn
	remotePath string
	dest       *os.File
	chunks     []chunkSpec
	digests    []string
	progress   *progressTracker
	hasher     *orderedFileHasher
}

func (d *download) chunkDone(i int, sum string) {
	d.digests[i] = sum
	d.progress.add(d.chunks[i].length)
	if d.hasher != nil {
		d.hasher.markReady(i)
	}
}

func (d *download) fetchChunk(slot, idx int, binary bool) error {
	c := d.chunks[idx]
	var (
		res  proto.FileReadResult
		rerr error
	)
	for attempt := 0; attempt <= sshMaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(transferRetryDelay)
		}
		res, rerr = decodeResult[proto.FileReadResult](d.conn.send(slot, proto.Envelope{
			Action: proto.ActionFileRead,
			Payload: proto.FileReadPayload{
				Path: d.remotePath, Offset: c.offset, Length: c.length,
				// Ask for the bytes as a binary frame when the agent supports
				// it; older agents ignore the field and reply with base64
				// inside the envelope as before.
				Binary: binary,
			},
		}))
		// Reject a chunk whose payload is not exactly the range we asked for: a
		// short/over-long chunk would otherwise be written at c.offset and
		// silently corrupt the assembled file (and a huge one is an
		// allocation/DoS vector).
		if rerr == nil && int64(len(res.Data)) != c.length {
			rerr = fmt.Errorf("chunk length mismatch at %d: requested %d bytes, agent returned %d", c.offset, c.length, len(res.Data))
		}
		// SECURITY: this only confirms the chunk's bytes match the SHA the
		// *same agent* sent alongside them — it is a transport-corruption
		// check, NOT independent integrity: a fully-compromised SOURCE agent
		// can return wrong bytes with a matching SHA. The fleet-rpc channel is
		// host-key-pinned + encrypted, so a network MITM cannot alter chunks in
		// flight; the residual trust is in the source host itself.
		if rerr == nil && sha256Hex(res.Data) != res.SHA256 {
			rerr = fmt.Errorf("chunk checksum mismatch at %d", c.offset)
		}
		if rerr == nil {
			break
		}
	}
	if rerr == nil {
		if _, err := d.dest.WriteAt(res.Data, c.offset); err != nil {
			rerr = fmt.Errorf("write local chunk: %w", err)
		}
	}
	if rerr != nil {
		return fmt.Errorf("read chunk at %d: %w", c.offset, rerr)
	}
	d.chunkDone(idx, res.SHA256)
	return nil
}

// verifyPrefix checks which chunks of an existing local partial file already
// match the remote source (local hashes computed in parallel, remote ones via a
// probe the agent may also parallelise) and returns the ones to fetch.
func (d *download) verifyPrefix(a *App, server ServerRecord, candidates []int) []int {
	local := hashFileChunks(d.dest, d.chunks, candidates)
	ranges := make([]proto.FileRange, 0, len(candidates))
	for _, i := range candidates {
		ranges = append(ranges, proto.FileRange{Offset: d.chunks[i].offset, Length: d.chunks[i].length})
	}
	res, err := decodeResult[proto.FileProbeResult](a.callRPC(server, proto.Envelope{
		Action:  proto.ActionFileProbe,
		Payload: proto.FileProbePayload{Path: d.remotePath, Ranges: ranges},
	}))
	got := make(map[int64]proto.FileRangeChecksum)
	if err == nil {
		for _, rc := range res.RangeChecksums {
			got[rc.Offset] = rc
		}
	}
	var fetch []int
	for _, i := range candidates {
		c := d.chunks[i]
		if rc, ok := got[c.offset]; ok && rc.Length == c.length && local[i] != "" && rc.SHA256 == local[i] {
			d.chunkDone(i, local[i])
			d.progress.emit(false, nil)
			continue
		}
		fetch = append(fetch, i)
	}
	return fetch
}

// orderedFileHasher computes the SHA-256 of a file being assembled out of order,
// hashing each chunk as soon as every chunk before it is on disk.
type orderedFileHasher struct {
	f      io.ReaderAt
	chunks []chunkSpec

	mu      sync.Mutex
	cond    *sync.Cond
	ready   []bool
	stopped bool

	done chan struct{}
	sum  string
	err  error
}

func newOrderedFileHasher(f io.ReaderAt, chunks []chunkSpec) *orderedFileHasher {
	o := &orderedFileHasher{f: f, chunks: chunks, ready: make([]bool, len(chunks)), done: make(chan struct{})}
	o.cond = sync.NewCond(&o.mu)
	go o.run()
	return o
}

func (o *orderedFileHasher) markReady(i int) {
	o.mu.Lock()
	o.ready[i] = true
	o.mu.Unlock()
	o.cond.Broadcast()
}

func (o *orderedFileHasher) run() {
	defer close(o.done)
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for i, c := range o.chunks {
		o.mu.Lock()
		for !o.ready[i] && !o.stopped {
			o.cond.Wait()
		}
		stopped := o.stopped
		o.mu.Unlock()
		if stopped {
			o.err = context.Canceled
			return
		}
		n, err := io.CopyBuffer(h, io.NewSectionReader(o.f, c.offset, c.length), buf)
		if err == nil && n != c.length {
			err = io.ErrUnexpectedEOF
		}
		if err != nil {
			o.err = err
			return
		}
	}
	o.sum = hex.EncodeToString(h.Sum(nil))
}

func (o *orderedFileHasher) wait() (string, error) {
	<-o.done
	return o.sum, o.err
}

func (o *orderedFileHasher) cancel() {
	o.mu.Lock()
	o.stopped = true
	o.mu.Unlock()
	o.cond.Broadcast()
	<-o.done
}

// ---- small helpers ----

func resolveUploadRemotePath(style TargetPathStyle, remoteDir, remotePath, localPath string) (string, error) {
	base := filepath.Base(localPath)
	switch {
	case remotePath == "":
		if remoteDir == "" {
			return "", fmt.Errorf("remote path required: no default remote dir is configured (set one with 'fleet file defaults set' or pass an explicit remote path)")
		}
		return style.Join(remoteDir, base), nil
	case style.HasTrailingSeparator(remotePath):
		return style.Join(remotePath, base), nil
	default:
		return style.Clean(remotePath), nil
	}
}

type wholeFileHashJob struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	res    struct {
		sum string
		err error
	}
}

// startWholeFileHash hashes a fixed section of the already-open upload source.
// SectionReader uses ReadAt, so it is safe alongside transfer workers and cannot
// be redirected by swapping the source path after UploadFile opened it.
func startWholeFileHash(file *os.File, size int64) *wholeFileHashJob {
	ctx, cancel := context.WithCancel(context.Background())
	job := &wholeFileHashJob{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer cancel()
		defer close(job.done)
		h := sha256.New()
		r := io.NewSectionReader(file, 0, size)
		buf := make([]byte, 1<<20)
		for {
			select {
			case <-ctx.Done():
				job.res.err = ctx.Err()
				return
			default:
			}
			n, err := r.Read(buf)
			if n > 0 {
				if _, werr := h.Write(buf[:n]); werr != nil {
					job.res.err = fmt.Errorf("hash local file: %w", werr)
					return
				}
			}
			if err == io.EOF {
				job.res.sum = hex.EncodeToString(h.Sum(nil))
				return
			}
			if err != nil {
				job.res.err = fmt.Errorf("hash local file: %w", err)
				return
			}
		}
	}()
	return job
}

func (j *wholeFileHashJob) Wait() (string, error) {
	<-j.done
	return j.res.sum, j.res.err
}

func (j *wholeFileHashJob) CancelAndWait() {
	if j == nil {
		return
	}
	j.once.Do(j.cancel)
	<-j.done
}

// fileIdentity is a cheap stand-in for "these bytes are the same bytes": size
// plus modification time at nanosecond resolution. It is used only to name the
// remote temp file for resume, never as an integrity check — finalize still
// verifies the assembled file.
func fileIdentity(info os.FileInfo) string {
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

func transferIDFor(remotePath string, size int64, wholeSum string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s|%d|%s", remotePath, size, wholeSum))
	return hex.EncodeToString(sum[:16])
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
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
