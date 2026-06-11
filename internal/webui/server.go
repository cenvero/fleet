// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

// Package webui serves a localhost-only browser file manager for Cenvero Fleet.
// It binds 127.0.0.1 only, requires a per-process random token on every request,
// and rides the same authenticated SSH transport as the rest of the controller —
// it never opens a remote-reachable or unauthenticated surface.
package webui

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/pkg/proto"
)

// DefaultAddr is the default bind address for the web UI. Loopback only.
const DefaultAddr = "127.0.0.1:9445"

// Server is the localhost web file manager.
type Server struct {
	app   *core.App
	token string
	hub   *progressHub
}

// New builds a web UI server with a fresh random session token.
func New(app *core.App) (*Server, error) {
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	return &Server{app: app, token: token, hub: newProgressHub()}, nil
}

// Token returns the per-process access token.
func (s *Server) Token() string { return s.token }

// ListenAndServe binds addr (must be loopback) and serves until ctx is done.
// It prints the access URL (with token) once. If addr is empty, DefaultAddr.
// onReady, when non-nil, is called with the full access URL right after the
// server starts accepting connections — used to open a browser.
func (s *Server) ListenAndServe(ctx context.Context, addr string, out io.Writer, onReady func(url string)) error {
	if addr == "" {
		addr = DefaultAddr
	}
	if err := ensureLoopbackAddr(addr); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { // #nosec G118 -- server-lifecycle shutdown, not a request-scoped context
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	url := fmt.Sprintf("http://%s/?t=%s", listener.Addr(), s.token)
	fmt.Fprintf(out, "Cenvero Fleet web UI: %s\n", url)
	fmt.Fprintln(out, "Bound to loopback only. Keep the token private; press Ctrl-C to stop.")
	if onReady != nil {
		// Fire once the accept loop is running so the browser doesn't race the
		// listener and get connection-refused.
		go func() {
			time.Sleep(350 * time.Millisecond)
			onReady(url)
		}()
	}
	err = srv.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/app.js", s.staticAsset("app.js", "text/javascript"))
	mux.HandleFunc("/app.css", s.staticAsset("app.css", "text/css"))

	mux.HandleFunc("/api/servers", s.guard(s.handleServers))
	mux.HandleFunc("/api/formats", s.guard(s.handleFormats))
	mux.HandleFunc("/api/list", s.guard(s.handleList))
	mux.HandleFunc("/api/upload", s.guard(s.handleUpload))
	mux.HandleFunc("/api/download", s.guard(s.handleDownload))
	mux.HandleFunc("/api/read", s.guard(s.handleRead))
	mux.HandleFunc("/api/checksum", s.guard(s.handleChecksum))
	mux.HandleFunc("/api/progress", s.guard(s.handleProgress))
	// Mutating endpoints are POST-only so the guard's Origin/CSRF check applies
	// (a state-changing GET would slip past it).
	mux.HandleFunc("/api/mkdir", s.guard(postOnly(s.handleMkdir)))
	mux.HandleFunc("/api/touch", s.guard(postOnly(s.handleTouch)))
	mux.HandleFunc("/api/write", s.guard(postOnly(s.handleWrite)))
	mux.HandleFunc("/api/rm", s.guard(postOnly(s.handleRemove)))
	mux.HandleFunc("/api/mv", s.guard(postOnly(s.handleMove)))
	mux.HandleFunc("/api/copy", s.guard(postOnly(s.handleCopy)))
	mux.HandleFunc("/api/move", s.guard(postOnly(s.handleMoveTransfer)))
	mux.HandleFunc("/api/compress", s.guard(postOnly(s.handleCompress)))
	mux.HandleFunc("/api/extract", s.guard(postOnly(s.handleExtract)))
	mux.HandleFunc("/api/chmod", s.guard(postOnly(s.handleChmod)))
	mux.HandleFunc("/api/duplicate", s.guard(postOnly(s.handleDuplicate)))
	return securityHeaders(mux)
}

// maxWebUploadBytes caps a single browser upload spooled to the controller's
// temp dir, so a runaway request can't fill the disk. Large transfers should
// use the `fleet file upload` CLI instead.
const maxWebUploadBytes = 64 << 30 // 64 GiB

// maxEditBytes caps the in-browser text editor: files larger than this are
// rejected for read/write so the editor stays a lightweight text tool, not a
// way to slurp huge files into memory.
const maxEditBytes = 2 << 20 // 2 MiB

// securityHeaders sets conservative headers on every response: no caching of the
// token-bearing page, no framing/clickjacking, no MIME sniffing, and a strict
// same-origin CSP (the UI only loads its own /app.css, /app.js and /api/*).
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'none'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// postOnly rejects non-POST requests, used for state-changing endpoints so the
// guard's POST-gated Origin/CSRF check actually covers them.
func postOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

// guard enforces loopback origin + token on every API request.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRequest(r) {
			http.Error(w, "forbidden: loopback only", http.StatusForbidden)
			return
		}
		if !s.tokenOK(r) {
			http.Error(w, "unauthorized: missing or bad token", http.StatusUnauthorized)
			return
		}
		// CSRF: state-changing requests must originate from a loopback page.
		if r.Method == http.MethodPost && !originLoopback(r) {
			http.Error(w, "forbidden: bad origin", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) tokenOK(r *http.Request) bool {
	provided := r.Header.Get("X-Fleet-Token")
	if provided == "" {
		provided = r.URL.Query().Get("t")
	}
	if provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) == 1
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !isLoopbackRequest(r) {
		http.Error(w, "forbidden: loopback only", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) staticAsset(name, contentType string) http.HandlerFunc {
	data, _ := assets.ReadFile("assets/" + name)
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRequest(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", contentType+"; charset=utf-8")
		_, _ = w.Write(data)
	}
}

func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	servers, err := s.app.ListServers()
	if err != nil {
		writeError(w, err)
		return
	}
	type serverInfo struct {
		Name      string `json:"name"`
		Reachable bool   `json:"reachable"`
		Mode      string `json:"mode"`
	}
	out := make([]serverInfo, 0, len(servers))
	for _, srv := range servers {
		out = append(out, serverInfo{Name: srv.Name, Reachable: srv.Observed.Reachable, Mode: string(srv.Mode)})
	}
	writeJSON(w, out)
}

// handleFormats lists the archive formats the controller supports, so the
// Compress dialog can offer them without hard-coding the set in the frontend.
func (s *Server) handleFormats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, core.ArchiveFormats())
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	server := r.URL.Query().Get("server")
	dir := r.URL.Query().Get("path")
	showHidden := r.URL.Query().Get("hidden") == "1" || r.URL.Query().Get("hidden") == "true"
	if server == "" { // Local: the controller's own filesystem.
		result, err := listLocalDir(dir, showHidden)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, result)
		return
	}
	result, err := s.app.ListRemoteDirHidden(server, dir, showHidden)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, result)
}

// cleanLocalPath validates a controller-side path: it must be absolute, and is
// returned in cleaned form. An empty path defaults to "/" so the Local pane has
// a sensible root. Relative paths are rejected so a `dir` from the browser can't
// be resolved against the controller's working directory.
func cleanLocalPath(p string) (string, error) {
	if p == "" {
		p = "/"
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path must be absolute")
	}
	return filepath.Clean(p), nil
}

// listLocalDir reads a directory on the controller and returns it in the same
// JSON shape as a remote listing, so the frontend renders Local and server panes
// identically. Dotfiles are hidden unless showHidden is set.
func listLocalDir(dir string, showHidden bool) (proto.FileListResult, error) {
	clean, err := cleanLocalPath(dir)
	if err != nil {
		return proto.FileListResult{}, err
	}
	ents, err := os.ReadDir(clean)
	if err != nil {
		return proto.FileListResult{}, err
	}
	out := proto.FileListResult{Path: clean, Entries: make([]proto.FileEntry, 0, len(ents))}
	for _, de := range ents {
		name := de.Name()
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue // entry vanished between ReadDir and Info; skip it
		}
		out.Entries = append(out.Entries, proto.FileEntry{
			Name:      name,
			Path:      filepath.Join(clean, name),
			Size:      info.Size(),
			Mode:      uint32(info.Mode().Perm()),
			IsDir:     de.IsDir(),
			IsSymlink: info.Mode()&os.ModeSymlink != 0,
			ModTime:   info.ModTime(),
		})
	}
	return out, nil
}

// handleTransfer drives a copy or move between any two sources, tracked by the
// progress hub like an upload. A source is Local (the controller filesystem)
// when its server name is empty, else a managed server. move=true deletes the
// source after a successful copy. Routing covers all four combinations:
//
//	server→server : CopyFile/CopyDir   (move → MoveFile/MoveDir)
//	local →server : UploadFile/UploadDir (move → upload, then remove local src)
//	server→local  : DownloadFile/DownloadDir (move → download, then RemoteDelete)
//	local →local  : os copy of file/tree (move → os.Rename)
func (s *Server) handleTransfer(w http.ResponseWriter, r *http.Request, move bool) {
	q := r.URL.Query()
	srcServer, srcPath := q.Get("srcServer"), q.Get("srcPath")
	dstServer, dstPath := q.Get("dstServer"), q.Get("dstPath")
	recursive := q.Get("recursive") == "1" || q.Get("recursive") == "true"
	if srcPath == "" || dstPath == "" {
		http.Error(w, "srcPath and dstPath are required", http.StatusBadRequest)
		return
	}
	srcLocal := srcServer == ""
	dstLocal := dstServer == ""
	// Validate local endpoints up front so a bad path fails fast (and cleanly).
	if srcLocal {
		clean, err := cleanLocalPath(srcPath)
		if err != nil {
			writeError(w, err)
			return
		}
		srcPath = clean
	}
	if dstLocal {
		clean, err := cleanLocalPath(dstPath)
		if err != nil {
			writeError(w, err)
			return
		}
		dstPath = clean
	}

	id := s.hub.start()
	go func() {
		prog := func(u core.ProgressUpdate) { s.hub.update(id, u) }
		var err error
		switch {
		case !srcLocal && !dstLocal: // server → server
			err = s.transferServerToServer(srcServer, srcPath, dstServer, dstPath, recursive, move, prog)
		case srcLocal && !dstLocal: // local → server (upload)
			err = s.transferLocalToServer(srcPath, dstServer, dstPath, recursive, move, prog)
		case !srcLocal && dstLocal: // server → local (download)
			err = s.transferServerToLocal(srcServer, srcPath, dstPath, recursive, move, prog)
		default: // local → local
			err = transferLocalToLocal(srcPath, dstPath, move)
		}
		s.hub.finish(id, err)
	}()
	writeJSON(w, map[string]string{"id": id})
}

func (s *Server) transferServerToServer(srcServer, srcPath, dstServer, dstPath string, recursive, move bool, prog core.ProgressFunc) error {
	switch {
	case move && recursive:
		_, err := s.app.MoveDir(srcServer, srcPath, dstServer, dstPath, core.FileTransferOptions{}, prog)
		return err
	case move:
		return s.app.MoveFile(srcServer, srcPath, dstServer, dstPath, core.FileTransferOptions{}, prog)
	case recursive:
		_, err := s.app.CopyDir(srcServer, srcPath, dstServer, dstPath, core.FileTransferOptions{}, prog)
		return err
	default:
		_, err := s.app.CopyFile(srcServer, srcPath, dstServer, dstPath, core.FileTransferOptions{}, prog)
		return err
	}
}

func (s *Server) transferLocalToServer(srcPath, dstServer, dstPath string, recursive, move bool, prog core.ProgressFunc) error {
	var err error
	if recursive {
		_, err = s.app.UploadDir(dstServer, srcPath, dstPath, core.FileTransferOptions{}, prog)
	} else {
		_, err = s.app.UploadFile(dstServer, srcPath, dstPath, core.FileTransferOptions{}, prog)
	}
	if err != nil || !move {
		return err
	}
	if recursive {
		return os.RemoveAll(srcPath)
	}
	return os.Remove(srcPath)
}

func (s *Server) transferServerToLocal(srcServer, srcPath, dstPath string, recursive, move bool, prog core.ProgressFunc) error {
	var err error
	if recursive {
		_, err = s.app.DownloadDir(srcServer, srcPath, dstPath, core.FileTransferOptions{}, prog)
	} else {
		_, err = s.app.DownloadFile(srcServer, srcPath, dstPath, core.FileTransferOptions{}, prog)
	}
	if err != nil || !move {
		return err
	}
	return s.app.RemoteDelete(srcServer, srcPath, recursive)
}

func transferLocalToLocal(srcPath, dstPath string, move bool) error {
	if move {
		return os.Rename(srcPath, dstPath)
	}
	info, err := os.Lstat(srcPath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return copyLocalTree(srcPath, dstPath)
	}
	return copyLocalFile(srcPath, dstPath, info.Mode())
}

// copyLocalFile copies a single regular file, preserving permission bits.
func copyLocalFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src) // #nosec G304 -- path validated by cleanLocalPath
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm()) // #nosec G304
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// copyLocalTree recursively copies a directory tree on the controller.
func copyLocalTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		return copyLocalFile(p, target, info.Mode())
	})
}

func (s *Server) handleCopy(w http.ResponseWriter, r *http.Request) { s.handleTransfer(w, r, false) }
func (s *Server) handleMoveTransfer(w http.ResponseWriter, r *http.Request) {
	s.handleTransfer(w, r, true)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	server := r.URL.Query().Get("server")
	dir := path.Clean(r.URL.Query().Get("dir"))
	name := path.Base(r.URL.Query().Get("name"))
	if name == "" || name == "." || name == "/" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	// The agent re-validates every path, but reject a non-absolute target here
	// too so a relative `dir` can't produce a surprising join.
	if !path.IsAbs(dir) {
		http.Error(w, "dir must be an absolute path", http.StatusBadRequest)
		return
	}

	// A Local destination pane writes the uploaded bytes straight to disk on the
	// controller — no spooling or agent round-trip needed.
	if server == "" {
		localPath := filepath.Join(filepath.Clean(dir), name)
		// O_NOFOLLOW so a symlink planted at the destination can't redirect the
		// truncating write to an arbitrary file; a symlinked target is rejected.
		out, err := os.OpenFile(localPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|oNoFollow, 0o600) // #nosec G304 -- dir validated absolute above, O_NOFOLLOW set
		if err != nil {
			writeError(w, symlinkClobberError(localPath, err))
			return
		}
		if _, err := io.Copy(out, http.MaxBytesReader(w, r.Body, maxWebUploadBytes)); err != nil {
			_ = out.Close()
			writeError(w, err)
			return
		}
		if err := out.Close(); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, map[string]string{"local_path": localPath})
		return
	}

	// Spool the browser upload to a controller-side temp file (size-capped), then
	// run the chunked/parallel/resumable transfer from there to the agent.
	tmp, err := os.CreateTemp("", "fleet-webui-upload-*")
	if err != nil {
		writeError(w, err)
		return
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, maxWebUploadBytes)); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		writeError(w, err)
		return
	}
	_ = tmp.Close()

	id := s.hub.start()
	remotePath := path.Join(dir, name)
	go func() {
		defer os.Remove(tmpPath)
		_, err := s.app.UploadFile(server, tmpPath, remotePath, core.FileTransferOptions{}, func(u core.ProgressUpdate) {
			s.hub.update(id, u)
		})
		s.hub.finish(id, err)
	}()
	writeJSON(w, map[string]string{"id": id, "remote_path": remotePath})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	server := r.URL.Query().Get("server")
	remotePath := r.URL.Query().Get("path")
	if remotePath == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	if server == "" { // Local: stream the controller's file directly.
		clean, err := cleanLocalPath(remotePath)
		if err != nil {
			writeError(w, err)
			return
		}
		info, err := os.Stat(clean)
		if err != nil {
			writeError(w, err)
			return
		}
		if info.IsDir() {
			writeError(w, fmt.Errorf("cannot download a directory"))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(clean)))
		http.ServeFile(w, r, clean)
		return
	}
	tmp, err := os.CreateTemp("", "fleet-webui-download-*")
	if err != nil {
		writeError(w, err)
		return
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := s.app.DownloadFile(server, remotePath, tmpPath, core.FileTransferOptions{}, nil); err != nil {
		writeError(w, err)
		return
	}
	f, err := os.Open(tmpPath)
	if err != nil {
		writeError(w, err)
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(remotePath)))
	if info != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	}
	_, _ = io.Copy(w, f)
}

// looksBinary reports whether b appears to be binary (and thus not safe to show
// in the text editor): a NUL byte is a strong signal, and so is a high ratio of
// non-printable, non-UTF-8 control bytes.
func looksBinary(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return true
	}
	n := min(len(b), 8000) // sampling the head is enough to classify
	ctrl := 0
	for i := range n {
		c := b[i]
		if c < 0x09 || (c > 0x0d && c < 0x20) {
			ctrl++
		}
	}
	return n > 0 && ctrl*100/n > 10
}

// handleRead returns a text file's content for the in-browser editor. Files are
// capped at maxEditBytes and rejected if they look binary. Local files come from
// the controller's own disk; server files are streamed through the agent.
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	server := r.URL.Query().Get("server")
	p := r.URL.Query().Get("path")
	if p == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	var data []byte
	if server == "" { // Local: the controller's own filesystem.
		clean, err := cleanLocalPath(p)
		if err != nil {
			writeError(w, err)
			return
		}
		info, err := os.Stat(clean)
		if err != nil {
			writeError(w, err)
			return
		}
		if info.IsDir() {
			writeError(w, fmt.Errorf("cannot edit a directory"))
			return
		}
		if info.Size() > maxEditBytes {
			writeError(w, fmt.Errorf("file too large to edit (%s); limit is 2 MiB", humanBytes(info.Size())))
			return
		}
		data, err = os.ReadFile(clean) // #nosec G304 -- path validated by cleanLocalPath
		if err != nil {
			writeError(w, err)
			return
		}
	} else {
		// Stream the remote file into a capped buffer so an oversized file can't
		// balloon controller memory. The agent caps each chunk; we stop early once
		// we cross the limit.
		var buf bytes.Buffer
		if _, err := s.app.CatRemoteFile(server, p, &capWriter{w: &buf, limit: maxEditBytes + 1}); err != nil {
			if err == errEditTooLarge {
				writeError(w, fmt.Errorf("file too large to edit; limit is 2 MiB"))
				return
			}
			writeError(w, err)
			return
		}
		data = buf.Bytes()
		if int64(len(data)) > maxEditBytes {
			writeError(w, fmt.Errorf("file too large to edit; limit is 2 MiB"))
			return
		}
	}
	if looksBinary(data) {
		writeError(w, fmt.Errorf("file appears to be binary and cannot be edited as text"))
		return
	}
	writeJSON(w, map[string]any{"content": string(data), "size": len(data)})
}

// errEditTooLarge is the sentinel capWriter uses to abort an oversized remote
// read once it crosses the editor's size limit.
var errEditTooLarge = fmt.Errorf("edit limit exceeded")

// capWriter wraps a buffer and aborts the write once limit bytes have been
// accepted, so a large remote file is rejected without buffering all of it.
type capWriter struct {
	w     io.Writer
	limit int64
	n     int64
}

func (c *capWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	if c.n > c.limit {
		return 0, errEditTooLarge
	}
	return c.w.Write(p)
}

// handleWrite saves edited text back to a file. The request body is the new
// content (capped at maxEditBytes). Local writes hit the controller's disk at
// 0o600; server writes spool to a controller temp then upload to the agent.
func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	server := r.URL.Query().Get("server")
	p := r.URL.Query().Get("path")
	if p == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEditBytes))
	if err != nil {
		writeError(w, fmt.Errorf("content too large to save; limit is 2 MiB"))
		return
	}
	if server == "" { // Local
		clean, err := cleanLocalPath(p)
		if err != nil {
			writeError(w, err)
			return
		}
		// O_NOFOLLOW so an attacker-planted symlink at the edit target can't
		// redirect this truncating write elsewhere; a symlinked target errors out.
		f, err := os.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|oNoFollow, 0o600) // #nosec G304 -- path validated by cleanLocalPath, O_NOFOLLOW set
		if err != nil {
			writeError(w, symlinkClobberError(clean, err))
			return
		}
		if _, err := f.Write(body); err != nil {
			_ = f.Close()
			writeError(w, err)
			return
		}
		if err := f.Close(); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}
	// Spool to a controller temp file, then upload to the target path on the agent.
	tmp, err := os.CreateTemp("", "fleet-webui-edit-*")
	if err != nil {
		writeError(w, err)
		return
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		writeError(w, err)
		return
	}
	if err := tmp.Close(); err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.app.UploadFile(server, tmpPath, p, core.FileTransferOptions{}, nil); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleTouch creates a new empty file. Local: O_EXCL so it won't clobber an
// existing file; server: upload an empty temp to the target path.
func (s *Server) handleTouch(w http.ResponseWriter, r *http.Request) {
	server, p := r.URL.Query().Get("server"), r.URL.Query().Get("path")
	if p == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	if server == "" { // Local
		clean, err := cleanLocalPath(p)
		if err != nil {
			writeError(w, err)
			return
		}
		f, err := os.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- path validated
		if err != nil {
			writeError(w, err)
			return
		}
		if err := f.Close(); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}
	tmp, err := os.CreateTemp("", "fleet-webui-touch-*")
	if err != nil {
		writeError(w, err)
		return
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := s.app.UploadFile(server, tmpPath, p, core.FileTransferOptions{}, nil); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// humanBytes renders a byte count for an error message (rough, two-figure).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// maxSSELifetime caps how long a single progress (SSE) stream may stay open, so
// a client can't hold a connection — and a server goroutine — open forever even
// if the underlying transfer never reports Done.
const maxSSELifetime = 30 * time.Minute

func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Bound the stream's lifetime so it can't leak past the cap.
	ctx, cancel := context.WithTimeout(r.Context(), maxSSELifetime)
	defer cancel()

	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snap, ok := s.hub.snapshot(id)
			if !ok {
				continue
			}
			payload, _ := json.Marshal(snap)
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
			if snap.Done {
				return
			}
		}
	}
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	server, p := r.URL.Query().Get("server"), r.URL.Query().Get("path")
	if server == "" { // Local
		clean, err := cleanLocalPath(p)
		if err != nil {
			writeError(w, err)
			return
		}
		if err := os.Mkdir(clean, 0o750); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}
	if err := s.app.RemoteMkdir(server, p); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	server, p := r.URL.Query().Get("server"), r.URL.Query().Get("path")
	recursive := r.URL.Query().Get("recursive") == "true"
	if server == "" { // Local
		clean, err := cleanLocalPath(p)
		if err != nil {
			writeError(w, err)
			return
		}
		if clean == "/" {
			writeError(w, fmt.Errorf("refusing to remove root"))
			return
		}
		// RemoveAll handles both files and non-empty trees; the frontend only
		// passes recursive=true for directories but RemoveAll is safe either way.
		var rmErr error
		if recursive {
			rmErr = os.RemoveAll(clean)
		} else {
			rmErr = os.Remove(clean)
		}
		if rmErr != nil {
			writeError(w, rmErr)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}
	if err := s.app.RemoteDelete(server, p, recursive); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	server := r.URL.Query().Get("server")
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if server == "" { // Local rename
		cf, err := cleanLocalPath(from)
		if err != nil {
			writeError(w, err)
			return
		}
		ct, err := cleanLocalPath(to)
		if err != nil {
			writeError(w, err)
			return
		}
		if err := os.Rename(cf, ct); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}
	if err := s.app.RemoteRename(server, from, to); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleCompress archives the selected base names inside `dir` into a single
// archive. server=="" runs on the controller's local filesystem. The repeated
// `name` params are the base names of the selection (the agent re-quotes them);
// `dir` is validated absolute for the Local case.
func (s *Server) handleCompress(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, err)
		return
	}
	server := r.Form.Get("server")
	dir := r.Form.Get("dir")
	archive := path.Base(r.Form.Get("archive"))
	format := r.Form.Get("format")
	names := r.Form["name"]
	if archive == "" || archive == "." || archive == "/" {
		http.Error(w, "archive name is required", http.StatusBadRequest)
		return
	}
	if len(names) == 0 {
		http.Error(w, "at least one name is required", http.StatusBadRequest)
		return
	}
	if format == "" {
		format = core.FormatFromName(archive)
	}
	if server == "" {
		clean, err := cleanLocalPath(dir)
		if err != nil {
			writeError(w, err)
			return
		}
		dir = clean
	}
	if err := s.app.CompressPaths(server, dir, names, archive, format); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleExtract unpacks an archive into its containing directory.
func (s *Server) handleExtract(w http.ResponseWriter, r *http.Request) {
	server, p := r.URL.Query().Get("server"), r.URL.Query().Get("path")
	if p == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	if server == "" {
		clean, err := cleanLocalPath(p)
		if err != nil {
			writeError(w, err)
			return
		}
		p = clean
	}
	if err := s.app.ExtractArchive(server, p); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleChmod sets octal permissions (e.g. 644/755) on a path.
func (s *Server) handleChmod(w http.ResponseWriter, r *http.Request) {
	server, p, mode := r.URL.Query().Get("server"), r.URL.Query().Get("path"), r.URL.Query().Get("mode")
	if p == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	if mode == "" {
		http.Error(w, "mode is required", http.StatusBadRequest)
		return
	}
	if server == "" {
		clean, err := cleanLocalPath(p)
		if err != nil {
			writeError(w, err)
			return
		}
		p = clean
	}
	if err := s.app.ChmodPath(server, p, mode); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleChecksum returns the SHA-256 of a file as {"sha256": "..."}.
func (s *Server) handleChecksum(w http.ResponseWriter, r *http.Request) {
	server, p := r.URL.Query().Get("server"), r.URL.Query().Get("path")
	if p == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	if server == "" {
		clean, err := cleanLocalPath(p)
		if err != nil {
			writeError(w, err)
			return
		}
		p = clean
	}
	sum, err := s.app.ChecksumPath(server, p)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"sha256": sum})
}

// handleDuplicate copies a file/dir to a sibling named "<name> copy.<ext>".
// server=="" copies on the controller's local filesystem; otherwise it does a
// same-server agent copy.
func (s *Server) handleDuplicate(w http.ResponseWriter, r *http.Request) {
	server, p := r.URL.Query().Get("server"), r.URL.Query().Get("path")
	if p == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	if server == "" {
		clean, err := cleanLocalPath(p)
		if err != nil {
			writeError(w, err)
			return
		}
		info, err := os.Lstat(clean)
		if err != nil {
			writeError(w, err)
			return
		}
		dst := freeDuplicateName(clean, func(c string) bool { _, e := os.Lstat(c); return e == nil })
		if info.IsDir() {
			if err := copyLocalTree(clean, dst); err != nil {
				writeError(w, err)
				return
			}
		} else if err := copyLocalFile(clean, dst, info.Mode()); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, map[string]string{"status": "ok", "path": dst})
		return
	}
	dst := freeDuplicateName(p, func(c string) bool { _, e := s.app.StatRemoteFile(server, c); return e == nil })
	if _, err := s.app.CopyFile(server, p, server, dst, core.FileTransferOptions{}, nil); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "path": dst})
}

// duplicateName derives a "<name> copy.<ext>" sibling path. It inserts " copy"
// before the final extension (preserving compound suffixes like ".tar.gz" only
// for the single trailing extension, matching Finder/Explorer behaviour).
func duplicateName(p string) string {
	dir := path.Dir(p)
	base := path.Base(p)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return path.Join(dir, stem+" copy"+ext)
}

// freeDuplicateName returns the first "<name> copy[ N].<ext>" sibling that does
// not already exist (per `exists`), so Duplicate never clobbers a sibling.
func freeDuplicateName(p string, exists func(string) bool) string {
	dir, base := path.Dir(p), path.Base(p)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; ; i++ {
		suffix := " copy"
		if i > 1 {
			suffix = fmt.Sprintf(" copy %d", i)
		}
		cand := path.Join(dir, stem+suffix+ext)
		if !exists(cand) {
			return cand
		}
	}
}

// ---- progress hub ----

type progressHub struct {
	mu    sync.Mutex
	items map[string]*liveTransfer
}

type liveTransfer struct {
	upd  core.ProgressUpdate
	done bool
	err  string
}

// progressSnapshot is the SSE payload.
type progressSnapshot struct {
	BytesDone     int64   `json:"bytes_done"`
	TotalBytes    int64   `json:"total_bytes"`
	RatePerSec    float64 `json:"rate_per_sec"`
	ActiveStreams int     `json:"active_streams"`
	Percent       int     `json:"percent"`
	Done          bool    `json:"done"`
	Error         string  `json:"error,omitempty"`
}

func newProgressHub() *progressHub {
	return &progressHub{items: make(map[string]*liveTransfer)}
}

func (h *progressHub) start() string {
	id, _ := randomToken()
	h.mu.Lock()
	h.items[id] = &liveTransfer{}
	h.mu.Unlock()
	return id
}

func (h *progressHub) update(id string, u core.ProgressUpdate) {
	h.mu.Lock()
	if t, ok := h.items[id]; ok {
		t.upd = u
	}
	h.mu.Unlock()
}

func (h *progressHub) finish(id string, err error) {
	h.mu.Lock()
	if t, ok := h.items[id]; ok {
		t.done = true
		if err != nil {
			t.err = err.Error()
		} else if t.upd.TotalBytes > 0 {
			t.upd.BytesDone = t.upd.TotalBytes
		}
	}
	h.mu.Unlock()
	// Drop the record a little later so a slow SSE poller can still read the
	// terminal state.
	go func() {
		time.Sleep(30 * time.Second)
		h.mu.Lock()
		delete(h.items, id)
		h.mu.Unlock()
	}()
}

func (h *progressHub) snapshot(id string) (progressSnapshot, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.items[id]
	if !ok {
		return progressSnapshot{}, false
	}
	pct := 0
	if t.upd.TotalBytes > 0 {
		pct = int(t.upd.BytesDone * 100 / t.upd.TotalBytes)
	}
	return progressSnapshot{
		BytesDone:     t.upd.BytesDone,
		TotalBytes:    t.upd.TotalBytes,
		RatePerSec:    t.upd.RatePerSec,
		ActiveStreams: t.upd.ActiveStreams,
		Percent:       pct,
		Done:          t.done,
		Error:         t.err,
	}, true
}

// ---- helpers ----

func randomToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func ensureLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid web ui address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("web ui address %q is not loopback; refusing to expose file transfer to the network", addr)
	}
	return nil
}

// symlinkClobberError turns the ELOOP that an O_NOFOLLOW open returns for a
// symlinked target into a clear, actionable message, and otherwise lstats the
// path to catch platforms that report a symlink differently. Non-symlink errors
// pass through unchanged.
func symlinkClobberError(path string, err error) error {
	if err == nil {
		return nil
	}
	symlinked := errors.Is(err, syscall.ELOOP)
	if !symlinked {
		if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			symlinked = true
		}
	}
	if symlinked {
		return fmt.Errorf("refusing to write through symlink %q", path)
	}
	return err
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// originLoopback reports whether a state-changing request carries a trustworthy
// same-origin signal. It FAILS CLOSED: a request must present either a loopback
// Origin or a same-origin/none Sec-Fetch-Site. Browsers always send at least one
// of these on a fetch/form POST, so the absence of both indicates a non-browser
// or forged request and is rejected.
func originLoopback(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// No Origin: trust only an explicit same-origin Fetch Metadata signal.
		// A missing Sec-Fetch-Site (older/non-browser client) is rejected so the
		// check can't be bypassed by simply omitting both headers.
		site := r.Header.Get("Sec-Fetch-Site")
		return site == "same-origin" || site == "none"
	}
	host := origin
	if _, after, found := strings.Cut(origin, "://"); found {
		host = after
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
