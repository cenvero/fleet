// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/pkg/proto"
)

// remoteFiles is the slice of *core.App the streaming download path uses. It
// exists so tests can substitute a remote that corrupts, truncates, stalls or
// changes a file mid-read; production always uses the App itself.
type remoteFiles interface {
	StatRemoteFile(serverName, remotePath string) (proto.FileStatResult, error)
	CatRemoteFile(serverName, remotePath string, w io.Writer) (int64, error)
	DownloadFile(serverName, remotePath, localPath string, opts core.FileTransferOptions, progress core.ProgressFunc) (proto.FileStatResult, error)
}

// Streaming download design
//
// The old handler ran the whole chunked download into a temp file, fsynced and
// re-hashed it, and only then sent the first byte, so a large file showed no
// browser progress at all for most of its transfer. Now:
//
//  1. The file is stat'ed; Content-Length and Content-Disposition are known.
//  2. Bytes are piped straight from CatRemoteFile (sequential reads, every
//     chunk SHA-256 verified before it is written) into the response, so the
//     browser starts receiving data after a single round trip.
//  3. For files above hybridStreamThreshold the parallel engine
//     (DownloadFile: multi-stream, binary frames) also runs into a private
//     temp dir. When it finishes first the response switches over: the prefix
//     already sent is cross-checked (SHA-256) against the parallel copy, and
//     the remainder is served from it. Total time therefore tracks the fast
//     engine while the first byte still arrives immediately.
//  4. The final byte is held back until the stream is verified: total size
//     equals the stat'ed size and a re-stat shows the file was not modified
//     while it was being read. Any failure aborts the connection before the
//     declared Content-Length is reached, so the browser marks the download as
//     failed instead of saving a silently corrupt or mixed file.
//  5. A client disconnect (or a cancel from the Transfers panel) stops the
//     sequential reader at its next chunk.

// hybridStreamThreshold is the size above which the parallel engine is also
// started. A var only so tests can lower it.
var hybridStreamThreshold int64 = 2 * proto.MaxRawChunkBytes

// backgroundDownloadSlots bounds concurrent parallel prefetches (each can use
// a file's worth of temp disk and several SSH channels). Beyond it, downloads
// simply stream sequentially.
var backgroundDownloadSlots = make(chan struct{}, 2)

var (
	errStreamTooLong  = errors.New("remote file grew while it was being downloaded")
	errStreamStopped  = errors.New("stream stopped")
	errStreamModified = errors.New("remote file changed while it was being downloaded; aborted to avoid saving a mixed copy")
)

// contentDisposition builds an RFC 6266 header value that survives any file
// name, including non-ASCII (RFC 2231 filename*) and quote characters.
func contentDisposition(kind, name string) string {
	if v := mime.FormatMediaType(kind, map[string]string{"filename": name}); v != "" {
		return v
	}
	return kind + `; filename="download"`
}

// streamOut writes a stream of known length to the response while holding its
// final byte back until the caller confirms the stream is valid.
type streamOut struct {
	w          http.ResponseWriter
	rc         *http.ResponseController
	size       int64
	accepted   int64
	tail       []byte
	hash       hash.Hash
	headerSent bool
	setHeaders func(http.Header)
	onProgress func(int64)
}

func newStreamOut(w http.ResponseWriter, size int64, setHeaders func(http.Header), onProgress func(int64)) *streamOut {
	return &streamOut{
		w:          w,
		rc:         http.NewResponseController(w),
		size:       size,
		tail:       make([]byte, 0, 1),
		hash:       sha256.New(),
		setHeaders: setHeaders,
		onProgress: onProgress,
	}
}

func (o *streamOut) sendHeaders() {
	if o.headerSent {
		return
	}
	h := o.w.Header()
	o.setHeaders(h)
	h.Set("Content-Length", strconv.FormatInt(o.size, 10))
	o.w.WriteHeader(http.StatusOK)
	o.headerSent = true
}

// accept appends p to the stream. Everything but the last byte seen so far is
// written through; the last byte waits for finish.
func (o *streamOut) accept(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	if o.accepted+int64(len(p)) > o.size {
		return errStreamTooLong
	}
	o.hash.Write(p)
	o.accepted += int64(len(p))
	o.sendHeaders()
	if len(o.tail) > 0 {
		if _, err := o.w.Write(o.tail); err != nil {
			return err
		}
		o.tail = o.tail[:0]
	}
	if _, err := o.w.Write(p[:len(p)-1]); err != nil {
		return err
	}
	o.tail = append(o.tail, p[len(p)-1])
	_ = o.rc.Flush()
	if o.onProgress != nil {
		o.onProgress(o.accepted)
	}
	return nil
}

// finish releases the held-back byte, completing the response.
func (o *streamOut) finish() error {
	if o.accepted != o.size {
		return fmt.Errorf("short read: got %d of %d bytes", o.accepted, o.size)
	}
	o.sendHeaders()
	if len(o.tail) > 0 {
		if _, err := o.w.Write(o.tail); err != nil {
			return err
		}
		o.tail = o.tail[:0]
	}
	_ = o.rc.Flush()
	return nil
}

func (o *streamOut) sum() string { return hex.EncodeToString(o.hash.Sum(nil)) }

// streamAbort marks an error that occurred after response headers were sent:
// the only honest way to report it is to abort the connection.
type streamAbort struct{ err error }

func (a *streamAbort) Error() string { return a.err.Error() }
func (a *streamAbort) Unwrap() error { return a.err }

// failed wraps err as a *streamAbort when bytes already went out.
func (o *streamOut) failed(err error) error {
	if o.headerSent {
		return &streamAbort{err: err}
	}
	return err
}

// catSink is the io.Writer handed to CatRemoteFile. Writes are serialized with
// stop() so that once the handler takes the stream over (or gives up), no
// further byte from the sequential reader can reach the response.
type catSink struct {
	mu      sync.Mutex
	ctx     context.Context
	out     *streamOut
	stopped bool
	outErr  error
}

func (c *catSink) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return 0, errStreamStopped
	}
	if c.ctx.Err() != nil {
		c.stopped = true
		c.outErr = cancelError(c.ctx)
		return 0, c.outErr
	}
	if err := c.out.accept(p); err != nil {
		c.stopped = true
		c.outErr = err
		return 0, err
	}
	return len(p), nil
}

// stop prevents further writes and reports whether the output side failed.
func (c *catSink) stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	return c.outErr
}

// bgDownload is a parallel-engine download into a private temp dir.
type bgDownload struct {
	dir  string
	path string
	done chan struct{}
	stat proto.FileStatResult
	err  error
}

func (s *Server) startBackgroundDownload(server, remotePath string) *bgDownload {
	select {
	case backgroundDownloadSlots <- struct{}{}:
	default:
		return nil
	}
	dir, err := os.MkdirTemp("", "fleet-webui-stream-*")
	if err != nil {
		<-backgroundDownloadSlots
		return nil
	}
	bg := &bgDownload{dir: dir, path: filepath.Join(dir, "payload"), done: make(chan struct{})}
	go func() {
		defer func() { <-backgroundDownloadSlots }()
		defer close(bg.done)
		bg.stat, bg.err = s.files.DownloadFile(server, remotePath, bg.path, core.FileTransferOptions{}, nil)
	}()
	return bg
}

// release deletes the temp dir once the (uncancellable) engine has returned.
func (bg *bgDownload) release() {
	if bg == nil {
		return
	}
	go func() {
		<-bg.done
		_ = os.RemoveAll(bg.dir)
	}()
}

// requireRegularEntry mirrors core's regular-file requirement so a directory
// or special file fails before any header is written.
func requireRegularEntry(entry proto.FileEntry, p string) error {
	if entry.IsDir || entry.Type == proto.FileEntryTypeDirectory {
		return fmt.Errorf("cannot download a directory")
	}
	if entry.IsSymlink || (entry.Type != "" && entry.Type != proto.FileEntryTypeRegular) {
		return fmt.Errorf("%s is not a regular file", p)
	}
	if entry.Size < 0 {
		return fmt.Errorf("invalid remote file size %d", entry.Size)
	}
	return nil
}

func sameVersion(a, b proto.FileEntry) bool {
	return a.Size == b.Size && a.ModTime.Equal(b.ModTime)
}

// streamRemote pipes a remote regular file to w (see the design note above)
// and returns the whole-stream SHA-256. Errors after the first byte are
// returned as *streamAbort; the HTTP handler must then abort the connection.
func (s *Server) streamRemote(ctx context.Context, w http.ResponseWriter, server, remotePath string, entry proto.FileEntry, setHeaders func(http.Header), onProgress func(int64), allowParallel bool) (string, error) {
	out := newStreamOut(w, entry.Size, setHeaders, onProgress)
	sink := &catSink{ctx: ctx, out: out}

	catDone := make(chan error, 1)
	go func() {
		_, err := s.files.CatRemoteFile(server, remotePath, sink)
		catDone <- err
	}()

	var bg *bgDownload
	if allowParallel && entry.Size > hybridStreamThreshold {
		bg = s.startBackgroundDownload(server, remotePath)
	}
	defer bg.release()
	var bgDone <-chan struct{}
	if bg != nil {
		bgDone = bg.done
	}

	complete := func() (string, error) {
		if err := s.verifyStreamEnd(server, remotePath, entry, out); err != nil {
			return "", out.failed(err)
		}
		if err := out.finish(); err != nil {
			return "", out.failed(err)
		}
		return out.sum(), nil
	}

	var catErr error
	catCh := (<-chan error)(catDone)
	for {
		select {
		case <-ctx.Done():
			_ = sink.stop()
			return "", out.failed(cancelError(ctx))
		case err := <-catCh:
			catCh = nil
			if outErr := sink.stop(); outErr != nil {
				return "", out.failed(outErr)
			}
			if err == nil {
				return complete()
			}
			catErr = err
			if bgDone == nil {
				return "", out.failed(catErr)
			}
			// The sequential reader failed; the parallel engine may still
			// deliver a verified copy to finish from.
		case <-bgDone:
			bgDone = nil
			if bg.err != nil {
				if catCh == nil {
					return "", out.failed(fmt.Errorf("%v (parallel download also failed: %v)", catErr, bg.err))
				}
				continue // keep streaming sequentially
			}
			if outErr := sink.stop(); outErr != nil {
				return "", out.failed(outErr)
			}
			if err := s.finishFromLocal(ctx, out, bg, entry); err != nil {
				return "", out.failed(err)
			}
			return complete()
		}
	}
}

// verifyStreamEnd checks the stream is complete and that the file did not
// change while it was read (size + mtime re-stat).
func (s *Server) verifyStreamEnd(server, remotePath string, entry proto.FileEntry, out *streamOut) error {
	if out.accepted != entry.Size {
		return fmt.Errorf("short read: got %d of %d bytes", out.accepted, entry.Size)
	}
	again, err := s.files.StatRemoteFile(server, remotePath)
	if err != nil {
		return fmt.Errorf("verify download: %w", err)
	}
	if !sameVersion(again.Entry, entry) {
		return errStreamModified
	}
	return nil
}

// finishFromLocal completes the response from the parallel engine's verified
// copy after checking it matches everything already streamed.
func (s *Server) finishFromLocal(ctx context.Context, out *streamOut, bg *bgDownload, entry proto.FileEntry) error {
	if !sameVersion(bg.stat.Entry, entry) {
		return errStreamModified
	}
	f, err := os.Open(bg.path) // #nosec G304 -- path is inside a private os.MkdirTemp directory owned by this process
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() != entry.Size {
		return fmt.Errorf("parallel download size %d does not match expected %d", info.Size(), entry.Size)
	}
	// Cross-check: the bytes already sent must equal the same prefix of the
	// independently fetched copy, or the two reads saw different content.
	prefix := sha256.New()
	if _, err := io.CopyN(prefix, f, out.accepted); err != nil {
		return fmt.Errorf("verify streamed prefix: %w", err)
	}
	if !bytes.Equal(prefix.Sum(nil), out.hash.Sum(nil)) {
		return errStreamModified
	}
	buf := make([]byte, 256<<10)
	for {
		if ctx.Err() != nil {
			return cancelError(ctx)
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := out.accept(buf[:n]); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// cancelError explains why a streaming transfer's context ended.
func cancelError(ctx context.Context) error {
	cause := context.Cause(ctx)
	if cause == nil || errors.Is(cause, context.Canceled) {
		return errors.New("download cancelled (the browser closed the connection)")
	}
	return fmt.Errorf("download cancelled: %w", cause)
}

// ---- handlers ----

// handleDownload streams a file to the browser as an attachment. An optional
// `track` id (16-64 lowercase hex chars, chosen by the page) registers the
// download with the progress hub so the Transfers panel can show progress,
// speed and ETA and offer cancellation.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	server := q.Get("server")
	p := q.Get("path")
	track := q.Get("track")

	tracked := false
	if track != "" {
		meta := transferMeta{Kind: "download", Label: s.displayBase(server, p), SrcServer: server, SrcPath: p}
		if err := s.hub.startWithID(track, meta); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tracked = true
	}
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	if tracked {
		s.hub.setCancel(track, func() { cancel(errors.New("cancelled from the Transfers panel")) })
	}
	started := time.Now()
	progress := func(done, total int64) {
		if !tracked {
			return
		}
		rate := 0.0
		if el := time.Since(started).Seconds(); el > 0 {
			rate = float64(done) / el
		}
		s.hub.update(track, core.ProgressUpdate{BytesDone: done, TotalBytes: total, RatePerSec: rate, ActiveStreams: 1})
	}

	err := s.download(ctx, w, r, server, p, progress)
	if tracked {
		s.hub.finish(track, err)
	}
	var abort *streamAbort
	if errors.As(err, &abort) {
		// Headers (and some bytes) are already out: abort the connection so
		// the browser records a failed download, never a truncated "success".
		panic(http.ErrAbortHandler)
	}
}

// download serves one file and returns nil on a complete, verified response.
// Errors before the first byte are written as JSON; later errors are returned
// wrapped in *streamAbort for the caller to abort the connection.
func (s *Server) download(ctx context.Context, w http.ResponseWriter, r *http.Request, server, p string, progress func(done, total int64)) error {
	if p == "" {
		err := errors.New("path is required")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return err
	}
	if server == "" {
		return s.downloadLocal(ctx, w, r, p, progress)
	}
	style, err := s.targetPathStyle(server)
	if err != nil {
		writeError(w, err)
		return err
	}
	stat, err := s.files.StatRemoteFile(server, p)
	if err != nil {
		writeError(w, err)
		return err
	}
	if err := requireRegularEntry(stat.Entry, p); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return err
	}
	name := style.Base(p)
	size := stat.Entry.Size
	progress(0, size)
	setHeaders := func(h http.Header) {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", contentDisposition("attachment", name))
	}
	sum, err := s.streamRemote(ctx, w, server, p, stat.Entry, setHeaders, func(n int64) { progress(n, size) }, true)
	if err != nil {
		var abort *streamAbort
		if !errors.As(err, &abort) {
			writeError(w, err)
		}
		return err
	}
	s.audit("file.download", server, fmt.Sprintf("%s:%s -> web UI browser download (%d bytes, sha256=%s)", server, p, size, sum))
	return nil
}

// downloadLocal serves a controller-local file with http.ServeContent (which
// streams and honours Range requests) on an already-open descriptor, so the
// path cannot be swapped for a directory between the check and the read. It
// counts bytes for progress and stops when the transfer is cancelled.
func (s *Server) downloadLocal(ctx context.Context, w http.ResponseWriter, r *http.Request, p string, progress func(done, total int64)) error {
	clean, err := s.cleanLocal(p)
	if err != nil {
		writeError(w, err)
		return err
	}
	f, err := os.Open(clean) // #nosec G304,G703 -- localhost-only same-origin file manager intentionally accepts the operator-selected absolute local path (config dir refused by cleanLocal)
	if err != nil {
		writeError(w, err)
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, err)
		return err
	}
	if info.IsDir() || !info.Mode().IsRegular() {
		err := errors.New("cannot download a directory or special file")
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return err
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", filepath.Base(clean)))
	progress(0, info.Size())
	cw := &countingWriter{ResponseWriter: w, ctx: ctx, progress: func(n int64) { progress(n, info.Size()) }}
	http.ServeContent(cw, r.WithContext(ctx), filepath.Base(clean), info.ModTime(), f)
	if cw.err != nil {
		return &streamAbort{err: cw.err}
	}
	if cw.status >= 400 {
		return fmt.Errorf("download failed: HTTP %d", cw.status)
	}
	return nil
}

// countingWriter reports bytes written and refuses further writes once the
// transfer's context is cancelled.
type countingWriter struct {
	http.ResponseWriter
	ctx      context.Context
	n        int64
	status   int
	err      error
	progress func(int64)
}

func (c *countingWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.ctx.Err() != nil {
		c.err = cancelError(c.ctx)
		return 0, c.err
	}
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	if c.progress != nil {
		c.progress(c.n)
	}
	if err != nil && c.err == nil {
		c.err = err
	}
	return n, err
}

func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// audit records a web-UI action in the controller's tamper-evident audit log
// with the same operator attribution the CLI uses.
func (s *Server) audit(action, target, details string) {
	if s.app == nil || s.app.AuditLog == nil {
		return
	}
	_ = s.app.AuditLog.Append(logs.AuditEntry{
		Action:   action,
		Target:   target,
		Operator: s.operatorName(),
		Details:  details,
	})
}

func (s *Server) operatorName() string {
	if s.operator != "" {
		return s.operator
	}
	if s.app != nil && s.app.Config.Operator != "" {
		return s.app.Config.Operator
	}
	if u, err := user.Current(); err == nil && strings.TrimSpace(u.Username) != "" {
		return u.Username
	}
	return "unknown"
}
