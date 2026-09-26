// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"os"
	"sync"
	"time"

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
		server.FileTransfer.RemoteDir = a.effectiveFileTransferDefaults(server).RemoteDir
		remotePath = InitialRemotePath(server)
	}
	resp, err := a.callRPC(server, proto.Envelope{
		Action:  proto.ActionFileList,
		Payload: proto.FileListPayload{Path: remotePath, ShowHidden: showHidden},
	})
	if err != nil {
		return proto.FileListResult{}, err
	}
	if resp.Error != nil {
		return proto.FileListResult{}, remoteErr(resp.Error)
	}
	return proto.DecodePayload[proto.FileListResult](resp.Payload)
}

// CopyFile copies a single file from one server to another. Within one server
// an agent with CapabilityFileCopy copies it locally, so no byte crosses the
// network. Otherwise the bytes are relayed through the controller as a
// pipeline — each chunk is read from the source and written to the destination
// while later chunks are still in flight, without a controller-side temp file.
// Works for any server mode. Progress is reported as a single 0..100% bar
// across both legs. An existing destination directory receives the file
// inside it, as with cp.
func (a *App) CopyFile(srcServer, srcPath, dstServer, dstPath string, opts FileTransferOptions, progress ProgressFunc) (proto.FileFinalizeResult, error) {
	dstRecord, err := a.GetServer(dstServer)
	if err != nil {
		return proto.FileFinalizeResult{}, err
	}
	dstStyle := TargetPathStyleForServer(dstRecord)
	if err := ValidateTargetPath(dstStyle, dstPath); err != nil {
		return proto.FileFinalizeResult{}, err
	}
	if err := rejectDotComponents(dstStyle, dstPath); err != nil {
		return proto.FileFinalizeResult{}, err
	}
	srcRecord, err := a.GetServer(srcServer)
	if err != nil {
		return proto.FileFinalizeResult{}, err
	}
	if srcServer == dstServer {
		res, handled, err := a.copyOnServer(srcRecord, srcPath, dstPath, progress)
		if handled {
			return res, err
		}
	}
	return a.relayCopy(srcRecord, srcPath, dstRecord, dstPath, opts, progress)
}

// copyOnServer asks the agent to copy srcPath to dstPath itself. handled is
// false when the agent cannot, and the caller should relay instead.
func (a *App) copyOnServer(server ServerRecord, srcPath, dstPath string, progress ProgressFunc) (proto.FileFinalizeResult, bool, error) {
	conn, err := a.openTransferConn(server, 1)
	if err != nil {
		return proto.FileFinalizeResult{}, true, err
	}
	defer conn.closeFn()
	if !conn.supports(proto.CapabilityFileCopy) {
		return proto.FileFinalizeResult{}, false, nil
	}
	style := TargetPathStyleForServer(server)
	call := func(to string) (proto.FileFinalizeResult, error) {
		return decodeResult[proto.FileFinalizeResult](conn.callRetry(0, proto.Envelope{
			Action:  proto.ActionFileCopy,
			Payload: proto.FileCopyPayload{From: srcPath, To: to},
		}))
	}
	target := dstPath
	res, err := call(target)
	if remoteErrorCode(err) == errCodeTargetIsDirectory {
		target = style.Join(dstPath, style.Base(srcPath))
		if verr := ValidateTargetPath(style, target); verr != nil {
			return proto.FileFinalizeResult{}, true, verr
		}
		res, err = call(target)
	}
	if remoteErrorCode(err) == errCodeUnsupportedAction {
		return proto.FileFinalizeResult{}, false, nil
	}
	if err != nil {
		return proto.FileFinalizeResult{}, true, err
	}
	if res.SHA256 == "" {
		return res, true, fmt.Errorf("copy integrity check failed: agent returned no digest for %s", target)
	}
	if progress != nil {
		total := max(res.Size*2, 1)
		progress(ProgressUpdate{BytesDone: total, TotalBytes: total, Done: true})
	}
	_ = a.AuditLog.Append(logs.AuditEntry{
		Action:   "file.copy",
		Target:   server.Name + " -> " + server.Name,
		Operator: a.operator(),
		Details:  fmt.Sprintf("%s:%s -> %s:%s (%d bytes, sha256=%s)", server.Name, srcPath, server.Name, target, res.Size, res.SHA256),
	})
	return res, true, nil
}

// relayCopy streams a file from one server to another through the controller.
func (a *App) relayCopy(src ServerRecord, srcPath string, dst ServerRecord, dstPath string, opts FileTransferOptions, progress ProgressFunc) (proto.FileFinalizeResult, error) {
	srcOpts := a.resolveTransferOptions(src, opts)
	dstOpts := a.resolveTransferOptions(dst, opts)
	chunkSize := min(srcOpts.ChunkSize, dstOpts.ChunkSize)
	parallel := min(srcOpts.Parallel, dstOpts.Parallel)

	srcConn, err := a.openTransferConn(src, 1)
	if err != nil {
		return proto.FileFinalizeResult{}, err
	}
	defer srcConn.closeFn()
	stat, first, err := statAndFirstChunk(srcConn, srcPath, chunkSize)
	if err != nil {
		return proto.FileFinalizeResult{}, fmt.Errorf("stat source: %w", err)
	}
	if err := requireRemoteRegular(stat.Entry, srcPath); err != nil {
		return proto.FileFinalizeResult{}, err
	}
	size := stat.Entry.Size
	if err := validateTransferSize(size, chunkSize); err != nil {
		return proto.FileFinalizeResult{}, fmt.Errorf("refusing to copy %s: %w", srcPath, err)
	}
	chunks := buildChunks(size, chunkSize)
	if first != nil && (len(chunks) == 0 || int64(len(first.Data)) != chunks[0].length || sha256Hex(first.Data) != first.SHA256) {
		first = nil
	}
	streams := min(parallel, max(len(chunks), 1))
	dstConn, err := a.openTransferConn(dst, streams)
	if err != nil {
		return proto.FileFinalizeResult{}, err
	}
	defer dstConn.closeFn()
	srcConn.grow(streams)

	total := size * 2
	if total == 0 {
		total = 1
	}
	r := &relay{
		srcConn: srcConn, dstConn: dstConn, srcPath: srcPath, chunks: chunks, first: first, size: size,
		mode: os.FileMode(stat.Entry.Mode).Perm(), progress: newProgressTracker(progress, total),
	}
	srcStyle := TargetPathStyleForServer(src)
	dstStyle := TargetPathStyleForServer(dst)
	target := dstPath
	if !dstConn.supports(proto.CapabilityFileChunkDigests) {
		// Older agents only notice a directory destination at the final rename.
		if st, err := decodeResult[proto.FileStatResult](dstConn.callRetry(0, proto.Envelope{Action: proto.ActionFileStat, Payload: proto.FileStatPayload{Path: target}})); err == nil && st.Entry.IsDir && !st.Entry.IsSymlink {
			target = dstStyle.Join(dstPath, srcStyle.Base(srcPath))
		}
	}
	res, relaySum, err := r.run(target)
	if remoteErrorCode(err) == errCodeTargetIsDirectory {
		target = dstStyle.Join(dstPath, srcStyle.Base(srcPath))
		if verr := ValidateTargetPath(dstStyle, target); verr != nil {
			return proto.FileFinalizeResult{}, verr
		}
		res, relaySum, err = r.run(target)
	}
	if err != nil {
		return res, fmt.Errorf("relay copy: %w", err)
	}
	// The relay is only verified end-to-end if the destination returns a
	// non-empty finalize digest that MATCHES the bytes relayed. Treating an
	// empty or missing digest as a pass would let a malicious destination
	// silently store different bytes while the controller reports success, so
	// require a real matching digest and fail closed otherwise.
	if res.SHA256 == "" {
		return res, fmt.Errorf("relay integrity check failed: destination returned no finalize digest (cannot confirm %d bytes hashed %s landed intact)", size, relaySum)
	}
	if res.SHA256 != relaySum {
		return res, fmt.Errorf("relay integrity check failed: source bytes hashed %s but destination finalized %s", relaySum, res.SHA256)
	}
	r.progress.mu.Lock()
	r.progress.bytesDone = total
	r.progress.mu.Unlock()
	r.progress.emit(true, nil)
	_ = a.AuditLog.Append(logs.AuditEntry{
		Action:   "file.copy",
		Target:   src.Name + " -> " + dst.Name,
		Operator: a.operator(),
		Details:  fmt.Sprintf("%s:%s -> %s:%s (%d bytes, sha256=%s)", src.Name, srcPath, dst.Name, target, size, relaySum),
	})
	return res, nil
}

type relay struct {
	srcConn, dstConn *transferConn
	srcPath          string
	chunks           []chunkSpec
	first            *proto.FileReadResult
	size             int64
	mode             os.FileMode
	progress         *progressTracker
}

// run relays the file to target and returns the destination's finalize result
// and the SHA-256 of the bytes relayed.
func (r *relay) run(target string) (proto.FileFinalizeResult, string, error) {
	if len(r.chunks) <= 1 && r.dstConn.supports(proto.CapabilityFilePut) {
		res, sum, err := r.put(target)
		if remoteErrorCode(err) != errCodeUnsupportedAction {
			return res, sum, err
		}
	}
	return r.chunked(target)
}

func (r *relay) readChunk(slot, idx int) (proto.FileReadResult, error) {
	if idx == 0 && r.first != nil {
		return *r.first, nil
	}
	c := r.chunks[idx]
	var res proto.FileReadResult
	var err error
	for attempt := 0; attempt <= sshMaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(transferRetryDelay)
		}
		res, err = decodeResult[proto.FileReadResult](r.srcConn.send(slot, proto.Envelope{
			Action:  proto.ActionFileRead,
			Payload: proto.FileReadPayload{Path: r.srcPath, Offset: c.offset, Length: c.length, Binary: r.srcConn.binaryFrames},
		}))
		if err == nil && int64(len(res.Data)) != c.length {
			err = fmt.Errorf("chunk length mismatch at %d: requested %d bytes, source returned %d", c.offset, c.length, len(res.Data))
		}
		if err == nil {
			return res, nil
		}
	}
	return res, fmt.Errorf("read source chunk at %d: %w", c.offset, err)
}

func (r *relay) put(target string) (proto.FileFinalizeResult, string, error) {
	var data []byte
	sum := sha256Hex(nil)
	if len(r.chunks) == 1 {
		res, err := r.readChunk(0, 0)
		if err != nil {
			return proto.FileFinalizeResult{}, "", err
		}
		data, sum = res.Data, res.SHA256
	}
	r.progress.add(r.size)
	env := proto.Envelope{Action: proto.ActionFilePut, Payload: &proto.FilePutPayload{
		Path: target, Mode: uint32(r.mode), Data: data, SHA256: sum,
	}}
	if r.dstConn.binaryFrames {
		env = proto.DetachBinary(env)
	}
	res, err := decodeResult[proto.FileFinalizeResult](r.dstConn.callRetry(0, env))
	if err != nil {
		return proto.FileFinalizeResult{}, "", err
	}
	return res, sum, nil
}

func (r *relay) chunked(target string) (proto.FileFinalizeResult, string, error) {
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return proto.FileFinalizeResult{}, "", err
	}
	transferID := hex.EncodeToString(idBytes[:])
	ow, err := decodeResult[proto.FileOpenWriteResult](r.dstConn.callRetry(0, proto.Envelope{Action: proto.ActionFileOpenWrite, Payload: proto.FileOpenWritePayload{
		Path: target, TotalSize: r.size, Mode: uint32(r.mode), TransferID: transferID,
	}}))
	if err != nil {
		return proto.FileFinalizeResult{}, "", err
	}
	// A relay's temp file is keyed by a random id, so it can never be resumed:
	// discard it on any failure instead of leaving it beside the destination.
	finalized := false
	defer func() {
		if !finalized {
			r.discard(target, transferID, ow.TempPath)
		}
	}()

	digests := make([]string, len(r.chunks))
	whole := newOrderedHasher()
	jobs := make(chan int, len(r.chunks))
	for i := range r.chunks {
		jobs <- i
	}
	close(jobs)
	var errs firstError
	fail := func(err error) {
		errs.set(err)
		whole.abort()
	}
	var wg sync.WaitGroup
	for slot := range min(r.srcConn.slots(), r.dstConn.slots()) {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			for idx := range jobs {
				if errs.get() != nil {
					return
				}
				data, sum, err := r.moveChunk(slot, target, transferID, idx)
				if err != nil {
					fail(err)
					return
				}
				digests[idx] = sum
				r.progress.add(2 * r.chunks[idx].length)
				r.progress.emit(false, nil)
				// Hash the relayed bytes in file order for the end-to-end
				// check; a chunk waits here only for the chunks before it.
				if !whole.feed(idx, data) {
					return
				}
			}
		}(slot)
	}
	wg.Wait()
	if err := errs.get(); err != nil {
		return proto.FileFinalizeResult{}, "", err
	}
	relaySum := whole.sum()
	final := proto.FileFinalizePayload{
		TransferID: transferID, Path: target, Mode: uint32(r.mode), WholeSHA256: relaySum, TotalSize: r.size,
	}
	if cd, err := chunkListDigest(r.size, r.chunks, digests); err == nil {
		final.ChunkDigest = cd
	}
	res, err := decodeResult[proto.FileFinalizeResult](r.dstConn.callRetry(0, proto.Envelope{Action: proto.ActionFileFinalize, Payload: final}))
	if err != nil {
		return proto.FileFinalizeResult{}, relaySum, fmt.Errorf("finalize destination: %w", err)
	}
	finalized = true // the agent removes its temp file on every finalize failure
	if res.ChunkDigest != "" && res.ChunkDigest != final.ChunkDigest {
		return res, relaySum, fmt.Errorf("relay integrity check failed: destination verified chunk digest %s, expected %s", res.ChunkDigest, final.ChunkDigest)
	}
	return res, relaySum, nil
}

// moveChunk reads one chunk from the source and writes it to the destination
// with the source's checksum, which the destination verifies before writing.
func (r *relay) moveChunk(slot int, target, transferID string, idx int) ([]byte, string, error) {
	c := r.chunks[idx]
	var werr error
	for attempt := 0; attempt <= sshMaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(transferRetryDelay)
		}
		res, err := r.readChunk(slot, idx)
		if err != nil {
			return nil, "", err
		}
		req := proto.Envelope{Action: proto.ActionFileWrite, Payload: &proto.FileWritePayload{
			TransferID: transferID, Path: target, Offset: c.offset, Data: res.Data, SHA256: res.SHA256,
		}}
		if r.dstConn.binaryFrames {
			req = proto.DetachBinary(req)
		}
		_, werr = decodeResult[proto.FileWriteResult](r.dstConn.send(slot, req))
		if werr == nil {
			return res.Data, res.SHA256, nil
		}
		if idx == 0 {
			r.first = nil // re-read rather than resend a chunk the destination refused
		}
	}
	return nil, "", fmt.Errorf("write destination chunk at %d: %w", c.offset, werr)
}

// discard removes an abandoned relay temp file on the destination.
func (r *relay) discard(target, transferID, tempPath string) {
	if r.dstConn.supports(proto.CapabilityFileChunkDigests) {
		// TotalSize -1 and a bogus digest make sure no agent could ever treat
		// this as a request to install the file.
		_, _ = r.dstConn.send(0, proto.Envelope{Action: proto.ActionFileFinalize, Payload: proto.FileFinalizePayload{
			TransferID: transferID, Path: target, TotalSize: -1, WholeSHA256: "0", Abort: true,
		}})
		return
	}
	if tempPath != "" {
		_, _ = r.dstConn.send(0, proto.Envelope{Action: proto.ActionFileDelete, Payload: proto.FileDeletePayload{Path: tempPath}})
	}
}

// orderedHasher hashes chunks in file order as workers hand them over out of
// order; a worker waits only for the chunks before its own.
type orderedHasher struct {
	mu      sync.Mutex
	cond    *sync.Cond
	next    int
	h       hash.Hash
	stopped bool
}

func newOrderedHasher() *orderedHasher {
	o := &orderedHasher{h: sha256.New()}
	o.cond = sync.NewCond(&o.mu)
	return o
}

// feed adds chunk i once chunks 0..i-1 are in. It returns false if the hasher
// was aborted.
func (o *orderedHasher) feed(i int, data []byte) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for o.next != i && !o.stopped {
		o.cond.Wait()
	}
	if o.stopped {
		return false
	}
	_, _ = o.h.Write(data)
	o.next++
	o.cond.Broadcast()
	return true
}

func (o *orderedHasher) abort() {
	o.mu.Lock()
	o.stopped = true
	o.mu.Unlock()
	o.cond.Broadcast()
}

func (o *orderedHasher) sum() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return hex.EncodeToString(o.h.Sum(nil))
}

// CopyDir recursively copies a directory tree from one server to another,
// preserving hidden and empty directories. Returns the number of regular files
// copied (directories are not counted).
func (a *App) CopyDir(srcServer, srcPath, dstServer, dstPath string, opts FileTransferOptions, progress ProgressFunc) (int, error) {
	srcRecord, err := a.GetServer(srcServer)
	if err != nil {
		return 0, err
	}
	dstRecord, err := a.GetServer(dstServer)
	if err != nil {
		return 0, err
	}
	srcStyle := TargetPathStyleForServer(srcRecord)
	dstStyle := TargetPathStyleForServer(dstRecord)
	if err := rejectDotComponents(dstStyle, dstPath); err != nil {
		return 0, err
	}
	srcPath = srcStyle.Clean(srcPath)
	dstPath = dstStyle.Clean(dstPath)
	if err := ValidateTargetPath(dstStyle, dstPath); err != nil {
		return 0, err
	}
	entries, err := a.scanRemoteDir(srcServer, srcPath)
	if err != nil {
		return 0, err
	}
	if err := validateTreeForStyle(entries, dstStyle); err != nil {
		return 0, err
	}
	if err := a.RemoteMkdir(dstServer, dstPath); err != nil {
		return 0, err
	}
	if err := forEachDirLevel(sortedDirKeys(entries), func(rel string) error {
		if err := a.RemoteMkdir(dstServer, dstStyle.Join(dstPath, rel)); err != nil {
			return fmt.Errorf("create destination directory %s: %w", rel, err)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	files := sortedFileKeys(entries)
	return runParallelTransfers(files, sumSizes(entries), progress, func(rel string, fp ProgressFunc) error {
		srcF := srcStyle.Join(srcPath, rel)
		dstF := dstStyle.Join(dstPath, rel)
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
	server, err := a.GetServer(serverName)
	if err != nil {
		return 0, 0, err
	}
	m, err := a.scanRemoteDir(serverName, TargetPathStyleForServer(server).Clean(remotePath))
	if err != nil {
		return 0, 0, err
	}
	for _, meta := range m {
		if meta.isDir() {
			continue
		}
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
		if meta.isDir() {
			continue
		}
		files++
		bytes += meta.size
	}
	return files, bytes, nil
}
