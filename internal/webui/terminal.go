// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

// The Terminal view gives the browser an interactive shell on a managed
// server. It never opens a shell itself: each session is `fleet ssh <server>`
// started by a launcher the CLI installs, under a pseudo-terminal on the
// controller, with the token this UI was launched with. So RBAC, the rule that
// refuses ssh while a command policy is configured, host-key pinning and the
// audit log all behave exactly as when an operator types `fleet ssh` in a
// terminal. The browser only sends keystrokes and window sizes, and receives
// the terminal's output over SSE.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TerminalLauncher builds the `fleet ssh` child for one terminal session. It
// must use exec.CommandContext(ctx, …) so the session can be ended.
type TerminalLauncher func(ctx context.Context, server string) (*exec.Cmd, error)

// SetTerminal enables the Terminal view.
func (s *Server) SetTerminal(launch TerminalLauncher) {
	if launch == nil {
		s.terminal = nil
		return
	}
	s.terminal = &termHub{launch: launch, sessions: map[string]*termSession{}}
}

const (
	maxTermSessions    = 8
	maxTermScrollback  = 1 << 20 // bytes of output kept for reconnecting streams
	maxTermInputBytes  = 64 << 10
	termDefaultCols    = 120
	termDefaultRows    = 32
	termMaxDimension   = 1000
	maxKeptTermExited  = 20
	termOutputChunkMax = 32 << 10
)

// termPTY is the controller-side end of a session's pseudo-terminal.
type termPTY interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
}

type termChunk struct {
	seq  uint64
	data []byte
}

type termSession struct {
	id       string
	server   string
	started  time.Time
	ended    time.Time
	state    string // running, exited
	exitCode int
	errText  string
	cols     int
	rows     int

	cmd    *exec.Cmd
	pty    termPTY
	cancel context.CancelFunc

	chunks  []termChunk
	bytes   int
	nextSeq uint64
	changed chan struct{}
}

type termHub struct {
	launch   TerminalLauncher
	mu       sync.Mutex
	sessions map[string]*termSession
	order    []string
	wg       sync.WaitGroup
}

type termSummary struct {
	ID       string    `json:"id"`
	Server   string    `json:"server"`
	State    string    `json:"state"`
	ExitCode int       `json:"exit_code"`
	Error    string    `json:"error,omitempty"`
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended,omitzero"`
	Cols     int       `json:"cols"`
	Rows     int       `json:"rows"`
}

func (t *termSession) summaryLocked() termSummary {
	return termSummary{ID: t.id, Server: t.server, State: t.state, ExitCode: t.exitCode, Error: t.errText,
		Started: t.started, Ended: t.ended, Cols: t.cols, Rows: t.rows}
}

func (t *termSession) notifyLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}

func (h *termHub) appendOutput(t *termSession, p []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t.nextSeq++
	t.chunks = append(t.chunks, termChunk{seq: t.nextSeq, data: append([]byte(nil), p...)})
	t.bytes += len(p)
	for t.bytes > maxTermScrollback && len(t.chunks) > 1 {
		t.bytes -= len(t.chunks[0].data)
		t.chunks[0] = termChunk{}
		t.chunks = t.chunks[1:]
	}
	t.notifyLocked()
}

func (h *termHub) get(id string) (*termSession, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.sessions[id]
	return t, ok
}

// evictLocked forgets the oldest exited sessions beyond maxKeptTermExited.
func (h *termHub) evictLocked() {
	exited := 0
	for _, id := range h.order {
		if h.sessions[id].state != "running" {
			exited++
		}
	}
	kept := h.order[:0]
	for _, id := range h.order {
		if exited > maxKeptTermExited && h.sessions[id].state != "running" {
			delete(h.sessions, id)
			exited--
			continue
		}
		kept = append(kept, id)
	}
	h.order = kept
}

// closeAll ends every session (UI shutdown) and waits up to grace.
func (h *termHub) closeAll(grace time.Duration) {
	h.mu.Lock()
	for _, t := range h.sessions {
		if t.state == "running" && t.cancel != nil {
			t.cancel()
		}
	}
	h.mu.Unlock()
	done := make(chan struct{})
	go func() { h.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
	}
}

func termDimension(v string, def int) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 2 {
		return def
	}
	if n > termMaxDimension {
		return termMaxDimension
	}
	return n
}

// validTerminalServer checks the requested server against the controller's
// server list, so the browser can only name a real server (never a flag or an
// arbitrary string), and rejects modes `fleet ssh` cannot serve.
func (s *Server) validTerminalServer(name string) (string, int, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", http.StatusBadRequest, errors.New("choose a server")
	}
	if s.app == nil {
		return "", http.StatusBadGateway, errors.New("no controller")
	}
	servers, err := s.app.ListServers()
	if err != nil {
		return "", http.StatusBadGateway, err
	}
	for _, srv := range servers {
		if srv.Name != name {
			continue
		}
		if string(srv.Mode) != "direct" {
			return "", http.StatusBadRequest, fmt.Errorf("%s is a %s-mode server; the terminal needs a direct-mode server for now (use Run a command instead)", name, srv.Mode)
		}
		return srv.Name, 0, nil
	}
	return "", http.StatusNotFound, fmt.Errorf("unknown server %q", name)
}

func (s *Server) terminalDisabled(w http.ResponseWriter) bool {
	if s.terminal != nil {
		return false
	}
	writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "the terminal is not enabled in this UI"})
	return true
}

// handleTermSessions lists sessions (GET).
func (s *Server) handleTermSessions(w http.ResponseWriter, r *http.Request) {
	if s.terminalDisabled(w) {
		return
	}
	h := s.terminal
	h.mu.Lock()
	out := make([]termSummary, 0, len(h.order))
	for _, id := range h.order {
		out = append(out, h.sessions[id].summaryLocked())
	}
	h.mu.Unlock()
	writeJSON(w, map[string]any{"sessions": out, "supported": termSupported})
}

// handleTermOpen starts a session (POST ?server=&cols=&rows=).
func (s *Server) handleTermOpen(w http.ResponseWriter, r *http.Request) {
	if s.terminalDisabled(w) {
		return
	}
	if !termSupported {
		writeJSONStatus(w, http.StatusNotImplemented, map[string]string{"error": "the browser terminal needs a Linux or macOS controller; use fleet ssh in a terminal"})
		return
	}
	server, status, err := s.validTerminalServer(r.URL.Query().Get("server"))
	if err != nil {
		writeJSONStatus(w, status, map[string]string{"error": err.Error()})
		return
	}
	cols := termDimension(r.URL.Query().Get("cols"), termDefaultCols)
	rows := termDimension(r.URL.Query().Get("rows"), termDefaultRows)

	h := s.terminal
	h.mu.Lock()
	running := 0
	for _, t := range h.sessions {
		if t.state == "running" {
			running++
		}
	}
	if running >= maxTermSessions {
		h.mu.Unlock()
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": fmt.Sprintf("%d terminals are already open; close one first", maxTermSessions)})
		return
	}
	h.mu.Unlock()

	id, err := randomToken()
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	id = id[:16]
	base := s.baseCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base) // #nosec G118 -- session lifetime outlives the request by design
	cmd, err := h.launch(ctx, server)
	if err != nil {
		cancel()
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 2 * time.Second
	pty, err := startTermPTY(cmd, cols, rows)
	if err != nil {
		cancel()
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "could not start the terminal: " + err.Error()})
		return
	}
	t := &termSession{
		id: id, server: server, started: time.Now().UTC(), state: "running", cols: cols, rows: rows,
		cmd: cmd, pty: pty, cancel: cancel, changed: make(chan struct{}),
	}
	h.mu.Lock()
	h.sessions[id] = t
	h.order = append(h.order, id)
	h.evictLocked()
	h.wg.Add(1)
	sum := t.summaryLocked()
	h.mu.Unlock()
	s.audit("webui.terminal", server, "session="+id)

	go func() {
		defer h.wg.Done()
		buf := make([]byte, termOutputChunkMax)
		for {
			n, rerr := pty.Read(buf)
			if n > 0 {
				h.appendOutput(t, buf[:n])
			}
			if rerr != nil {
				break
			}
		}
		werr := cmd.Wait()
		_ = pty.Close()
		h.mu.Lock()
		defer h.mu.Unlock()
		t.state = "exited"
		t.ended = time.Now().UTC()
		var exitErr *exec.ExitError
		switch {
		case werr == nil:
		case errors.As(werr, &exitErr):
			t.exitCode = exitErr.ExitCode()
		default:
			t.exitCode = -1
			t.errText = werr.Error()
		}
		cancel()
		t.notifyLocked()
	}()
	writeJSON(w, map[string]any{"session": sum})
}

// handleTermInput writes keystrokes (POST body) to a session.
func (s *Server) handleTermInput(w http.ResponseWriter, r *http.Request) {
	if s.terminalDisabled(w) {
		return
	}
	t, ok := s.terminal.get(r.URL.Query().Get("id"))
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "unknown terminal"})
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTermInputBytes))
	if err != nil {
		writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "input too large"})
		return
	}
	s.terminal.mu.Lock()
	running := t.state == "running"
	s.terminal.mu.Unlock()
	if !running {
		writeJSONStatus(w, http.StatusConflict, map[string]string{"error": "this terminal has ended"})
		return
	}
	if len(data) > 0 {
		if _, err := t.pty.Write(data); err != nil && !errors.Is(err, os.ErrClosed) {
			writeJSONStatus(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTermResize changes a session's window size (POST ?id=&cols=&rows=).
func (s *Server) handleTermResize(w http.ResponseWriter, r *http.Request) {
	if s.terminalDisabled(w) {
		return
	}
	t, ok := s.terminal.get(r.URL.Query().Get("id"))
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "unknown terminal"})
		return
	}
	cols := termDimension(r.URL.Query().Get("cols"), 0)
	rows := termDimension(r.URL.Query().Get("rows"), 0)
	if cols == 0 || rows == 0 {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "cols and rows are required"})
		return
	}
	s.terminal.mu.Lock()
	running := t.state == "running"
	if running {
		t.cols, t.rows = cols, rows
	}
	s.terminal.mu.Unlock()
	if running {
		_ = t.pty.Resize(cols, rows)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTermClose ends a session (POST ?id=).
func (s *Server) handleTermClose(w http.ResponseWriter, r *http.Request) {
	if s.terminalDisabled(w) {
		return
	}
	h := s.terminal
	id := r.URL.Query().Get("id")
	t, ok := h.get(id)
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "unknown terminal"})
		return
	}
	h.mu.Lock()
	if t.state == "running" {
		t.cancel()
	} else {
		// Closing an ended session forgets it.
		delete(h.sessions, id)
		kept := h.order[:0]
		for _, o := range h.order {
			if o != id {
				kept = append(kept, o)
			}
		}
		h.order = kept
	}
	h.mu.Unlock()
	writeJSON(w, map[string]string{"status": "closed"})
}

// handleTermStream streams a session's output as SSE "data" events (base64,
// since a read can split a multi-byte character or an escape sequence) and a
// final "exit" event.
func (s *Server) handleTermStream(w http.ResponseWriter, r *http.Request) {
	if s.terminalDisabled(w) {
		return
	}
	h := s.terminal
	t, ok := h.get(r.URL.Query().Get("id"))
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "unknown terminal"})
		return
	}
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > after {
			after = n
		}
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	// A terminal can stay open for a long time; the stream lives as long as
	// the session, the client or the UI.
	ctx := r.Context()
	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()
	for {
		h.mu.Lock()
		var pending []termChunk
		for _, c := range t.chunks {
			if c.seq > after {
				pending = append(pending, c)
			}
		}
		sum := t.summaryLocked()
		changed := t.changed
		h.mu.Unlock()

		if len(pending) > 0 {
			// Coalesce into one event per flush: fewer, larger frames.
			var b strings.Builder
			last := after
			for _, c := range pending {
				b.Write(c.data)
				last = c.seq
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: data\ndata: %s\n\n", last, base64.StdEncoding.EncodeToString([]byte(b.String()))); err != nil {
				return
			}
			after = last
		}
		if sum.State != "running" {
			_ = writeSSE(w, "exit", "", sum)
			_ = rc.Flush()
			return
		}
		if rc.Flush() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
			// Let a burst of output gather before the next frame.
			select {
			case <-ctx.Done():
				return
			case <-time.After(8 * time.Millisecond):
			}
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}
