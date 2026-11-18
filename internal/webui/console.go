// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

// The Control view lets the browser run any fleet CLI command. It does not
// reimplement any command: every run is a fresh `fleet` child process, started
// by a launcher the CLI installs, with the controller's config dir and the
// RBAC token this UI was launched with. So RBAC, cmd-policy, redaction, audit
// and every confirmation still happen exactly as in a terminal. Output streams
// back over SSE.
//
// The browser can choose the command and its arguments, but not the process
// environment, the config dir or the token. Interactive commands (terminal UIs,
// $EDITOR, shells) are listed as unavailable and refused. Stdin is either the
// text the operator typed or empty, so a prompt never waits for input that
// cannot arrive. Commands that ask for confirmation fail closed without a
// terminal, and the form passes --yes / --confirm only when the operator ticks
// them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// CommandArg is one positional argument of a command, parsed from its usage line.
type CommandArg struct {
	Name     string   `json:"name"`
	Required bool     `json:"required,omitempty"`
	Variadic bool     `json:"variadic,omitempty"`
	Suggest  []string `json:"suggest,omitempty"`
	// Server marks an argument that names a managed server, so the form can
	// offer the server list.
	Server bool `json:"server,omitempty"`
}

// CommandFlag is one flag of a command.
type CommandFlag struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Default   string `json:"default,omitempty"`
	Usage     string `json:"usage,omitempty"`
	Required  bool   `json:"required,omitempty"`
	// Sensitive flags (passwords, DSNs, secret values) are rendered as password
	// inputs and masked in the run history.
	Sensitive bool `json:"sensitive,omitempty"`
}

// CommandSpec describes one runnable fleet command for the Control view.
type CommandSpec struct {
	Path    []string      `json:"path"`
	Use     string        `json:"use"`
	Short   string        `json:"short,omitempty"`
	Long    string        `json:"long,omitempty"`
	Example string        `json:"example,omitempty"`
	Args    []CommandArg  `json:"args"`
	Flags   []CommandFlag `json:"flags"`
	// Destructive commands get a confirmation step in the browser.
	Destructive bool `json:"destructive,omitempty"`
	// Stdin marks commands that read their input from stdin (secret values,
	// automation scripts), so the form offers a text box for it.
	Stdin bool `json:"stdin,omitempty"`
	// Unavailable, when set, says why the web UI cannot run this command
	// (interactive terminal UIs, $EDITOR, …). It is still listed so the
	// operator learns where to go instead.
	Unavailable string `json:"unavailable,omitempty"`
}

// CommandLauncher builds the child process for one run. args is the full
// argument vector after the binary name, starting with the command path. The
// launcher must build the command with exec.CommandContext(ctx, …), so the
// Control view can cancel it, and may refuse an invocation by returning an
// error.
type CommandLauncher func(ctx context.Context, args []string) (*exec.Cmd, error)

// SetConsole enables the Control view: catalog is the list of commands the
// browser may pick from, launch starts one.
func (s *Server) SetConsole(catalog []CommandSpec, launch CommandLauncher) {
	if launch == nil {
		s.console = nil
		return
	}
	byPath := make(map[string]CommandSpec, len(catalog))
	sorted := make([]CommandSpec, 0, len(catalog))
	for _, spec := range catalog {
		if len(spec.Path) == 0 {
			continue
		}
		key := strings.Join(spec.Path, " ")
		if _, dup := byPath[key]; dup {
			continue
		}
		if spec.Args == nil {
			spec.Args = []CommandArg{}
		}
		if spec.Flags == nil {
			spec.Flags = []CommandFlag{}
		}
		byPath[key] = spec
		sorted = append(sorted, spec)
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		return strings.Join(sorted[i].Path, " ") < strings.Join(sorted[j].Path, " ")
	})
	s.console = &console{catalog: sorted, byPath: byPath, launch: launch, runs: newRunHub()}
}

type console struct {
	catalog []CommandSpec
	byPath  map[string]CommandSpec
	launch  CommandLauncher
	runs    *runHub
}

const (
	// maxConcurrentRuns caps how many commands run at once from the browser.
	maxConcurrentRuns = 8
	// maxKeptRuns is how many finished runs stay in the history.
	maxKeptRuns = 60
	// maxRunOutputBytes is how much output each run keeps; older output is
	// dropped (and the run marked truncated) once it is exceeded.
	maxRunOutputBytes = 4 << 20
	// maxRunChunks bounds the chunk list for very chatty commands.
	maxRunChunks = 20000
	// maxRunRequestBytes caps the JSON body of a run request (args + stdin).
	maxRunRequestBytes = 2 << 20
	maxRunArgs         = 256
	maxRunArgBytes     = 64 << 10
	// runCancelGrace is how long a cancelled run gets to exit after an
	// interrupt before it is killed.
	runCancelGrace = 5 * time.Second
)

// reservedRunFlags may not come from the browser: the launcher pins them.
var reservedRunFlags = []string{"--config-dir", "--token"}

// sensitiveFlagNames have values that are masked in the run history.
var sensitiveFlagNames = map[string]bool{
	"value": true, "password": true, "login-password": true, "dsn": true, "db-dsn": true,
	"secret-value": true, "passphrase": true,
}

type runChunk struct {
	Seq    uint64 `json:"seq"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

type commandRun struct {
	id        string
	path      []string
	display   []string
	started   time.Time
	ended     time.Time
	state     string // running, ok, failed, cancelled
	exitCode  int
	errText   string
	chunks    []runChunk
	bytes     int
	truncated bool
	nextSeq   uint64
	cancel    context.CancelFunc
	cancelled bool
	changed   chan struct{}
}

// runSummary is the JSON shape of a run (without its output).
type runSummary struct {
	ID        string    `json:"id"`
	Path      []string  `json:"path"`
	Command   []string  `json:"command"`
	State     string    `json:"state"`
	ExitCode  int       `json:"exit_code"`
	Error     string    `json:"error,omitempty"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended,omitzero"`
	Truncated bool      `json:"truncated,omitempty"`
	Bytes     int       `json:"bytes"`
	LastSeq   uint64    `json:"last_seq"`
}

type runHub struct {
	mu    sync.Mutex
	runs  map[string]*commandRun
	order []string
	wg    sync.WaitGroup
}

func newRunHub() *runHub { return &runHub{runs: map[string]*commandRun{}} }

func (r *commandRun) summaryLocked() runSummary {
	return runSummary{
		ID: r.id, Path: r.path, Command: r.display, State: r.state, ExitCode: r.exitCode, Error: r.errText,
		Started: r.started, Ended: r.ended, Truncated: r.truncated, Bytes: r.bytes, LastSeq: r.nextSeq,
	}
}

// notifyLocked wakes every stream waiting on this run.
func (r *commandRun) notifyLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (h *runHub) appendOutput(r *commandRun, stream string, text string) {
	if text == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r.nextSeq++
	r.chunks = append(r.chunks, runChunk{Seq: r.nextSeq, Stream: stream, Text: text})
	r.bytes += len(text)
	for (r.bytes > maxRunOutputBytes || len(r.chunks) > maxRunChunks) && len(r.chunks) > 1 {
		r.bytes -= len(r.chunks[0].Text)
		r.chunks[0] = runChunk{}
		r.chunks = r.chunks[1:]
		r.truncated = true
	}
	r.notifyLocked()
}

func (h *runHub) running() int {
	n := 0
	for _, r := range h.runs {
		if r.state == "running" {
			n++
		}
	}
	return n
}

// evictLocked drops the oldest finished runs beyond maxKeptRuns.
func (h *runHub) evictLocked() {
	finished := 0
	for _, id := range h.order {
		if h.runs[id].state != "running" {
			finished++
		}
	}
	if finished <= maxKeptRuns {
		return
	}
	kept := h.order[:0]
	for _, id := range h.order {
		r := h.runs[id]
		if finished > maxKeptRuns && r.state != "running" {
			delete(h.runs, id)
			finished--
			continue
		}
		kept = append(kept, id)
	}
	h.order = kept
}

func (h *runHub) get(id string) (*commandRun, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.runs[id]
	return r, ok
}

func (h *runHub) list() []runSummary {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]runSummary, 0, len(h.order))
	for i := len(h.order) - 1; i >= 0; i-- {
		out = append(out, h.runs[h.order[i]].summaryLocked())
	}
	return out
}

// clearFinished removes every finished run from the history.
func (h *runHub) clearFinished() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := h.order[:0]
	removed := 0
	for _, id := range h.order {
		if h.runs[id].state != "running" {
			delete(h.runs, id)
			removed++
			continue
		}
		kept = append(kept, id)
	}
	h.order = kept
	return removed
}

// cancelAll interrupts every running command (UI shutdown) and waits up to
// grace for them to exit.
func (h *runHub) cancelAll(grace time.Duration) {
	h.mu.Lock()
	for _, r := range h.runs {
		if r.state == "running" && r.cancel != nil {
			r.cancelled = true
			r.cancel()
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

// runWriter feeds one output stream of a child into its run, holding back an
// incomplete trailing UTF-8 sequence so a multi-byte character split across
// two reads is never mangled into replacement characters.
type runWriter struct {
	hub     *runHub
	run     *commandRun
	stream  string
	mu      sync.Mutex
	pending []byte
}

func (w *runWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	data := append(w.pending, p...)
	cut := len(data)
	for i := len(data) - 1; i >= 0 && i >= len(data)-utf8.UTFMax; i-- {
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:]) {
				cut = i
			}
			break
		}
	}
	w.hub.appendOutput(w.run, w.stream, strings.ToValidUTF8(string(data[:cut]), "�"))
	w.pending = append([]byte(nil), data[cut:]...)
	return len(p), nil
}

func (w *runWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) > 0 {
		w.hub.appendOutput(w.run, w.stream, strings.ToValidUTF8(string(w.pending), "�"))
		w.pending = nil
	}
}

// validateRunArgs rejects argument vectors the browser may not send.
func validateRunArgs(args []string) error {
	if len(args) > maxRunArgs {
		return fmt.Errorf("too many arguments (max %d)", maxRunArgs)
	}
	for _, a := range args {
		if len(a) > maxRunArgBytes {
			return fmt.Errorf("argument too long (max %d bytes)", maxRunArgBytes)
		}
		if strings.ContainsRune(a, 0) {
			return errors.New("arguments may not contain NUL bytes")
		}
		if !utf8.ValidString(a) {
			return errors.New("arguments must be valid UTF-8")
		}
	}
	for _, a := range args {
		if a == "--" {
			break // everything after is positional
		}
		for _, f := range reservedRunFlags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return fmt.Errorf("%s is fixed by the web UI and cannot be set here", f)
			}
		}
	}
	return nil
}

// displayArgs masks the values of sensitive flags for the run history.
func displayArgs(spec CommandSpec, args []string) []string {
	sensitive := map[string]bool{}
	short := map[string]string{}
	for _, f := range spec.Flags {
		if f.Sensitive || sensitiveFlagNames[f.Name] {
			sensitive[f.Name] = true
			if f.Shorthand != "" {
				short[f.Shorthand] = f.Name
			}
		}
	}
	out := make([]string, 0, len(args))
	maskNext := false
	for _, a := range args {
		if maskNext {
			out = append(out, "••••")
			maskNext = false
			continue
		}
		if a == "--" {
			out = append(out, a)
			continue
		}
		name := ""
		switch {
		case strings.HasPrefix(a, "--"):
			name = strings.TrimPrefix(a, "--")
		case strings.HasPrefix(a, "-") && len(a) >= 2:
			name = short[a[1:2]]
			if name != "" && len(a) > 2 {
				out = append(out, a[:2]+"••••")
				continue
			}
		}
		if name != "" {
			if key, _, hasValue := strings.Cut(name, "="); sensitive[key] {
				if hasValue {
					out = append(out, "--"+key+"=••••")
				} else {
					out = append(out, a)
					maskNext = true
				}
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

type runRequest struct {
	Path  []string `json:"path"`
	Args  []string `json:"args"`
	Stdin string   `json:"stdin"`
}

func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
	if s.console == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "the command console is not enabled in this UI"})
		return
	}
	writeJSON(w, map[string]any{"commands": s.console.catalog})
}

// handleRuns lists runs (GET) or starts one (POST).
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	if s.console == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "the command console is not enabled in this UI"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		if id := r.URL.Query().Get("id"); id != "" {
			run, ok := s.console.runs.get(id)
			if !ok {
				writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "unknown run"})
				return
			}
			s.console.runs.mu.Lock()
			sum := run.summaryLocked()
			chunks := append([]runChunk(nil), run.chunks...)
			s.console.runs.mu.Unlock()
			writeJSON(w, map[string]any{"run": sum, "output": chunks})
			return
		}
		writeJSON(w, map[string]any{"runs": s.console.runs.list()})
	case http.MethodPost:
		s.startRun(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRunRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid run request: "+err.Error(), http.StatusBadRequest)
		return
	}
	spec, ok := s.console.byPath[strings.Join(req.Path, " ")]
	if !ok {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "unknown command: fleet " + strings.Join(req.Path, " ")})
		return
	}
	if spec.Unavailable != "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "fleet " + strings.Join(spec.Path, " ") + " cannot run from the web UI: " + spec.Unavailable})
		return
	}
	if req.Args == nil {
		req.Args = []string{}
	}
	if err := validateRunArgs(req.Args); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	full := append(append([]string{}, spec.Path...), req.Args...)

	hub := s.console.runs
	hub.mu.Lock()
	if hub.running() >= maxConcurrentRuns {
		hub.mu.Unlock()
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": fmt.Sprintf("%d commands are already running; wait for one to finish or cancel it", maxConcurrentRuns)})
		return
	}
	id, err := randomToken()
	if err != nil {
		hub.mu.Unlock()
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	id = id[:16]
	base := s.baseCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base) // #nosec G118 -- run lifetime outlives the request by design
	run := &commandRun{
		id: id, path: spec.Path, display: append(append([]string{"fleet"}, spec.Path...), displayArgs(spec, req.Args)...),
		started: time.Now().UTC(), state: "running", cancel: cancel, changed: make(chan struct{}),
	}
	cmd, err := s.console.launch(ctx, full)
	if err != nil {
		hub.mu.Unlock()
		cancel()
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	stdout := &runWriter{hub: hub, run: run, stream: "out"}
	stderr := &runWriter{hub: hub, run: run, stream: "err"}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	} else {
		cmd.Stdin = nil // /dev/null: prompts see EOF and fail closed
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if runtime.GOOS == "windows" {
			return cmd.Process.Kill()
		}
		return cmd.Process.Signal(os.Interrupt)
	}
	cmd.WaitDelay = runCancelGrace
	if err := cmd.Start(); err != nil {
		hub.mu.Unlock()
		cancel()
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "could not start fleet: " + err.Error()})
		return
	}
	hub.runs[id] = run
	hub.order = append(hub.order, id)
	hub.evictLocked()
	hub.wg.Add(1)
	sum := run.summaryLocked()
	hub.mu.Unlock()

	// Record the command path only: arguments can carry secrets, and the child
	// audits its own action in full (with its own redaction) anyway.
	s.audit("webui.command", strings.Join(spec.Path, " "), "run="+id)

	go func() {
		defer hub.wg.Done()
		err := cmd.Wait()
		stdout.flush()
		stderr.flush()
		hub.mu.Lock()
		defer hub.mu.Unlock()
		run.ended = time.Now().UTC()
		run.exitCode = 0
		var exitErr *exec.ExitError
		switch {
		case run.cancelled:
			run.state = "cancelled"
			run.exitCode = -1
			if errors.As(err, &exitErr) {
				run.exitCode = exitErr.ExitCode()
			}
		case err == nil:
			run.state = "ok"
		case errors.As(err, &exitErr):
			run.state = "failed"
			run.exitCode = exitErr.ExitCode()
		default:
			run.state = "failed"
			run.exitCode = -1
			run.errText = err.Error()
		}
		cancel()
		run.notifyLocked()
	}()
	writeJSON(w, map[string]any{"run": sum})
}

func (s *Server) handleRunCancel(w http.ResponseWriter, r *http.Request) {
	if s.console == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "the command console is not enabled in this UI"})
		return
	}
	run, ok := s.console.runs.get(r.URL.Query().Get("id"))
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "unknown run"})
		return
	}
	hub := s.console.runs
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if run.state != "running" {
		writeJSONStatus(w, http.StatusConflict, map[string]string{"error": "this command has already finished"})
		return
	}
	run.cancelled = true
	run.cancel()
	writeJSON(w, map[string]string{"status": "cancelling"})
}

func (s *Server) handleRunsClear(w http.ResponseWriter, r *http.Request) {
	if s.console == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "the command console is not enabled in this UI"})
		return
	}
	writeJSON(w, map[string]int{"removed": s.console.runs.clearFinished()})
}

// handleRunStream streams one run's output as SSE: "chunk" events carry
// output after the requested sequence number, "status" events the run state.
// The stream ends after the final status of a finished run.
func (s *Server) handleRunStream(w http.ResponseWriter, r *http.Request) {
	if s.console == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "the command console is not enabled in this UI"})
		return
	}
	run, ok := s.console.runs.get(r.URL.Query().Get("id"))
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "unknown run"})
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
	ctx, cancel := context.WithTimeout(r.Context(), maxSSELifetime)
	defer cancel()

	hub := s.console.runs
	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()
	for {
		hub.mu.Lock()
		var pending []runChunk
		for _, c := range run.chunks {
			if c.Seq > after {
				pending = append(pending, c)
			}
		}
		sum := run.summaryLocked()
		changed := run.changed
		hub.mu.Unlock()

		for _, c := range pending {
			if err := writeSSE(w, "chunk", strconv.FormatUint(c.Seq, 10), c); err != nil {
				return
			}
			after = c.Seq
		}
		if err := writeSSE(w, "status", "", sum); err != nil {
			return
		}
		if rc.Flush() != nil {
			return
		}
		if sum.State != "running" {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
			// Coalesce bursts of output into fewer, larger writes.
			select {
			case <-ctx.Done():
				return
			case <-time.After(40 * time.Millisecond):
			}
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

func writeSSE(w io.Writer, event, id string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if id != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}
