// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// JobStatus is the lifecycle state of a background job.
type JobStatus string

const (
	// JobRunning means the job was launched and no FLEETEXIT marker has been
	// observed in its logfile yet.
	JobRunning JobStatus = "running"
	// JobDone means the FLEETEXIT marker was observed; ExitCode is then valid.
	JobDone JobStatus = "done"
)

// jobExitMarker is the fixed sentinel prefix the launched shell appends after
// the command finishes. The launcher writes it as
// "FLEETEXIT:<nonce>:<code>", where <nonce> is a per-job unguessable token (see
// JobRecord.Nonce). The trailing exit code lets `job status` detect completion
// by tailing the logfile, with no agent/proto change required.
//
// SECURITY: completion is detected only by matching the per-job nonce
// (jobExitMarker + nonce + ":"), so the user command's own stdout/stderr cannot
// forge a "FLEETEXIT:0" line and spoof completion — it does not know the nonce.
const jobExitMarker = "FLEETEXIT:"

// jobNonceBytes is the number of random bytes behind each job's completion
// nonce. 16 bytes (128 bits) hex-encodes to a 32-char charset-safe token.
const jobNonceBytes = 16

// newJobNonce returns a fresh, charset-safe (lowercase hex) completion nonce.
// The token is unguessable so only the launcher — which embeds it literally —
// can write a marker the status parser will accept.
func newJobNonce() (string, error) {
	b := make([]byte, jobNonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate job nonce: %w", err)
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0x0f]
	}
	return string(out), nil
}

// JobRecord is a single background job tracked in the on-disk store.
type JobRecord struct {
	ID      int    `json:"id"`
	Name    string `json:"name,omitempty"` // optional operator-supplied label
	Server  string `json:"server"`
	Command string `json:"command"`
	Logfile string `json:"logfile"`
	// Nonce is the per-job unguessable token embedded in the launcher's
	// completion marker ("FLEETEXIT:<nonce>:<code>"). Status detection accepts a
	// marker only if it carries this exact nonce, so the user command cannot
	// forge completion. It is persisted in jobs.json for the life of the job.
	Nonce    string    `json:"nonce"`
	Status   JobStatus `json:"status"`
	ExitCode int       `json:"exit_code"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
}

// ExecFunc runs a command on a named server and returns its combined stdout.
// It mirrors the shape of App.ExecCommand reduced to what the job engine needs,
// so the pure logic can be exercised in tests with a fake.
type ExecFunc func(server, command string) (stdout string, exitCode int, err error)

// JobStore is a small standalone JSON-backed store for background jobs,
// persisted as a single document at <configDir>/jobs.json (0600). It follows
// the same read/modify/write pattern as TagStore and is kept off *App so it
// does not require touching app.go.
type JobStore struct {
	path string
	mu   sync.Mutex
}

// jobsDocument is the on-disk JSON shape.
type jobsDocument struct {
	Counter int         `json:"counter"`
	Jobs    []JobRecord `json:"jobs"`
}

// NewJobStore opens (without reading) a job store rooted at configDir. If
// configDir is empty the default config dir is used.
func NewJobStore(configDir string) *JobStore {
	if configDir == "" {
		configDir = DefaultConfigDir("")
	}
	return &JobStore{path: JobsPath(configDir)}
}

// JobsPath returns the on-disk location of the jobs document for a config dir.
func JobsPath(configDir string) string {
	return filepath.Join(configDir, "jobs.json")
}

func (s *JobStore) read() (jobsDocument, error) {
	doc := jobsDocument{}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return doc, fmt.Errorf("read jobs: %w", err)
	}
	if len(data) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("decode jobs: %w", err)
	}
	return doc, nil
}

func (s *JobStore) write(doc jobsDocument) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode jobs: %w", err)
	}
	// Atomic write: temp file in the same dir -> chmod 0600 -> rename.
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".jobs-*.json")
	if err != nil {
		return fmt.Errorf("write jobs: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write jobs: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write jobs: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write jobs: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("write jobs: %w", err)
	}
	return nil
}

// nextID reserves and persists the next monotonically-increasing job id.
// Caller must hold s.mu.
func (s *JobStore) reserve(doc *jobsDocument) int {
	doc.Counter++
	return doc.Counter
}

// JobLogfile returns the remote logfile path for a job. The per-job nonce is
// included so the path is UNPREDICTABLE (another local user on the remote host
// can't guess it from the sequential id), and buildJobLaunch creates it 0600
// (umask 077) so it isn't world-readable.
func JobLogfile(id int, nonce string) string {
	return "/var/tmp/fleet-job-" + strconv.Itoa(id) + "-" + nonce + ".log"
}

// buildJobLaunch builds the detached launcher command. The command is wrapped
// so its combined output is redirected to the logfile and a
// FLEETEXIT:<nonce>:<code> marker is appended once it finishes; setsid +
// </dev/null fully detaches it so the agent RPC returns immediately.
//
// The user command is run via a nested `sh -c <quoted cmd>` rather than spliced
// directly into the brace group, so that a trailing ';' (e.g. "sleep 60;") or
// any other shell metacharacter cannot break the surrounding script. The
// FLEETEXIT:<nonce>:<code> marker is then echoed as a separate statement using
// the command's real exit code ($?), so completion is always recorded.
//
// SECURITY: only this launcher knows the per-job nonce, so the user command's
// own output cannot forge the completion marker. The nonce is a fixed-charset
// hex token (see newJobNonce), so it carries no shell metacharacters; it is
// embedded inside the already single-quoted inner script regardless.
//
// Every other interpolated value (the user command and the logfile path) is
// single-quoted for safe embedding in the remote /bin/sh command.
func buildJobLaunch(command, logfile, nonce string) string {
	inner := "umask 077; { sh -c " + shellQuote(command) + "; echo " + jobExitMarker + nonce + ":$?; } > " + shellQuote(logfile) + " 2>&1"
	return "setsid sh -c " + shellQuote(inner) + " </dev/null >/dev/null 2>&1 &"
}

// Start records a new job and launches it on the server via exec. The returned
// record is the persisted job (status running). The exec call is expected to
// return promptly because the remote launcher detaches the work.
func (s *JobStore) Start(exec ExecFunc, server, command, name string) (JobRecord, error) {
	if exec == nil {
		return JobRecord{}, fmt.Errorf("exec function is required")
	}
	if strings.TrimSpace(server) == "" {
		return JobRecord{}, fmt.Errorf("server name is required")
	}
	if strings.TrimSpace(command) == "" {
		return JobRecord{}, fmt.Errorf("command is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return JobRecord{}, err
	}
	nonce, err := newJobNonce()
	if err != nil {
		return JobRecord{}, err
	}
	id := s.reserve(&doc)
	logfile := JobLogfile(id, nonce)
	rec := JobRecord{
		ID:      id,
		Name:    strings.TrimSpace(name),
		Server:  server,
		Command: command,
		Logfile: logfile,
		Nonce:   nonce,
		Status:  JobRunning,
		Started: time.Now().UTC(),
	}

	if _, _, err := exec(server, buildJobLaunch(command, logfile, nonce)); err != nil {
		// Do not persist a job we failed to launch; roll the counter back so
		// the id is reused next time.
		doc.Counter--
		_ = s.write(doc)
		return JobRecord{}, fmt.Errorf("launch job on %s: %w", server, err)
	}

	doc.Jobs = append(doc.Jobs, rec)
	if err := s.write(doc); err != nil {
		return JobRecord{}, err
	}
	return rec, nil
}

// Get returns a copy of the stored job record for an id.
func (s *JobStore) Get(id int) (JobRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return JobRecord{}, err
	}
	for _, j := range doc.Jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return JobRecord{}, fmt.Errorf("job %d not found", id)
}

// List returns all stored jobs sorted by id (descending: newest first).
func (s *JobStore) List() ([]JobRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]JobRecord, len(doc.Jobs))
	copy(out, doc.Jobs)
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// markerLineIndex locates the launcher's completion marker for a job. The
// marker is "FLEETEXIT:<nonce>:" and is written by the launcher as the final
// line of the logfile. We accept it only when it carries the job's exact nonce
// AND begins a line (start of content or just after a newline), so the user
// command cannot forge it: it neither knows the nonce nor can prepend its own
// "FLEETEXIT:<nonce>:" at line start without it. Returns the byte index of the
// marker's start, or -1 if no authentic marker is present.
//
// We scan from the end (LastIndex) because the marker is the last line; an
// empty nonce (legacy/corrupt record) never matches, failing closed.
func markerLineIndex(logContent, nonce string) int {
	if nonce == "" {
		return -1
	}
	want := jobExitMarker + nonce + ":"
	for off := len(logContent); off >= 0; {
		idx := strings.LastIndex(logContent[:off], want)
		if idx < 0 {
			return -1
		}
		// The marker must start a line: at offset 0 or right after a newline.
		if idx == 0 || logContent[idx-1] == '\n' {
			return idx
		}
		// A non-anchored occurrence (e.g. user output "xFLEETEXIT:<nonce>:");
		// keep scanning earlier in the buffer.
		off = idx
	}
	return -1
}

// parseExit scans logfile content for the job's authentic
// FLEETEXIT:<nonce>:<code> marker. It returns (exitCode, true) when the job has
// finished and (0, false) otherwise. The nonce is the per-job token stored in
// the job record; without a matching, line-anchored marker the job is treated
// as still running, so spoofed "FLEETEXIT:0" output cannot mark it done.
func parseExit(logContent, nonce string) (int, bool) {
	idx := markerLineIndex(logContent, nonce)
	if idx < 0 {
		return 0, false
	}
	rest := logContent[idx+len(jobExitMarker)+len(nonce)+1:]
	// The code may be followed by a newline or trailing data; take leading digits.
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	code, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, false
	}
	return code, true
}

// stripExitMarker removes the authentic FLEETEXIT:<nonce>:<code> trailer (and
// any preceding newline) so log output shown to the user does not include the
// sentinel. A spoofed/non-authentic "FLEETEXIT:..." line in the user's own
// output is left untouched, since it is genuine job output.
func stripExitMarker(logContent, nonce string) string {
	idx := markerLineIndex(logContent, nonce)
	if idx < 0 {
		return logContent
	}
	out := logContent[:idx]
	return strings.TrimRight(out, "\n")
}

// Tail reads the job's logfile from the server and returns its content with the
// FLEETEXIT marker stripped. If the job has finished, the stored record is
// updated (status done + exit code) and the refreshed record is returned.
func (s *JobStore) Tail(exec ExecFunc, id int) (rec JobRecord, output string, err error) {
	if exec == nil {
		return JobRecord{}, "", fmt.Errorf("exec function is required")
	}
	rec, err = s.Get(id)
	if err != nil {
		return JobRecord{}, "", err
	}
	// cat the logfile; an empty/missing file just yields empty output.
	raw, _, execErr := exec(rec.Server, "cat "+shellQuote(rec.Logfile)+" 2>/dev/null")
	if execErr != nil {
		return rec, "", fmt.Errorf("read job %d log: %w", id, execErr)
	}
	output = stripExitMarker(raw, rec.Nonce)
	if rec.Status != JobDone {
		if code, done := parseExit(raw, rec.Nonce); done {
			rec.Status = JobDone
			rec.ExitCode = code
			rec.Finished = time.Now().UTC()
			if uerr := s.update(rec); uerr != nil {
				return rec, output, uerr
			}
			// On the transition to finished-with-a-nonzero-exit, fire the
			// "job-failed" notification. Best-effort: it must never affect the
			// returned record or fail the Tail.
			if code != 0 {
				s.fireJobFailed(rec)
			}
		}
	}
	return rec, output, nil
}

// fireJobFailed sends a best-effort "job-failed" notification for a finished job
// whose exit code was non-zero. The NotifyStore is loaded from the same config
// dir as the job store (jobs.json's parent), so it requires no extra wiring. Any
// failure — including a panic — is swallowed so it can never break Tail/Wait.
func (s *JobStore) fireJobFailed(rec JobRecord) {
	defer func() { _ = recover() }()
	configDir := filepath.Dir(s.path)
	msg := fmt.Sprintf("job %d on %s failed (exit %d): %s", rec.ID, rec.Server, rec.ExitCode, rec.Command)
	_ = NewNotifyStore(configDir).Fire(NotifyEventJobFailed, msg)
}

// update persists a single changed job record in place.
func (s *JobStore) update(rec JobRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return err
	}
	found := false
	for i := range doc.Jobs {
		if doc.Jobs[i].ID == rec.ID {
			doc.Jobs[i] = rec
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("job %d not found", rec.ID)
	}
	return s.write(doc)
}

// Wait polls the job until it finishes (FLEETEXIT observed) or the timeout
// elapses. A non-positive timeout waits indefinitely. poll controls the gap
// between checks; sleep abstracts time.Sleep so tests can run without delay.
func (s *JobStore) Wait(exec ExecFunc, id int, timeout, poll time.Duration, sleep func(time.Duration)) (JobRecord, error) {
	if poll <= 0 {
		poll = time.Second
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		rec, _, err := s.Tail(exec, id)
		if err != nil {
			return JobRecord{}, err
		}
		if rec.Status == JobDone {
			return rec, nil
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return rec, fmt.Errorf("timed out waiting for job %d after %s", id, timeout)
		}
		sleep(poll)
	}
}
