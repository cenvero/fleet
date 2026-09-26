// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A `fleet daemon` claims its config directory for as long as it runs: it holds
// an advisory lock on data/daemon.lock (released by the kernel even if the
// process is killed) and records its pid in data/daemon.pid. `fleet start`,
// `fleet stop` and `fleet status` use the two together, so a second daemon for
// the same directory is refused, and `fleet stop` only ever signals the process
// that actually holds the lock — never a stale pid the system has since handed
// to an unrelated program.

const (
	// DaemonStartTimeout bounds how long `fleet start` waits for the daemon it
	// launched to bind its listeners.
	DaemonStartTimeout = 10 * time.Second
	// DaemonStopTimeout bounds how long `fleet stop` waits for the daemon to
	// exit after asking it to.
	DaemonStopTimeout = 15 * time.Second

	// daemonLogRotateBytes rotates daemon.log to daemon.log.1 when `fleet
	// start` finds it larger than this.
	daemonLogRotateBytes = 10 << 20
	// daemonLogTailLines is how much of the log a failed start prints.
	daemonLogTailLines = 20
)

// daemonClaimWait covers a concurrent `fleet status`/`stop` probe holding the
// lock for an instant while it checks whether a daemon is running.
var daemonClaimWait = 2 * time.Second

// ErrDaemonRunning reports that a daemon already holds the config directory.
var ErrDaemonRunning = errors.New("a fleet daemon is already running")

// DaemonPIDPath is the file a running daemon records its process id in.
func DaemonPIDPath(configDir string) string {
	return filepath.Join(configDir, "data", "daemon.pid")
}

// DaemonLogPath is where `fleet start` appends a detached daemon's output.
func DaemonLogPath(configDir string) string {
	return filepath.Join(configDir, "logs", "daemon.log")
}

func daemonLockPath(configDir string) string {
	return filepath.Join(configDir, "data", "daemon.lock")
}

// DaemonInstance is a running daemon's claim on its config directory.
type DaemonInstance struct {
	configDir string
	pid       int
	lock      *os.File
}

// ClaimDaemon makes the calling process the config directory's daemon: it takes
// the daemon lock and writes the pid file. It fails with ErrDaemonRunning when
// another daemon holds the directory. Call Release when the daemon stops.
func ClaimDaemon(configDir string) (*DaemonInstance, error) {
	if err := os.MkdirAll(filepath.Join(configDir, "data"), 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	lockPath := daemonLockPath(configDir)
	lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- fixed file inside the controller's own config dir
	if err != nil {
		return nil, fmt.Errorf("open daemon lock %s: %w", lockPath, err)
	}
	deadline := time.Now().Add(daemonClaimWait)
	for {
		acquired, err := tryAdvisoryFileLock(lock)
		if err != nil {
			_ = lock.Close()
			return nil, fmt.Errorf("acquire daemon lock %s: %w", lockPath, err)
		}
		if acquired {
			break
		}
		if !time.Now().Before(deadline) {
			_ = lock.Close()
			if pid, err := ReadDaemonPID(configDir); err == nil {
				return nil, fmt.Errorf("%w for %s (pid %d)", ErrDaemonRunning, configDir, pid)
			}
			return nil, fmt.Errorf("%w for %s", ErrDaemonRunning, configDir)
		}
		time.Sleep(50 * time.Millisecond)
	}
	d := &DaemonInstance{configDir: configDir, pid: os.Getpid(), lock: lock}
	if err := writeDaemonPIDFile(configDir, d.pid); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return d, nil
}

// Release removes the pid file (when it still names this process) and drops
// the lock.
func (d *DaemonInstance) Release() {
	if d == nil || d.lock == nil {
		return
	}
	if pid, err := ReadDaemonPID(d.configDir); err == nil && pid == d.pid {
		_ = os.Remove(DaemonPIDPath(d.configDir))
	}
	_ = d.lock.Close()
	d.lock = nil
}

// ReadDaemonPID returns the pid recorded in the config directory's pid file.
func ReadDaemonPID(configDir string) (int, error) {
	data, err := os.ReadFile(DaemonPIDPath(configDir)) // #nosec G304 -- fixed file inside the controller's own config dir
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid daemon pid file %s", DaemonPIDPath(configDir))
	}
	return pid, nil
}

// writeDaemonPIDFile atomically replaces the pid file with an owner-only copy.
func writeDaemonPIDFile(configDir string, pid int) error {
	path := DaemonPIDPath(configDir)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".daemon-pid-*")
	if err != nil {
		return fmt.Errorf("write daemon pid file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write daemon pid file: %w", err)
	}
	if _, err := tmp.WriteString(strconv.Itoa(pid) + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write daemon pid file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write daemon pid file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("write daemon pid file: %w", err)
	}
	return nil
}

// removeDaemonPIDFileIf removes the pid file if it still records pid.
func removeDaemonPIDFileIf(configDir string, pid int) bool {
	if recorded, err := ReadDaemonPID(configDir); err == nil && recorded != pid {
		return false
	}
	return os.Remove(DaemonPIDPath(configDir)) == nil
}

// daemonLockHeld reports whether a daemon currently holds the config
// directory's lock. It takes the lock for an instant when it is free, which
// ClaimDaemon's short retry absorbs.
func daemonLockHeld(configDir string) bool {
	lock, err := os.OpenFile(daemonLockPath(configDir), os.O_RDWR, 0) // #nosec G304 -- fixed file inside the controller's own config dir
	if err != nil {
		return false // never created: no daemon has run with this build
	}
	defer lock.Close()
	acquired, err := tryAdvisoryFileLock(lock)
	if err != nil {
		return false
	}
	return !acquired
}

// controlAddressAnswers reports whether something accepts TCP connections on
// the daemon's local control address.
func controlAddressAnswers(address string) bool {
	if strings.TrimSpace(address) == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", address, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// DaemonState is what another process can observe about a config directory's
// daemon.
type DaemonState struct {
	Running bool `json:"running"`
	// PID is the daemon's process id, when it holds the lock and its pid file
	// names a live process.
	PID     int    `json:"pid,omitempty"`
	PIDFile string `json:"pid_file"`
	// ControlAddress answering means a daemon is serving local control
	// requests — possibly one started by an older release, which records no
	// pid file.
	ControlAddress   string `json:"control_address,omitempty"`
	ControlReachable bool   `json:"control_reachable"`
	// StalePIDFile is a pid file left by a daemon that no longer runs.
	StalePIDFile bool `json:"stale_pid_file,omitempty"`
}

// DaemonStatus inspects the daemon for configDir without changing anything.
func DaemonStatus(configDir, controlAddress string) DaemonState {
	state := DaemonState{PIDFile: DaemonPIDPath(configDir), ControlAddress: controlAddress}
	pid, pidErr := ReadDaemonPID(configDir)
	held := daemonLockHeld(configDir)
	state.ControlReachable = controlAddressAnswers(controlAddress)
	if held && pidErr == nil && processAlive(pid) {
		state.PID = pid
	}
	if !held {
		if _, err := os.Stat(state.PIDFile); err == nil {
			state.StalePIDFile = true
		}
	}
	state.Running = held || state.ControlReachable
	return state
}

// Describe renders the state as one line for humans.
func (s DaemonState) Describe() string {
	switch {
	case s.Running && s.PID > 0:
		return fmt.Sprintf("fleet daemon is running (pid %d)", s.PID)
	case s.Running && s.ControlReachable:
		return fmt.Sprintf("a fleet daemon is answering on %s (no pid file: started by an older release or outside `fleet start`)", s.ControlAddress)
	case s.Running:
		return "fleet daemon is running"
	default:
		return "fleet daemon is not running"
	}
}

// DaemonStartOptions controls StartDaemon. Only ConfigDir is required.
type DaemonStartOptions struct {
	ConfigDir      string
	ControlAddress string
	// Executable and Args default to this binary and
	// `--config-dir <ConfigDir> daemon`.
	Executable string
	Args       []string
	// Env defaults to this process's environment without FLEET_TOKEN: the
	// invoking operator's token is never handed to the long-running daemon.
	Env     []string
	Timeout time.Duration
}

// DaemonStartResult describes the daemon StartDaemon found or launched.
type DaemonStartResult struct {
	PID            int
	LogPath        string
	AlreadyRunning bool
	State          DaemonState
}

// DefaultDaemonArgs is the command line `fleet start` runs the daemon with.
// It carries no credentials.
func DefaultDaemonArgs(configDir string) []string {
	return []string{"--config-dir", configDir, "daemon"}
}

// daemonEnv returns env without FLEET_TOKEN.
func daemonEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "FLEET_TOKEN=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// StartDaemon launches the config directory's daemon detached from the
// terminal, with its output appended to DaemonLogPath, and waits until it
// accepts connections on its control address. It returns AlreadyRunning
// (without launching anything) when a daemon is already up.
func StartDaemon(opts DaemonStartOptions) (DaemonStartResult, error) {
	configDir := opts.ConfigDir
	logPath := DaemonLogPath(configDir)
	result := DaemonStartResult{LogPath: logPath}
	if state := DaemonStatus(configDir, opts.ControlAddress); state.Running {
		result.AlreadyRunning, result.PID, result.State = true, state.PID, state
		return result, nil
	} else if state.StalePIDFile {
		_ = os.Remove(state.PIDFile)
	}
	if strings.TrimSpace(opts.ControlAddress) == "" {
		return result, fmt.Errorf("runtime.control_address is not set; the daemon cannot start without it")
	}

	exe := opts.Executable
	if exe == "" {
		self, err := os.Executable()
		if err != nil {
			return result, fmt.Errorf("locate the fleet binary: %w", err)
		}
		exe = self
	}
	args := opts.Args
	if args == nil {
		args = DefaultDaemonArgs(configDir)
	}
	env := opts.Env
	if env == nil {
		env = daemonEnv(os.Environ())
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DaemonStartTimeout
	}

	logFile, offset, err := openDaemonLog(logPath)
	if err != nil {
		return result, err
	}
	fmt.Fprintf(logFile, "=== fleet start: launching daemon at %s ===\n", time.Now().UTC().Format(time.RFC3339))
	cmd := exec.Command(exe, args...) // #nosec G204 -- this binary (or a test helper) with fixed, credential-free arguments
	cmd.Dir = configDir
	cmd.Env = env
	cmd.Stdin = nil // /dev/null
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachedProcAttr()
	err = cmd.Start()
	_ = logFile.Close() // the daemon holds its own descriptor
	if err != nil {
		return result, fmt.Errorf("launch fleet daemon: %w", err)
	}
	result.PID = cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-exited:
			status := "exited"
			if err != nil {
				status = err.Error()
			}
			return result, fmt.Errorf("fleet daemon failed to start (%s)%s", status, daemonLogExcerpt(logPath, offset))
		case <-deadline.C:
			return result, fmt.Errorf("fleet daemon (pid %d) did not start listening on %s within %s; it may still be starting — check `fleet status`%s",
				result.PID, opts.ControlAddress, timeout, daemonLogExcerpt(logPath, offset))
		case <-tick.C:
			if pid, err := ReadDaemonPID(configDir); err == nil && pid == result.PID && controlAddressAnswers(opts.ControlAddress) {
				result.State = DaemonStatus(configDir, opts.ControlAddress)
				return result, nil
			}
		}
	}
}

// openDaemonLog opens the daemon log for appending (owner-only), rotating it
// first when it has grown large, and returns the offset new output starts at.
func openDaemonLog(path string) (*os.File, int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, 0, fmt.Errorf("create log directory: %w", err)
	}
	if info, err := os.Stat(path); err == nil && info.Size() > daemonLogRotateBytes {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) // #nosec G304 -- fixed file inside the controller's own config dir
	if err != nil {
		return nil, 0, fmt.Errorf("open daemon log %s: %w", path, err)
	}
	_ = f.Chmod(0o600) // tighten a log created by an older release
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("open daemon log %s: %w", path, err)
	}
	return f, info.Size(), nil
}

// daemonLogExcerpt returns the last lines the daemon wrote after offset,
// formatted to follow an error message, or "" when there are none.
func daemonLogExcerpt(path string, offset int64) string {
	lines := tailLogLines(path, offset, daemonLogTailLines)
	if len(lines) == 0 {
		return fmt.Sprintf("; logs: %s", path)
	}
	return fmt.Sprintf("; last lines of %s:\n  %s", path, strings.Join(lines, "\n  "))
}

// tailLogLines returns up to n trailing lines of path written after offset.
func tailLogLines(path string, offset int64, n int) []string {
	f, err := os.Open(path) // #nosec G304 -- fixed file inside the controller's own config dir
	if err != nil {
		return nil
	}
	defer f.Close()
	const maxRead = 64 << 10
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	start := max(offset, info.Size()-maxRead, 0)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRead))
	if err != nil {
		return nil
	}
	var lines []string
	for _, line := range bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n")) {
		text := strings.TrimRight(string(line), "\r")
		if strings.HasPrefix(text, "=== fleet start:") || strings.TrimSpace(text) == "" {
			continue
		}
		lines = append(lines, text)
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// DaemonStopResult describes what StopDaemon did.
type DaemonStopResult struct {
	PID                 int
	WasRunning          bool
	RemovedStalePIDFile bool
}

// StopDaemon asks the config directory's daemon to exit (SIGTERM; on Windows
// the process is terminated) and waits up to timeout for it. It only signals
// the process that holds the daemon lock and whose pid file names it, so a pid
// recycled by the system is never touched. A daemon that is not running is
// not an error; a stale pid file is removed.
func StopDaemon(configDir, controlAddress string, timeout time.Duration) (DaemonStopResult, error) {
	var result DaemonStopResult
	if timeout <= 0 {
		timeout = DaemonStopTimeout
	}
	pid, pidErr := ReadDaemonPID(configDir)
	if !daemonLockHeld(configDir) {
		if _, err := os.Stat(DaemonPIDPath(configDir)); err == nil {
			result.RemovedStalePIDFile = os.Remove(DaemonPIDPath(configDir)) == nil
		}
		if controlAddressAnswers(controlAddress) {
			return result, fmt.Errorf("a fleet daemon is answering on %s but did not record a pid file (started by an older release, or its config dir differs); stop that process manually", controlAddress)
		}
		return result, nil
	}
	switch {
	case pidErr != nil:
		return result, fmt.Errorf("a fleet daemon holds %s but its pid file is unreadable (%v); stop that process manually", configDir, pidErr)
	case !processAlive(pid):
		return result, fmt.Errorf("a fleet daemon holds %s but its pid file names pid %d, which is not running; stop the daemon manually", configDir, pid)
	}
	if ok, why := processLooksLikeDaemon(pid); !ok {
		return result, fmt.Errorf("refusing to signal pid %d: %s", pid, why)
	}
	result.PID = pid
	result.WasRunning = true
	if err := terminateProcess(pid); err != nil {
		return result, fmt.Errorf("stop fleet daemon (pid %d): %w", pid, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for processAlive(pid) && daemonLockHeld(configDir) {
		select {
		case <-ctx.Done():
			return result, fmt.Errorf("fleet daemon (pid %d) did not exit within %s; it is still running", pid, timeout)
		case <-tick.C:
		}
	}
	removeDaemonPIDFileIf(configDir, pid)
	return result, nil
}

type daemonReadyKey struct{}

// WithDaemonReady returns a context that makes RunDaemon call ready once its
// listeners are bound and serving.
func WithDaemonReady(ctx context.Context, ready func()) context.Context {
	return context.WithValue(ctx, daemonReadyKey{}, ready)
}

func notifyDaemonReady(ctx context.Context) {
	if ready, ok := ctx.Value(daemonReadyKey{}).(func()); ok && ready != nil {
		ready()
	}
}
