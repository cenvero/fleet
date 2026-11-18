// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cenvero/fleet/internal/version"
)

// The background helper is one small, always-running process per config dir
// that does the controller's background work: it tracks running jobs, runs
// the hourly agent auto-sync and prunes old job logs. It is separate from the
// controller daemon (`fleet start`), which is optional; the helper is not.
//
//   - Every fleet command starts it when it isn't running (the CLI's pre-run
//     calls EnsureHelper: one lock probe, nothing else, when it is). There is
//     no user-facing off switch; each task honours its own settings instead.
//   - It runs as the hidden `fleet --config-dir <dir> __helper`, detached from
//     the terminal, with output in logs/helper.log and FLEET_TOKEN removed from
//     its environment. It never runs under a token.
//   - It holds data/helper.lock for its whole life, so there is never more
//     than one per config dir; data/helper.pid is informational.
//   - It exits by itself when its executable changes (an upgrade) or the
//     config dir is gone or no longer initialized, and on SIGTERM. Commands
//     that replace or remove the controller stop it first (StopHelper).
//   - It reports what it is doing in data/helper-status.json (HelperStatus).
//
// Adding a task: write a func(ctx, *App) error in package core and add a
// HelperTask for it to helperRegistry. The runner gives each run a freshly
// opened App (closed afterwards, so config changes are picked up and the
// databases are not held open), a timeout, and panic recovery, and records
// the outcome in the status file. A task should
//
//   - check its own on/off setting on every run (from app.Config, which is
//     re-read each run) and return nil with a helperSay note when it is off;
//   - return an error only for something an operator should see (`fleet
//     helper status` shows it, and so will `fleet errors`), and call helperSay
//     with a one-line summary of what it did;
//   - honour ctx and keep its network calls bounded;
//   - leave work the daemon already does to the daemon (daemonLockHeld).
//
// Tasks run on their own goroutines (a long agent sync never delays job
// tracking) and a task never overlaps itself. Times are wall-clock, and the
// next run is scheduled from the end of the last one, so a laptop waking from
// sleep runs each task once, not once per missed interval.

// HelperCommand is the hidden fleet subcommand the helper runs as.
const HelperCommand = "__helper"

const (
	// helperLogRotateBytes rotates helper.log to helper.log.1 when the helper
	// is started and finds it larger than this.
	helperLogRotateBytes = 5 << 20
	// helperStatusFormat is HelperStatus.Format.
	helperStatusFormat = 1
	// helperLaunchThrottle stops commands run in quick succession (or a
	// helper that cannot start) from launching a helper each time.
	helperLaunchThrottle = 10 * time.Second
	// HelperStopTimeout bounds how long StopHelper waits for the helper to exit.
	HelperStopTimeout = 10 * time.Second
	// helperTaskStopWait bounds how long an exiting helper waits for its
	// tasks to wind down after cancelling them.
	helperTaskStopWait = 10 * time.Second
	// helperDefaultTaskTimeout bounds a run of a task that sets no Timeout.
	helperDefaultTaskTimeout = 5 * time.Minute
	// helperErrorTextLimit truncates errors stored in the status file.
	helperErrorTextLimit = 500
)

// Variables so tests can shorten them.
var (
	// helperClaimWait covers a concurrent EnsureHelper or StopHelper probe
	// holding the lock for an instant while it checks for a running helper.
	helperClaimWait = 2 * time.Second
	// helperTick is the longest the runner sleeps before looking again at its
	// schedule, its executable and the config dir.
	helperTick = 5 * time.Second
	// helperHeartbeat is how often the status file is rewritten when nothing
	// in it changed, so updated_at shows the helper is alive.
	helperHeartbeat = time.Minute
)

var (
	// ErrHelperRunning reports that a helper already holds the config dir.
	ErrHelperRunning = errors.New("the fleet background helper is already running")
	// ErrHelperBinaryChanged ends a helper whose executable was replaced.
	ErrHelperBinaryChanged = errors.New("the fleet executable changed")
	// ErrHelperConfigGone ends a helper whose config dir is gone or no longer
	// initialized.
	ErrHelperConfigGone = errors.New("the config dir is gone or no longer initialized")
)

// HelperLockPath is the lock a running helper holds.
func HelperLockPath(configDir string) string {
	return filepath.Join(configDir, "data", "helper.lock")
}

// HelperPIDPath is where a running helper records its process id.
func HelperPIDPath(configDir string) string {
	return filepath.Join(configDir, "data", "helper.pid")
}

// HelperStatusPath is the helper's status file; see HelperStatus.
func HelperStatusPath(configDir string) string {
	return filepath.Join(configDir, "data", "helper-status.json")
}

func helperStatusLockPath(configDir string) string {
	return filepath.Join(configDir, "data", "helper-status.lock")
}

// HelperLogPath is where the helper's output goes.
func HelperLogPath(configDir string) string {
	return filepath.Join(configDir, "logs", "helper.log")
}

// HelperArgs is the command line the helper runs with. It carries no
// credentials.
func HelperArgs(configDir string) []string {
	return []string{"--config-dir", configDir, HelperCommand}
}

// HelperSupported reports whether this binary runs the background helper: a
// release build that is not a Go test binary. A development build never
// starts one.
func HelperSupported() bool {
	return AgentAutoSyncSupported() && !runningAsGoTest()
}

// runningAsGoTest reports whether this process is a `go test` binary, which
// must never launch a helper that outlives the test.
func runningAsGoTest() bool {
	base := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	return strings.HasSuffix(base, ".test")
}

// HelperStatus is data/helper-status.json, the record of what the background
// helper is doing, for `fleet helper status`, `fleet errors` and anything else
// that wants to surface background problems. It is written atomically (temp
// file + rename, mode 0600) under data/helper-status.lock, by the helper and,
// for launch failures only, by the command that tried to start it. Times are
// RFC 3339 in UTC; a zero time is left out.
//
//	{
//	  "format": 1,
//	  "pid": 4242,                              // the helper that wrote this
//	  "started_at": "2026-11-18T09:00:00Z",
//	  "version": "v1.4.0",                      // its version.Version
//	  "updated_at": "2026-11-18T09:41:05Z",     // last write; at least once a minute while it runs
//	  "stopped_at": "...", "stop_reason": "...", // set when it exits cleanly, cleared on start
//	  "last_launch_at": "...",                  // last time a command started a helper
//	  "last_launch_error": "...",               // the last failure to start one,
//	  "last_launch_error_at": "...",            //   and when (resolved if before started_at)
//	  "tasks": {
//	    "jobs": {
//	      "interval": "5s",
//	      "last_run_at": "...",                 // last run that did work
//	      "next_run_at": "...",                 // refreshed with every write
//	      "last_result": "ok",                  // "ok" or "error": how the last run went
//	      "last_message": "watching 2 running jobs",
//	      "last_error": "...",                  // the most recent failure, kept after later
//	      "last_error_at": "..."                //   successes; current if last_result is "error"
//	    }
//	  }
//	}
//
// Whether the helper is running is never read from this file: the lock says
// that (HelperState). Readers should ignore fields they don't know; new ones
// may be added without changing format, which changes only if existing fields
// change meaning.
type HelperStatus struct {
	Format            int                         `json:"format"`
	PID               int                         `json:"pid,omitempty"`
	StartedAt         time.Time                   `json:"started_at,omitzero"`
	Version           string                      `json:"version,omitempty"`
	UpdatedAt         time.Time                   `json:"updated_at,omitzero"`
	StoppedAt         time.Time                   `json:"stopped_at,omitzero"`
	StopReason        string                      `json:"stop_reason,omitempty"`
	LastLaunchAt      time.Time                   `json:"last_launch_at,omitzero"`
	LastLaunchError   string                      `json:"last_launch_error,omitempty"`
	LastLaunchErrorAt time.Time                   `json:"last_launch_error_at,omitzero"`
	Tasks             map[string]HelperTaskStatus `json:"tasks,omitempty"`
}

// HelperTaskStatus is one task's entry in HelperStatus.Tasks.
type HelperTaskStatus struct {
	Interval    string    `json:"interval"`
	LastRunAt   time.Time `json:"last_run_at,omitzero"`
	NextRunAt   time.Time `json:"next_run_at,omitzero"`
	LastResult  string    `json:"last_result,omitempty"`
	LastMessage string    `json:"last_message,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
}

// Values of HelperTaskStatus.LastResult.
const (
	HelperResultOK    = "ok"
	HelperResultError = "error"
)

// ReadHelperStatus returns the status file's contents, or a zero status when
// there is none yet.
func ReadHelperStatus(configDir string) (HelperStatus, error) {
	var status HelperStatus
	data, err := os.ReadFile(HelperStatusPath(configDir)) // #nosec G304 -- fixed file inside the controller's own config dir
	if err != nil {
		if os.IsNotExist(err) {
			return status, nil
		}
		return status, err
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return HelperStatus{}, fmt.Errorf("decode %s: %w", HelperStatusPath(configDir), err)
	}
	return status, nil
}

// updateHelperStatus applies fn to the status file under its lock, writing it
// back when fn reports a change. An unreadable file is started afresh.
func updateHelperStatus(configDir string, fn func(*HelperStatus) bool) error {
	return withAdvisoryFileLock(helperStatusLockPath(configDir), func() error {
		status, _ := ReadHelperStatus(configDir)
		if !fn(&status) {
			return nil
		}
		status.Format = helperStatusFormat
		data, err := json.MarshalIndent(status, "", "  ")
		if err != nil {
			return err
		}
		return writeHelperFile(HelperStatusPath(configDir), append(data, '\n'))
	})
}

// writeHelperFile replaces path with an owner-only file holding data (temp
// file in the same directory + rename), so readers never see a partial file.
func writeHelperFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func truncateHelperText(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > helperErrorTextLimit {
		return s[:helperErrorTextLimit] + "…"
	}
	return s
}

// HelperInstance is a running helper's claim on its config dir.
type HelperInstance struct {
	configDir string
	pid       int
	lock      *os.File
}

// ClaimHelper makes the calling process the config dir's helper: it takes the
// helper lock and writes the pid file. It fails with ErrHelperRunning when
// another helper holds it. Call Release when the helper stops.
func ClaimHelper(configDir string) (*HelperInstance, error) {
	if err := os.MkdirAll(filepath.Join(configDir, "data"), 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	lockPath := HelperLockPath(configDir)
	lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- fixed file inside the controller's own config dir
	if err != nil {
		return nil, fmt.Errorf("open helper lock %s: %w", lockPath, err)
	}
	deadline := time.Now().Add(helperClaimWait)
	for {
		acquired, err := tryAdvisoryFileLock(lock)
		if err != nil {
			_ = lock.Close()
			return nil, fmt.Errorf("acquire helper lock %s: %w", lockPath, err)
		}
		if acquired {
			break
		}
		if !time.Now().Before(deadline) {
			_ = lock.Close()
			return nil, ErrHelperRunning
		}
		time.Sleep(25 * time.Millisecond)
	}
	h := &HelperInstance{configDir: configDir, pid: os.Getpid(), lock: lock}
	if err := writeHelperFile(HelperPIDPath(configDir), []byte(strconv.Itoa(h.pid)+"\n")); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("write helper pid file: %w", err)
	}
	return h, nil
}

// Release removes the pid file (when it still names this process) and drops
// the lock.
func (h *HelperInstance) Release() {
	if h == nil || h.lock == nil {
		return
	}
	if pid, err := ReadHelperPID(h.configDir); err == nil && pid == h.pid {
		_ = os.Remove(HelperPIDPath(h.configDir))
	}
	_ = h.lock.Close()
	h.lock = nil
}

// ReadHelperPID returns the pid recorded in the helper's pid file.
func ReadHelperPID(configDir string) (int, error) {
	data, err := os.ReadFile(HelperPIDPath(configDir)) // #nosec G304 -- fixed file inside the controller's own config dir
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid helper pid file %s", HelperPIDPath(configDir))
	}
	return pid, nil
}

// HelperRunning reports whether a helper holds the config dir's lock. It takes
// the lock for an instant when it is free, which ClaimHelper's retry absorbs.
// This is the whole cost of EnsureHelper when the helper is up.
func HelperRunning(configDir string) bool {
	lock, err := os.OpenFile(HelperLockPath(configDir), os.O_RDWR, 0) // #nosec G304 -- fixed file inside the controller's own config dir
	if err != nil {
		return false // never created: no helper has run here
	}
	defer lock.Close()
	acquired, err := tryAdvisoryFileLock(lock)
	return err == nil && !acquired
}

// HelperState is what `fleet helper status` reports.
type HelperState struct {
	Supported  bool          `json:"supported"`
	Running    bool          `json:"running"`
	PID        int           `json:"pid,omitempty"`
	Uptime     string        `json:"uptime,omitempty"`
	LogPath    string        `json:"log_path"`
	StatusPath string        `json:"status_path"`
	Status     HelperStatus  `json:"status"`
	Tasks      []HelperBrief `json:"tasks"`
}

// HelperBrief names a registered task, in registry order.
type HelperBrief struct {
	Name     string `json:"name"`
	Interval string `json:"interval"`
}

// ReadHelperState gathers the helper's state for configDir.
func ReadHelperState(configDir string) (HelperState, error) {
	state := HelperState{
		Supported:  HelperSupported(),
		Running:    HelperRunning(configDir),
		LogPath:    HelperLogPath(configDir),
		StatusPath: HelperStatusPath(configDir),
	}
	for _, t := range helperRegistry() {
		state.Tasks = append(state.Tasks, HelperBrief{Name: t.Name, Interval: t.Interval.String()})
	}
	status, err := ReadHelperStatus(configDir)
	state.Status = status
	if state.Running {
		if pid, perr := ReadHelperPID(configDir); perr == nil {
			state.PID = pid
		}
		if !status.StartedAt.IsZero() && (state.PID == 0 || status.PID == state.PID) {
			state.Uptime = time.Since(status.StartedAt).Round(time.Second).String()
		}
	}
	return state, err
}

// EnsureHelper starts the config dir's helper with launch unless one is
// running, and reports whether it started one. When a helper is up it does
// nothing but probe the lock. A command that finds none launches one at most
// every helperLaunchThrottle (several commands in a row, or a helper that
// cannot start, do not each launch another); the ClaimHelper lock makes a
// second helper exit at once anyway. A launch failure is recorded in the
// status file and returned. Callers decide whether a helper may be started at
// all (release build, initialized config dir, no token); see the CLI.
func EnsureHelper(configDir string, launch func(configDir string) error) (bool, error) {
	if HelperRunning(configDir) {
		return false, nil
	}
	return launchHelper(configDir, launch, false)
}

func launchHelper(configDir string, launch func(string) error, force bool) (bool, error) {
	due := false
	if err := updateHelperStatus(configDir, func(st *HelperStatus) bool {
		// Read the clock under the lock: a time taken before waiting for it
		// would sort before the launch the previous holder just recorded.
		now := time.Now()
		since := now.Sub(st.LastLaunchAt)
		// A launch recorded more than a minute in the future means the clock
		// was set back; don't let that block launches.
		if !force && !st.LastLaunchAt.IsZero() && since > -time.Minute && since < helperLaunchThrottle {
			return false
		}
		st.LastLaunchAt = now.UTC()
		due = true
		return true
	}); err != nil {
		return false, err
	}
	if !due {
		return false, nil
	}
	if err := launch(configDir); err != nil {
		_ = updateHelperStatus(configDir, func(st *HelperStatus) bool {
			st.LastLaunchError = truncateHelperText(err.Error())
			st.LastLaunchErrorAt = time.Now().UTC()
			return true
		})
		return false, err
	}
	return true, nil
}

// RestartHelper stops the running helper (if any) and launches a new one at
// once, then waits up to wait for it to take its lock.
func RestartHelper(configDir string, launch func(string) error, wait time.Duration) error {
	if _, err := StopHelper(configDir, HelperStopTimeout); err != nil {
		return err
	}
	if _, err := launchHelper(configDir, launch, true); err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	for !HelperRunning(configDir) {
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the background helper did not start within %s; see %s", wait, HelperLogPath(configDir))
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// StartHelperProcess launches `fleet __helper` for configDir detached from the
// terminal (its own session; on Windows, no console), with its output appended
// to HelperLogPath and FLEET_TOKEN removed from its environment, and returns
// without waiting for it.
func StartHelperProcess(configDir string) error {
	if runningAsGoTest() {
		return errors.New("a test binary never starts the background helper")
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the fleet binary: %w", err)
	}
	logFile, _, err := openRotatingLog(HelperLogPath(configDir), helperLogRotateBytes)
	if err != nil {
		return err
	}
	defer logFile.Close()                              // the child holds its own descriptor
	cmd := exec.Command(exe, HelperArgs(configDir)...) // #nosec G204 -- this binary with fixed, credential-free arguments
	cmd.Dir = configDir
	cmd.Env = daemonEnv(os.Environ())
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachedProcAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch the background helper: %w", err)
	}
	// Reap the child if this process outlives it (a long dashboard session);
	// otherwise it is simply re-parented when this process exits.
	go func() { _ = cmd.Wait() }()
	return nil
}

// StopHelper asks the config dir's helper to exit (SIGTERM; on Windows the
// process is terminated) and waits up to timeout for it. Like StopDaemon it
// only signals the process that holds the helper lock and whose pid file names
// it (and, where the OS shows it, whose command line runs __helper), so a
// recycled pid is never touched. It reports whether a helper was stopped; one
// that is not running is not an error, and a stale pid file is removed.
func StopHelper(configDir string, timeout time.Duration) (bool, error) {
	if timeout <= 0 {
		timeout = HelperStopTimeout
	}
	if !HelperRunning(configDir) {
		if _, err := os.Stat(HelperPIDPath(configDir)); err == nil {
			_ = os.Remove(HelperPIDPath(configDir))
		}
		return false, nil
	}
	pid, err := ReadHelperPID(configDir)
	switch {
	case err != nil:
		return false, fmt.Errorf("a background helper holds %s but its pid file is unreadable (%v)", configDir, err)
	case !processAlive(pid):
		return false, fmt.Errorf("a background helper holds %s but its pid file names pid %d, which is not running", configDir, pid)
	}
	if ok, why := processLooksLikeHelper(pid); !ok {
		return false, fmt.Errorf("refusing to signal pid %d: %s", pid, why)
	}
	if err := terminateProcess(pid); err != nil {
		return false, fmt.Errorf("stop the background helper (pid %d): %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for processAlive(pid) && HelperRunning(configDir) {
		if !time.Now().Before(deadline) {
			return true, fmt.Errorf("the background helper (pid %d) did not exit within %s", pid, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if recorded, err := ReadHelperPID(configDir); err == nil && recorded == pid {
		_ = os.Remove(HelperPIDPath(configDir))
	}
	return true, nil
}

// HelperTask is one piece of the helper's background work; see the package
// comment above for how to write one.
type HelperTask struct {
	// Name identifies the task in the status file and `fleet helper status`.
	Name string
	// Interval is the gap between the end of one run and the start of the
	// next.
	Interval time.Duration
	// StartDelay is how long after the helper starts the first run waits,
	// unless the status file schedules it later (so a helper that restarts
	// often still runs a daily task daily).
	StartDelay time.Duration
	// Timeout bounds one run; zero means helperDefaultTaskTimeout.
	Timeout time.Duration
	// Pending, if set, is asked before each run whether there is anything to
	// do; when it says no, the run is skipped without opening the App and is
	// not recorded as a run. Keep it cheap and local.
	Pending func(configDir string, now time.Time) bool
	// Run does the work.
	Run func(ctx context.Context, app *App) error
}

// helperRegistry is the helper's task list. Each call returns fresh tasks (a
// task may keep state across its runs, like the job tracker's backoff).
func helperRegistry() []HelperTask {
	jobs := newJobTracker()
	return []HelperTask{
		{
			// Notices jobs finishing (or becoming impossible to finish) so
			// `fleet jobs` is current and "job-failed" fires without anyone
			// polling. Each job is probed about 10s after it starts, then
			// less often (up to every minute); an unreachable server is
			// retried with backoff up to every 5 minutes.
			Name: "jobs", Interval: 5 * time.Second, Timeout: 5 * time.Minute,
			Pending: jobs.pending, Run: jobs.run,
		},
		{
			// The hourly agent auto-sync (see agent_autosync.go). Checks every
			// 5 minutes whether the shared hourly run is due.
			Name: "agent-sync", Interval: 5 * time.Minute, StartDelay: 30 * time.Second,
			Timeout: agentAutoSyncRunTimeout + time.Minute, Run: helperAgentSync,
		},
		{
			// Removes job records and remote job logs past job-log-retention.
			Name: "job-log-prune", Interval: 24 * time.Hour, StartDelay: 2 * time.Minute,
			Timeout: 5 * time.Minute, Run: helperPruneJobLogs,
		},
	}
}

// helperReportKey carries a run's helperReport in its context.
type helperReportKey struct{}

type helperReport struct {
	mu  sync.Mutex
	msg string
}

// helperSay records a one-line summary of the current run of a helper task,
// shown by `fleet helper status`. The last call wins. Outside a helper run it
// does nothing.
func helperSay(ctx context.Context, format string, args ...any) {
	if r, ok := ctx.Value(helperReportKey{}).(*helperReport); ok {
		r.mu.Lock()
		r.msg = fmt.Sprintf(format, args...)
		r.mu.Unlock()
	}
}

// HelperOptions controls RunHelper. The zero value runs the registry.
type HelperOptions struct {
	// Tasks replaces the registry (tests).
	Tasks []HelperTask
	// Log receives the helper's log lines; nil means os.Stderr, which the
	// launcher points at HelperLogPath.
	Log io.Writer
	// Executable is the file whose replacement ends the helper; "" means
	// this process's executable.
	Executable string
}

// RunHelper is the body of `fleet __helper`: it claims the config dir's
// helper lock (failing with ErrHelperRunning when another helper holds it)
// and runs the tasks until ctx ends, SIGINT/SIGTERM arrives, the executable
// changes (ErrHelperBinaryChanged) or the config dir goes away
// (ErrHelperConfigGone).
func RunHelper(ctx context.Context, configDir string, opts HelperOptions) error {
	inst, err := ClaimHelper(configDir)
	if err != nil {
		return err
	}
	defer inst.Release()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	r := &helperRunner{
		configDir: configDir,
		tasks:     opts.Tasks,
		log:       opts.Log,
		exe:       opts.Executable,
		pid:       inst.pid,
		running:   map[string]bool{},
		next:      map[string]time.Time{},
		changed:   make(chan struct{}, 1),
	}
	if r.tasks == nil {
		r.tasks = helperRegistry()
	}
	if r.log == nil {
		r.log = os.Stderr
	}
	if r.exe == "" {
		if r.exe, err = os.Executable(); err != nil {
			return fmt.Errorf("locate the fleet binary: %w", err)
		}
	}
	if r.exeInfo, err = os.Stat(r.exe); err != nil {
		return fmt.Errorf("stat %s: %w", r.exe, err)
	}
	return r.run(ctx)
}

// helperRunner schedules and records the helper's tasks.
type helperRunner struct {
	configDir string
	tasks     []HelperTask
	log       io.Writer
	exe       string
	exeInfo   os.FileInfo
	pid       int

	mu        sync.Mutex
	status    HelperStatus // this helper's view; launch fields belong to the file
	running   map[string]bool
	next      map[string]time.Time
	lastWrite time.Time
	wg        sync.WaitGroup
	changed   chan struct{} // a task finished
	persistMu sync.Mutex    // keeps snapshots reaching the file in order
	logMu     sync.Mutex    // tasks log from their own goroutines
}

func (r *helperRunner) logf(format string, args ...any) {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	fmt.Fprintf(r.log, "%s helper: "+format+"\n", append([]any{time.Now().UTC().Format(time.RFC3339)}, args...)...)
}

// errHelperStopped is the exit reason when the helper is asked to stop.
var errHelperStopped = errors.New("stopped")

func (r *helperRunner) run(ctx context.Context) error {
	r.begin()
	taskCtx, cancelTasks := context.WithCancel(ctx)
	var reason error
	for reason == nil {
		if reason = r.exitReason(); reason != nil {
			break
		}
		now := wallNow()
		wake := now.Add(helperTick)
		r.mu.Lock()
		for _, t := range r.tasks {
			if r.running[t.Name] {
				continue
			}
			if due := r.next[t.Name]; !now.Before(due) {
				r.running[t.Name] = true
				r.wg.Add(1)
				go r.runTask(taskCtx, t)
			} else if due.Before(wake) {
				wake = due
			}
		}
		heartbeat := now.Sub(r.lastWrite) >= helperHeartbeat
		r.mu.Unlock()
		if heartbeat {
			r.persist()
		}
		timer := time.NewTimer(time.Until(wake))
		select {
		case <-ctx.Done():
			reason = errHelperStopped
		case <-r.changed:
		case <-timer.C:
		}
		timer.Stop()
	}
	cancelTasks()
	if !waitGroupWithin(&r.wg, helperTaskStopWait) {
		r.logf("tasks did not stop within %s; exiting anyway", helperTaskStopWait)
	}
	r.mu.Lock()
	r.status.StoppedAt = time.Now().UTC()
	r.status.StopReason = reason.Error()
	r.mu.Unlock()
	r.persist()
	r.logf("exiting (pid %d): %v", r.pid, reason)
	if reason == errHelperStopped {
		return nil
	}
	return reason
}

// waitGroupWithin waits for wg, or gives up after d; it reports whether wg
// finished.
func waitGroupWithin(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// begin initializes the status (keeping each task's history from the last
// helper) and the schedule, and writes the status file.
func (r *helperRunner) begin() {
	previous, _ := ReadHelperStatus(r.configDir)
	now := wallNow()
	r.mu.Lock()
	r.status = HelperStatus{
		PID:       r.pid,
		StartedAt: now.UTC(),
		Version:   version.Version,
		Tasks:     map[string]HelperTaskStatus{},
	}
	for _, t := range r.tasks {
		ts := previous.Tasks[t.Name]
		ts.Interval = t.Interval.String()
		next := now.Add(t.StartDelay)
		// A run the last helper scheduled later stays there, within reason.
		if later := ts.NextRunAt; later.After(next) && later.Before(now.Add(t.StartDelay+t.Interval)) {
			next = later
		}
		r.next[t.Name] = next
		r.status.Tasks[t.Name] = ts
	}
	r.mu.Unlock()
	r.persist()
	r.logf("started (pid %d, %s, %s)", r.pid, version.Version, r.exe)
}

// exitReason reports why the helper should stop, or nil.
func (r *helperRunner) exitReason() error {
	if !IsInitialized(r.configDir) {
		return ErrHelperConfigGone
	}
	info, err := os.Stat(r.exe)
	if err != nil || !os.SameFile(info, r.exeInfo) || info.Size() != r.exeInfo.Size() || !info.ModTime().Equal(r.exeInfo.ModTime()) {
		return fmt.Errorf("%w (%s)", ErrHelperBinaryChanged, r.exe)
	}
	return nil
}

// runTask runs one task once and records the result.
func (r *helperRunner) runTask(ctx context.Context, t HelperTask) {
	defer r.wg.Done()
	started := wallNow()
	message, skipped, err := r.invoke(ctx, t, started)
	r.finish(t, started, message, skipped, err)
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

// invoke calls the task with a fresh App, a timeout and panic recovery.
func (r *helperRunner) invoke(ctx context.Context, t HelperTask, now time.Time) (message string, skipped bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			r.logf("task %s panicked: %v\n%s", t.Name, p, debug.Stack())
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	if t.Pending != nil && !t.Pending(r.configDir, now) {
		return "", true, nil
	}
	app, err := Open(r.configDir)
	if err != nil {
		return "", false, fmt.Errorf("open the controller: %w", err)
	}
	defer app.Close()
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = helperDefaultTaskTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	report := &helperReport{}
	err = t.Run(context.WithValue(runCtx, helperReportKey{}, report), app)
	report.mu.Lock()
	message = report.msg
	report.mu.Unlock()
	return message, false, err
}

// finish records a run in the status and schedules the next one.
func (r *helperRunner) finish(t HelperTask, started time.Time, message string, skipped bool, err error) {
	r.mu.Lock()
	r.running[t.Name] = false
	r.next[t.Name] = wallNow().Add(t.Interval)
	if skipped {
		r.mu.Unlock()
		return
	}
	ts := r.status.Tasks[t.Name]
	before := ts
	ts.LastRunAt = started.UTC()
	ts.LastMessage = truncateHelperText(message)
	ts.LastResult = HelperResultOK
	if err != nil {
		ts.LastResult = HelperResultError
		ts.LastError = truncateHelperText(err.Error())
		ts.LastErrorAt = time.Now().UTC()
	}
	r.status.Tasks[t.Name] = ts
	changed := ts.LastResult != before.LastResult || ts.LastMessage != before.LastMessage || ts.LastError != before.LastError
	r.mu.Unlock()
	if err != nil && (before.LastResult != HelperResultError || ts.LastError != before.LastError) {
		r.logf("task %s failed: %v", t.Name, err)
	}
	if changed {
		r.persist()
	}
}

// persist writes this helper's view to the status file, keeping the fields
// launching commands own.
func (r *helperRunner) persist() {
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	if !IsInitialized(r.configDir) {
		return // removed (self-uninstall) or being replaced: don't recreate data/
	}
	r.mu.Lock()
	snapshot := r.status
	snapshot.UpdatedAt = time.Now().UTC()
	snapshot.Tasks = make(map[string]HelperTaskStatus, len(r.status.Tasks))
	for name, ts := range r.status.Tasks {
		if !r.running[name] {
			ts.NextRunAt = r.next[name].UTC()
		}
		snapshot.Tasks[name] = ts
	}
	r.lastWrite = wallNow()
	r.mu.Unlock()
	err := updateHelperStatus(r.configDir, func(st *HelperStatus) bool {
		snapshot.LastLaunchAt = st.LastLaunchAt
		snapshot.LastLaunchError = st.LastLaunchError
		snapshot.LastLaunchErrorAt = st.LastLaunchErrorAt
		*st = snapshot
		return true
	})
	if err != nil {
		r.logf("write %s: %v", HelperStatusPath(r.configDir), err)
	}
}
