// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

// Package webui serves a localhost-only browser file manager for Cenvero Fleet.
// It binds 127.0.0.1 only, requires a per-process random token on every request,
// and rides the same authenticated SSH transport as the rest of the controller —
// it never opens a remote-reachable or unauthenticated surface.
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cenvero/fleet/internal/core"
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
	mux.HandleFunc("/api/list", s.guard(s.handleList))
	mux.HandleFunc("/api/upload", s.guard(s.handleUpload))
	mux.HandleFunc("/api/download", s.guard(s.handleDownload))
	mux.HandleFunc("/api/progress", s.guard(s.handleProgress))
	// Mutating endpoints are POST-only so the guard's Origin/CSRF check applies
	// (a state-changing GET would slip past it).
	mux.HandleFunc("/api/mkdir", s.guard(postOnly(s.handleMkdir)))
	mux.HandleFunc("/api/rm", s.guard(postOnly(s.handleRemove)))
	mux.HandleFunc("/api/mv", s.guard(postOnly(s.handleMove)))
	mux.HandleFunc("/api/copy", s.guard(postOnly(s.handleCopy)))
	mux.HandleFunc("/api/move", s.guard(postOnly(s.handleMoveTransfer)))
	return securityHeaders(mux)
}

// maxWebUploadBytes caps a single browser upload spooled to the controller's
// temp dir, so a runaway request can't fill the disk. Large transfers should
// use the `fleet file upload` CLI instead.
const maxWebUploadBytes = 64 << 30 // 64 GiB

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

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	server := r.URL.Query().Get("server")
	dir := r.URL.Query().Get("path")
	showHidden := r.URL.Query().Get("hidden") == "1" || r.URL.Query().Get("hidden") == "true"
	result, err := s.app.ListRemoteDirHidden(server, dir, showHidden)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, result)
}

// handleTransfer drives a server-to-server copy or move, tracked by the progress
// hub like an upload. move=true deletes the source after a successful copy.
func (s *Server) handleTransfer(w http.ResponseWriter, r *http.Request, move bool) {
	q := r.URL.Query()
	srcServer, srcPath := q.Get("srcServer"), q.Get("srcPath")
	dstServer, dstPath := q.Get("dstServer"), q.Get("dstPath")
	recursive := q.Get("recursive") == "1" || q.Get("recursive") == "true"
	if srcServer == "" || srcPath == "" || dstServer == "" || dstPath == "" {
		http.Error(w, "srcServer, srcPath, dstServer, dstPath are required", http.StatusBadRequest)
		return
	}
	id := s.hub.start()
	go func() {
		prog := func(u core.ProgressUpdate) { s.hub.update(id, u) }
		var err error
		switch {
		case move && recursive:
			_, err = s.app.MoveDir(srcServer, srcPath, dstServer, dstPath, core.FileTransferOptions{}, prog)
		case move:
			err = s.app.MoveFile(srcServer, srcPath, dstServer, dstPath, core.FileTransferOptions{}, prog)
		case recursive:
			_, err = s.app.CopyDir(srcServer, srcPath, dstServer, dstPath, core.FileTransferOptions{}, prog)
		default:
			_, err = s.app.CopyFile(srcServer, srcPath, dstServer, dstPath, core.FileTransferOptions{}, prog)
		}
		s.hub.finish(id, err)
	}()
	writeJSON(w, map[string]string{"id": id})
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
	if server == "" || name == "" || name == "." || name == "/" {
		http.Error(w, "server and name are required", http.StatusBadRequest)
		return
	}
	// The agent re-validates every path, but reject a non-absolute target here
	// too so a relative `dir` can't produce a surprising join.
	if !path.IsAbs(dir) {
		http.Error(w, "dir must be an absolute path", http.StatusBadRequest)
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
	if server == "" || remotePath == "" {
		http.Error(w, "server and path are required", http.StatusBadRequest)
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

	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
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
	if err := s.app.RemoteMkdir(server, p); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	server, p := r.URL.Query().Get("server"), r.URL.Query().Get("path")
	recursive := r.URL.Query().Get("recursive") == "true"
	if err := s.app.RemoteDelete(server, p, recursive); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	server := r.URL.Query().Get("server")
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if err := s.app.RemoteRename(server, from, to); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
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

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func originLoopback(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Some same-origin POSTs omit Origin; allow when Sec-Fetch-Site is same-origin or absent.
		site := r.Header.Get("Sec-Fetch-Site")
		return site == "" || site == "same-origin" || site == "none"
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
