// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cenvero/fleet/internal/transport"
)

// JobStatus is the lifecycle state of a background job.
type JobStatus string

const (
	// JobRunning means the job was launched and no FLEETEXIT marker has been
	// observed in its logfile yet.
	JobRunning JobStatus = "running"
	// JobDone means the FLEETEXIT marker was observed; ExitCode is then valid.
	JobDone JobStatus = "done"
	// JobLost means the job can never report completion: its server was
	// removed, its log file is gone, or no process on the server runs it any
	// more (the server rebooted, or the job was killed) while its log has no
	// marker. LostReason says which; ExitCode is not valid. See jobLostStrikes.
	JobLost JobStatus = "lost"
)

// A running job is given up on (marked JobLost) once jobLostStrikes probes in a
// row found it unable to finish — its log file missing, or (where the server
// can tell, Linux) no process left that runs it — and it is at least
// jobLostMinAge old, so a probe that races the launch can never lose a job.
// A probe that cannot tell (no /proc, an unexpected reply) changes nothing,
// and a transport error is not a strike: an unreachable server is retried,
// never given up on. A job whose server record no longer exists is lost at
// once.
const (
	jobLostStrikes = 3
	jobLostMinAge  = time.Minute
	// jobLaunchGrace: a job whose launch has not returned yet is not struck
	// within this long, so a slow launch is not taken for a lost job.
	jobLaunchGrace = 5 * time.Minute
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
	// Finished is when the job was seen to finish (done) or given up on (lost).
	Finished time.Time `json:"finished,omitempty"`
	// LostReason says why a JobLost job was given up on.
	LostReason string `json:"lost_reason,omitempty"`
	// Strikes counts the probes in a row that found the job unable to finish;
	// see jobLostStrikes. A probe that sees the job alive resets it.
	Strikes int `json:"strikes,omitempty"`
	// PID is the job's wrapper process on the server, and Launched when the
	// launch returned. Liveness is checked on that one process: the nonce is
	// not secret on the server (it is in the log file's name), so a check
	// that searched every process for it could be fooled into calling a
	// dead job alive. Records from before this have neither.
	PID      int       `json:"pid,omitempty"`
	Launched time.Time `json:"launched,omitzero"`
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

	// ServerRemoved, when set, reports whether a job's server is known to be
	// gone from the fleet (its record was removed). A probe that cannot reach
	// such a server marks the job lost instead of retrying it forever.
	// App.Jobs sets it.
	ServerRemoved func(server string) bool
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

// Jobs returns the job store for this App's config dir. Its probes treat a job
// whose server has since been removed from the fleet as lost.
func (a *App) Jobs() *JobStore {
	s := NewJobStore(a.ConfigDir)
	s.ServerRemoved = a.serverRemoved
	return s
}

// serverRemoved reports whether name is no longer one of the fleet's servers:
// only when its record file is certainly gone. Comparing names against the
// server list was wrong on a case-insensitive filesystem (macOS, Windows),
// where a job started for "Web-01" runs on the record "web-01", and any other
// doubt (an unreadable directory) must not lose a running job either.
func (a *App) serverRemoved(name string) bool {
	if validateSafeName(name) != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(a.ConfigDir, "servers", name+".toml"))
	return errors.Is(err, os.ErrNotExist)
}

// JobsPath returns the on-disk location of the jobs document for a config dir.
func JobsPath(configDir string) string {
	return filepath.Join(configDir, "jobs.json")
}

func (s *JobStore) withWriteLock(fn func() error) error {
	return withAdvisoryFileLock(s.path+".lock", fn)
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

// Prune removes finished job records whose Finished time is older than cutoff,
// best-effort deleting each one's remote logfile (rm -f) through exec. Running
// jobs are never reaped — a long-running job's log must not be deleted out from
// under it; the agent-side mtime sweep (see App.PruneJobLogs) catches truly
// stale files. Remote deletion failures are ignored (an unreachable server just
// leaves its /var/tmp file for the sweep). Returns the number of records removed.
func (s *JobStore) Prune(cutoff time.Time, exec ExecFunc) (int, error) {
	var removed int
	// Collected under the lock, executed after it is released. Doing the remote
	// deletions inline would hold a CROSS-PROCESS advisory lock across N
	// sequential SSH round-trips, so one unreachable server could stall every
	// other `fleet` invocation until the 10s lock timeout. The record removal is
	// the part that needs to be atomic; the log cleanup is best-effort and its
	// failures are already ignored.
	type pendingDelete struct {
		server  string
		logfile string
	}
	var pending []pendingDelete
	err := s.withWriteLock(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		doc, err := s.read()
		if err != nil {
			return err
		}
		var kept []JobRecord
		removed = 0
		pending = pending[:0]
		for _, j := range doc.Jobs {
			if (j.Status == JobDone || j.Status == JobLost) && !j.Finished.IsZero() && j.Finished.Before(cutoff) {
				removed++
				if exec != nil && j.Logfile != "" {
					pending = append(pending, pendingDelete{server: j.Server, logfile: j.Logfile})
				}
				continue
			}
			kept = append(kept, j)
		}
		if removed == 0 {
			return nil
		}
		doc.Jobs = kept
		return s.write(doc)
	})
	if err != nil {
		return removed, err
	}
	for _, p := range pending {
		_, _, _ = exec(p.server, "rm -f "+shellQuote(p.logfile))
	}
	return removed, nil
}

// maxJobLogBytes bounds how much of a job's remote logfile Tail reads into memory
// (the tail end, where the completion marker lives).
const maxJobLogBytes = 4 << 20 // 4 MiB

// jobProbeBytes is how much of the log's end Check reads. The marker line is
// about 45 bytes, so this finds it without pulling the job's output across the
// network on every poll.
const jobProbeBytes = 512

// jobLogDir is the remote directory job logs are written to. It is a variable
// only so tests that run the in-process agent keep their logs in a temp dir.
var jobLogDir = "/var/tmp"

// JobLogfile returns the remote logfile path for a job. The per-job nonce is
// included so the path is UNPREDICTABLE (another local user on the remote host
// can't guess it from the sequential id), and buildJobLaunch creates it 0600
// (umask 077) so it isn't world-readable.
func JobLogfile(id int, nonce string) string {
	return jobLogDir + "/fleet-job-" + strconv.Itoa(id) + "-" + nonce + ".log"
}

// buildJobLaunch builds the detached launcher command. The command is wrapped
// so its combined output is redirected to the logfile and a
// FLEETEXIT:<nonce>:<code> marker is appended once it finishes; setsid +
// </dev/null fully detaches it so the agent RPC returns immediately.
//
// The user command is run via a nested `sh -c <quoted cmd>` rather than spliced
// directly into the brace group, so that a trailing ';' (e.g. "sleep 60;") or
// any other shell metacharacter cannot break the surrounding script. The
// FLEETEXIT:<nonce>:<code> marker is then printed as a separate statement using
// the command's real exit code ($?), so completion is always recorded. It is
// printed after a newline of its own: the marker is only accepted at the start
// of a line, and output that doesn't end in one (`printf done`) used to leave
// it mid-line and the job "running" forever. stripExitMarker removes that
// newline again.
//
// The wrapper shell stays alive for as long as the job runs, and its command
// line carries the marker text; that is how a probe tells a running job from
// one whose process is gone (see buildJobProbe).
//
// SECURITY: only this launcher knows the per-job nonce, so the user command's
// own output cannot forge the completion marker. The nonce is a fixed-charset
// hex token (see newJobNonce), so it carries no shell metacharacters; it is
// embedded inside the already single-quoted inner script regardless.
//
// Every other interpolated value (the user command and the logfile path) is
// single-quoted for safe embedding in the remote /bin/sh command.
func buildJobLaunch(command, logfile, nonce string) string {
	inner := "umask 077; { sh -c " + shellQuote(command) + "; printf '\\n" + jobExitMarker + nonce + ":%s\\n' \"$?\"; } > " + shellQuote(logfile) + " 2>&1"
	// The background setsid is not a process group leader, so it does not
	// fork: $! is the wrapper shell itself, reported for liveness checks.
	return "setsid sh -c " + shellQuote(inner) + " </dev/null >/dev/null 2>&1 & printf '" + jobPIDPrefix + "%s\\n' \"$!\""
}

// jobPIDPrefix starts the line in which the launcher reports the wrapper's pid.
const jobPIDPrefix = "fleet-job-pid:"

// parseJobPID reads the wrapper pid the launcher reported, or 0.
func parseJobPID(out string) int {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), jobPIDPrefix); ok {
			if pid, err := strconv.Atoi(v); err == nil && pid > 1 {
				return pid
			}
		}
	}
	return 0
}

// Start records a new job and launches it on the server via exec. The returned
// record is the persisted job (status running). The exec call is expected to
// return promptly because the remote launcher detaches the work.
//
// The job is recorded before the launch and the jobs lock is NOT held across
// it: the launch is a network round-trip, and holding a cross-process lock
// through it let one slow or unreachable server stall every other fleet
// process that touches jobs.json until the lock timed out. Recording first
// also means a launched job is always tracked, even if this process dies
// before the launch returns (it then finishes or is marked lost as usual).
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
	nonce, err := newJobNonce()
	if err != nil {
		return JobRecord{}, err
	}

	var rec JobRecord
	err = s.withWriteLock(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		doc, err := s.read()
		if err != nil {
			return err
		}
		id := s.reserve(&doc)
		rec = JobRecord{
			ID:      id,
			Name:    strings.TrimSpace(name),
			Server:  server,
			Command: command,
			Logfile: JobLogfile(id, nonce),
			Nonce:   nonce,
			Status:  JobRunning,
			Started: time.Now().UTC(),
		}
		doc.Jobs = append(doc.Jobs, rec)
		return s.write(doc)
	})
	if err != nil {
		return JobRecord{}, err
	}

	launchOut, _, err := exec(server, buildJobLaunch(command, rec.Logfile, nonce))
	if err == nil {
		pid := parseJobPID(launchOut)
		if updated, _, uerr := s.transition(rec.ID, func(j *JobRecord) bool {
			j.PID, j.Launched = pid, time.Now().UTC()
			return true
		}); uerr == nil && updated.ID == rec.ID {
			rec = updated
		}
		return rec, nil
	}
	// A launch that may have reached the server (the connection dropped
	// after the request was sent, or the agent failed after starting it)
	// may be running: keep it, so the helper settles it as done or lost,
	// rather than forget a job that runs.
	if !launchCertainlyFailed(err) {
		return rec, fmt.Errorf("launch job %d on %s may have run (%w); it stays tracked: check it with `fleet job status %d`", rec.ID, server, err, rec.ID)
	}
	// Forget the job we certainly failed to launch, and give its id back
	// unless another job has taken the next one in the meantime.
	_ = s.withWriteLock(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		doc, rerr := s.read()
		if rerr != nil {
			return rerr
		}
		kept := doc.Jobs[:0]
		for _, j := range doc.Jobs {
			if j.ID != rec.ID {
				kept = append(kept, j)
			}
		}
		doc.Jobs = kept
		if doc.Counter == rec.ID {
			doc.Counter--
		}
		return s.write(doc)
	})
	return JobRecord{}, fmt.Errorf("launch job on %s: %w", server, err)
}

// preExecAgentCodes are agent answers to shell.exec that come before anything
// runs: the request was refused as it stood. Others (internal_error among
// them) can come after the command started.
var preExecAgentCodes = map[string]bool{
	"bad_payload": true, "unsupported_action": true, "unknown_action": true,
	"capability_denied": true, "forbidden": true, "unauthorized": true, "policy_denied": true,
}

// launchCertainlyFailed reports whether a launch error means the job never
// started: it was never sent, or the agent refused it before running it.
func launchCertainlyFailed(err error) bool {
	if transport.RequestNotSent(err) {
		return true
	}
	var refused *AgentRefusedError
	return errors.As(err, &refused) && preExecAgentCodes[refused.Code]
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

// stripExitMarker removes the authentic FLEETEXIT:<nonce>:<code> trailer, and
// the newline the launcher writes in front of it, so log output shown to the
// user does not include the sentinel. (Jobs launched by older releases have no
// such newline; for those it is the output's own last newline that goes, which
// is what was always shown.) A spoofed/non-authentic "FLEETEXIT:..." line in
// the user's own output is left untouched, since it is genuine job output.
func stripExitMarker(logContent, nonce string) string {
	idx := markerLineIndex(logContent, nonce)
	if idx < 0 {
		return logContent
	}
	return strings.TrimSuffix(logContent[:idx], "\n")
}

// Tail reads the job's logfile from the server (up to maxJobLogBytes of its
// end) and returns its content with the FLEETEXIT marker stripped. If the job
// has finished, the stored record is updated (status done + exit code) and the
// refreshed record is returned. It is for showing output (`fleet job logs`);
// Check is the cheap way to learn a job's state.
func (s *JobStore) Tail(exec ExecFunc, id int) (rec JobRecord, output string, err error) {
	if exec == nil {
		return JobRecord{}, "", fmt.Errorf("exec function is required")
	}
	rec, err = s.Get(id)
	if err != nil {
		return JobRecord{}, "", err
	}
	// Read the logfile bounded to the last maxJobLogBytes so a runaway job whose
	// output grew huge can't force the controller to slurp an unbounded file into
	// memory. The completion marker (FLEETEXIT…) is the final line, so reading the
	// tail still detects completion; the only loss is older output on a giant log.
	raw, _, execErr := exec(rec.Server, "tail -c "+strconv.Itoa(maxJobLogBytes)+" "+shellQuote(rec.Logfile)+" 2>/dev/null")
	if execErr != nil {
		return rec, "", fmt.Errorf("read job %d log: %w", id, execErr)
	}
	output = stripExitMarker(raw, rec.Nonce)
	// A marker in the log may have been printed by the job itself; whether
	// the job has really finished is Check's to decide (it looks at the
	// wrapper process too).
	if rec.Status == JobRunning {
		if _, done := parseExit(raw, rec.Nonce); done {
			if checked, cerr := s.Check(exec, id); cerr == nil {
				rec = checked
			}
		}
	}
	return rec, output, err
}

// jobProbeHeader starts the first line of a probe's reply; see buildJobProbe.
const jobProbeHeader = "FLEETPROBE:"

// buildJobProbe builds the command Check runs: one header line, then (if the
// log exists) the last tailBytes of the log:
//
//	FLEETPROBE:<alive|dead|unknown>:<log|nolog>:<exit code, or empty>
//	<end of the log>
//
// alive/dead says whether any process on the server still carries the job's
// marker text in its command line — the launcher's wrapper shell does for as
// long as the job runs. Linux is asked through /proc/<pid>/cmdline; elsewhere
// `ps -A -ww -o args=` is used. unknown means the server could not tell (no
// /proc and no usable ps, or grep failed), and is never held against the job.
// The pattern is written as "[F]LEETEXIT:…" so it matches the wrapper but not
// this probe or grep itself, whose command lines hold the bracketed form.
// Liveness is checked before the log is read: a wrapper is gone only after it
// has written the marker (or was killed), so a job that ends between the two
// steps is never mistaken for a lost one.
//
// The exit code field covers a marker that is no longer in the tail: a job
// that leaves a child writing to its log (`./server &`) gets its marker
// followed by more output. When the wrapper is not known to be alive, the
// probe searches the log on the server for the line-anchored marker — all of
// it when the wrapper is gone (once, then the job is done), the last
// maxJobLogBytes when the server can't tell, as Tail always did — and sends
// back only the code.
//
// The header is printed before any log content, so nothing the job writes can
// forge it.
func buildJobProbe(logfile, nonce string, tailBytes, pid int) string {
	q := shellQuote(logfile)
	marker := "[" + jobExitMarker[:1] + "]" + jobExitMarker[1:] + nonce + ":"
	pattern := shellQuote(marker)
	anchored := shellQuote("^" + marker + "[0-9][0-9]*$")
	// Liveness: with the wrapper's pid, only that process counts (its
	// command line carries the marker); without it (a job launched by an
	// older release), any process whose command line carries it.
	liveness := "if [ -r /proc/self/cmdline ]; then grep -qsa -e " + pattern + " /proc/[0-9]*/cmdline; case $? in 0) s=alive ;; 1) s=dead ;; esac; " +
		"elif p=$(ps -A -ww -o args= 2>/dev/null) && [ -n \"$p\" ]; then printf '%s\\n' \"$p\" | grep -qs -e " + pattern + "; case $? in 0) s=alive ;; 1) s=dead ;; esac; fi; "
	if pid > 1 {
		ps := strconv.Itoa(pid)
		liveness = "if [ -r /proc/self/cmdline ]; then if grep -qsa -e " + pattern + " /proc/" + ps + "/cmdline; then s=alive; else s=dead; fi; " +
			"elif command -v ps >/dev/null 2>&1; then if ps -ww -o args= -p " + ps + " 2>/dev/null | grep -qs -e " + pattern + "; then s=alive; else s=dead; fi; fi; "
	}
	return "s=unknown; " + liveness +
		"if [ -e " + q + " ]; then c=; " +
		"case $s in dead) c=$(grep -a -e " + anchored + " " + q + " 2>/dev/null | tail -n 1) ;; " +
		"unknown) c=$(tail -c " + strconv.Itoa(maxJobLogBytes) + " " + q + " 2>/dev/null | grep -a -e " + anchored + " | tail -n 1) ;; esac; " +
		"printf '" + jobProbeHeader + "%s:log:%s\\n' \"$s\" \"${c##*:}\"; tail -c " + strconv.Itoa(tailBytes) + " " + q + " 2>/dev/null; " +
		"else printf '" + jobProbeHeader + "%s:nolog:\\n' \"$s\"; fi"
}

// jobProbe is a parsed probe reply.
type jobProbe struct {
	liveness  string // "alive", "dead" or "unknown"
	logExists bool
	exitCode  int // valid when exited
	exited    bool
	logTail   string
}

// parseJobProbe parses buildJobProbe's reply. Lines before the header (a
// shell's startup chatter) are skipped; the first header line is the probe's
// own, since the log content it may be followed by comes after it.
func parseJobProbe(out string) (jobProbe, error) {
	if !strings.HasPrefix(out, jobProbeHeader) {
		i := strings.Index(out, "\n"+jobProbeHeader)
		if i < 0 {
			first, _, _ := strings.Cut(out, "\n")
			if len(first) > 80 {
				first = first[:80] + "…"
			}
			return jobProbe{}, fmt.Errorf("unexpected reply to the job probe: %q", first)
		}
		out = out[i+1:]
	}
	head, rest, _ := strings.Cut(out, "\n")
	fields := strings.Split(strings.TrimSuffix(head, "\r"), ":")
	if (len(fields) != 3 && len(fields) != 4) || fields[0]+":" != jobProbeHeader {
		if len(head) > 80 {
			head = head[:80] + "…"
		}
		return jobProbe{}, fmt.Errorf("unexpected reply to the job probe: %q", head)
	}
	p := jobProbe{liveness: fields[1], logTail: rest}
	switch fields[1] {
	case "alive", "dead", "unknown":
	default:
		return jobProbe{}, fmt.Errorf("unexpected job probe liveness %q", fields[1])
	}
	switch fields[2] {
	case "log":
		p.logExists = true
	case "nolog":
	default:
		return jobProbe{}, fmt.Errorf("unexpected job probe log state %q", fields[2])
	}
	if len(fields) == 4 && fields[3] != "" {
		code, err := strconv.Atoi(fields[3])
		if err != nil || code < 0 {
			return jobProbe{}, fmt.Errorf("unexpected job probe exit code %q", fields[3])
		}
		p.exitCode, p.exited = code, true
	}
	return p, nil
}

// Check finds out whether a running job has finished, reading only the last
// jobProbeBytes of its log, and records the outcome: done (with its exit
// code), lost (see jobLostStrikes), or still running. A job that is already
// done or lost is returned as stored, without contacting its server.
//
// A transport error is returned as is and changes nothing, unless the job's
// server is known to have been removed (ServerRemoved), which loses the job.
func (s *JobStore) Check(exec ExecFunc, id int) (JobRecord, error) {
	if exec == nil {
		return JobRecord{}, fmt.Errorf("exec function is required")
	}
	rec, err := s.Get(id)
	if err != nil || rec.Status != JobRunning {
		return rec, err
	}
	out, _, execErr := exec(rec.Server, buildJobProbe(rec.Logfile, rec.Nonce, jobProbeBytes, rec.PID))
	if execErr != nil {
		if s.ServerRemoved != nil && s.ServerRemoved(rec.Server) {
			return s.lose(id, fmt.Sprintf("server %s was removed from the fleet", rec.Server))
		}
		return rec, fmt.Errorf("check job %d on %s: %w", id, rec.Server, execErr)
	}
	probe, err := parseJobProbe(out)
	if err != nil {
		return rec, fmt.Errorf("check job %d on %s: %w", id, rec.Server, err)
	}
	// The server found the last authentic marker in the whole log, once the
	// wrapper was gone.
	if probe.exited {
		return s.finish(id, probe.exitCode)
	}
	// A marker in the recent output counts only once the wrapper has gone:
	// it writes its marker as its last act, so one seen while it still runs
	// was printed by the job itself (from output someone else influenced).
	if probe.liveness != "alive" {
		if code, done := parseExit(probe.logTail, rec.Nonce); done {
			return s.finish(id, code)
		}
	}
	switch {
	case !probe.logExists:
		return s.strike(id, "its log file "+rec.Logfile+" is gone")
	case probe.liveness == "dead":
		return s.strike(id, "no process on "+rec.Server+" runs it any more (the server rebooted, or the job was killed)")
	case probe.liveness == "alive" && rec.Strikes > 0:
		updated, _, err := s.transition(id, func(j *JobRecord) bool {
			if j.Strikes == 0 {
				return false
			}
			j.Strikes = 0
			return true
		})
		return updated, err
	}
	return rec, nil
}

// finish records that a running job exited with code.
func (s *JobStore) finish(id, code int) (JobRecord, error) {
	rec, _, err := s.transition(id, func(j *JobRecord) bool {
		j.Status = JobDone
		j.ExitCode = code
		j.Finished = time.Now().UTC()
		j.Strikes = 0
		return true
	})
	return rec, err
}

// lose gives up on a running job at once.
func (s *JobStore) lose(id int, reason string) (JobRecord, error) {
	rec, _, err := s.transition(id, func(j *JobRecord) bool {
		j.Status = JobLost
		j.LostReason = reason
		j.Finished = time.Now().UTC()
		return true
	})
	return rec, err
}

// strike counts one probe that found a running job unable to finish, and
// gives up on it once the rule in jobLostStrikes is met.
func (s *JobStore) strike(id int, reason string) (JobRecord, error) {
	rec, _, err := s.transition(id, func(j *JobRecord) bool {
		// Its launch may not have returned yet: the log and the wrapper may
		// not exist so far, which says nothing about the job. (A job from an
		// older release never records Launched; it is struck once the grace
		// has passed, as before.)
		if j.Launched.IsZero() && time.Since(j.Started) < jobLaunchGrace {
			return false
		}
		j.Strikes++
		if j.Strikes >= jobLostStrikes && time.Since(j.Started) >= jobLostMinAge {
			j.Status = JobLost
			j.LostReason = reason
			j.Finished = time.Now().UTC()
		}
		return true
	})
	return rec, err
}

// transition applies change to a job that is still running, as a
// compare-and-set: under the store's write lock it re-reads the job and calls
// change only if the job is still JobRunning, so when several pollers (the
// background helper, `fleet job wait`, `fleet job logs`) see the same outcome,
// exactly one of them records it. change reports whether it modified the
// record. The returned record is the stored one afterwards; ended reports
// whether this call moved the job out of JobRunning. Only that caller sends
// the "job-failed" notification, so it goes out once.
func (s *JobStore) transition(id int, change func(*JobRecord) bool) (rec JobRecord, ended bool, err error) {
	err = s.withWriteLock(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		doc, err := s.read()
		if err != nil {
			return err
		}
		for i := range doc.Jobs {
			if doc.Jobs[i].ID != id {
				continue
			}
			if doc.Jobs[i].Status != JobRunning || !change(&doc.Jobs[i]) {
				rec = doc.Jobs[i]
				return nil
			}
			rec = doc.Jobs[i]
			ended = rec.Status != JobRunning
			return s.write(doc)
		}
		return fmt.Errorf("job %d not found", id)
	})
	if err != nil {
		return rec, false, err
	}
	// Notify outside the lock: it is a network call.
	if ended && (rec.Status == JobLost || rec.ExitCode != 0) {
		s.fireJobFailed(rec)
	}
	return rec, ended, nil
}

// fireJobFailed sends a best-effort "job-failed" notification for a job that
// finished with a non-zero exit code or was lost. The NotifyStore is loaded
// from the same config dir as the job store (jobs.json's parent), so it
// requires no extra wiring. Any failure — including a panic — is swallowed so
// it can never break Tail/Check/Wait.
func (s *JobStore) fireJobFailed(rec JobRecord) {
	defer func() { _ = recover() }()
	configDir := filepath.Dir(s.path)
	msg := fmt.Sprintf("job %d on %s failed (exit %d): %s", rec.ID, rec.Server, rec.ExitCode, rec.Command)
	if rec.Status == JobLost {
		msg = fmt.Sprintf("job %d on %s was lost (%s): %s", rec.ID, rec.Server, rec.LostReason, rec.Command)
	}
	_ = NewNotifyStore(configDir).Fire(NotifyEventJobFailed, msg)
}

// Wait polls the job with Check until it is no longer running (done or lost)
// or the timeout elapses. A non-positive timeout waits indefinitely. poll
// controls the gap between checks; sleep abstracts time.Sleep so tests can run
// without delay. The caller tells done from lost by the returned Status.
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
		rec, err := s.Check(exec, id)
		if err != nil {
			return JobRecord{}, err
		}
		if rec.Status != JobRunning {
			return rec, nil
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return rec, fmt.Errorf("timed out waiting for job %d after %s", id, timeout)
		}
		sleep(poll)
	}
}
