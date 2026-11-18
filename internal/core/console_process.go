// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/version"
)

// A console in the background is `fleet console serve`, started detached by
// `fleet console --background` or by choosing "keep it running in the
// background" in the console panel. There is at most one per config
// directory: it holds data/console.lock for as long as it runs.
//
// What other processes can learn about it lives in two owner-only files in
// the data directory:
//
//   - console.json: its pid, address, limits and which views are on. Never a
//     credential.
//   - console.key: the launch key. `fleet console open` sends it to the
//     console to get a one-time code that opens a browser tab. It is not the
//     session token, which the console keeps in memory only, and is only
//     good for asking for those codes.
//
// Both sit next to the controller's own private keys, behind the same 0700
// directory: reading them takes the same access as reading the keys.
//
// It stops by itself after a while without use, and after a fixed time
// however much it is used (ConsoleLimits), so a forgotten console does not
// stay reachable for ever.

// ErrConsoleRunning reports that a console already runs in the background
// for the config directory.
var ErrConsoleRunning = errors.New("a fleet console is already running in the background")

// Background console limits: how long it may go unused, and how long it may
// run at most, with and without the Control or Terminal view on (either one
// lets the session run commands across the fleet, so it gets shorter limits).
const (
	ConsoleIdleLimit              = time.Hour
	ConsoleMaxLifetime            = 8 * time.Hour
	ConsoleIdleLimitPrivileged    = 30 * time.Minute
	ConsoleMaxLifetimePrivileged  = time.Hour
	consoleStartTimeout           = 15 * time.Second
	consoleLaunchKeyBytes         = 32
	consoleLogRotateBytes         = 1 << 20
	consoleStopTimeout            = 10 * time.Second
	consoleStateFile              = "console.json"
	consoleKeyFile                = "console.key"
	consoleLockFile               = "console.lock"
	consoleLogFile                = "console.log"
	consoleStatePattern           = ".console-state-*"
	consoleKeyPattern             = ".console-key-*"
	consoleHandoverTokenMaxLength = 256
)

// ConsoleLimits returns the idle and absolute limits for a background
// console with the given views on.
func ConsoleLimits(control, terminal bool) (idle, lifetime time.Duration) {
	if control || terminal {
		return ConsoleIdleLimitPrivileged, ConsoleMaxLifetimePrivileged
	}
	return ConsoleIdleLimit, ConsoleMaxLifetime
}

// ConsoleState is what `fleet console status` reports.
type ConsoleState struct {
	Running bool `json:"running"`
	// Starting is set while the console holds its lock but has not yet
	// published where it listens.
	Starting  bool      `json:"starting,omitempty"`
	PID       int       `json:"pid,omitempty"`
	Address   string    `json:"address,omitempty"` // http://127.0.0.1:9445/
	StartedAt time.Time `json:"started_at,omitzero"`
	// StopsBy is when it stops however much it is used; IdleLimit how long
	// it may go unused.
	StopsBy   time.Time `json:"stops_by,omitzero"`
	IdleLimit string    `json:"idle_limit,omitempty"`
	Control   bool      `json:"control"`
	Terminal  bool      `json:"terminal"`
	Version   string    `json:"version,omitempty"`
	LogPath   string    `json:"log,omitempty"`
}

// ConsoleLogPath is where a background console's output goes.
func ConsoleLogPath(configDir string) string {
	return filepath.Join(configDir, "data", consoleLogFile)
}

func consolePath(configDir, name string) string {
	return filepath.Join(configDir, "data", name)
}

// ConsoleInstance is a running background console's claim on its config
// directory.
type ConsoleInstance struct {
	configDir string
	pid       int
	lock      *os.File
}

// ClaimConsole makes the calling process the config directory's background
// console. It fails with ErrConsoleRunning when another one holds it.
func ClaimConsole(configDir string) (*ConsoleInstance, error) {
	if err := os.MkdirAll(filepath.Join(configDir, "data"), 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	lockPath := consolePath(configDir, consoleLockFile)
	lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- fixed file inside the controller's own config dir
	if err != nil {
		return nil, fmt.Errorf("open console lock %s: %w", lockPath, err)
	}
	acquired, err := tryAdvisoryFileLock(lock)
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("acquire console lock %s: %w", lockPath, err)
	}
	if !acquired {
		_ = lock.Close()
		return nil, ErrConsoleRunning
	}
	// Whatever a console that died without cleaning up left behind is stale.
	removeConsoleFiles(configDir)
	return &ConsoleInstance{configDir: configDir, pid: os.Getpid(), lock: lock}, nil
}

// NewConsoleLaunchKey returns a fresh random launch key.
func NewConsoleLaunchKey() (string, error) {
	var raw [consoleLaunchKeyBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate console launch key: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// Publish records where the console listens, and its launch key, for other
// processes. The key is written first, so a state file that names an
// address always has its key beside it.
func (c *ConsoleInstance) Publish(state ConsoleState, launchKey string) error {
	if c == nil || c.lock == nil {
		return errors.New("console is not claimed")
	}
	state.Running, state.Starting = true, false
	state.PID = c.pid
	if state.Version == "" {
		state.Version = version.Version
	}
	if err := writeFileAtomic(consolePath(c.configDir, consoleKeyFile), consoleKeyPattern, []byte(launchKey+"\n")); err != nil {
		return fmt.Errorf("write console launch key: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(consolePath(c.configDir, consoleStateFile), consoleStatePattern, append(data, '\n')); err != nil {
		return fmt.Errorf("write console state: %w", err)
	}
	return nil
}

// Release removes the console's files and drops its lock.
func (c *ConsoleInstance) Release() {
	if c == nil || c.lock == nil {
		return
	}
	if st, err := readConsoleStateFile(c.configDir); err != nil || st.PID == c.pid {
		removeConsoleFiles(c.configDir)
	}
	_ = c.lock.Close()
	c.lock = nil
}

func removeConsoleFiles(configDir string) {
	_ = os.Remove(consolePath(configDir, consoleKeyFile))
	_ = os.Remove(consolePath(configDir, consoleStateFile))
}

func readConsoleStateFile(configDir string) (ConsoleState, error) {
	var st ConsoleState
	data, err := os.ReadFile(consolePath(configDir, consoleStateFile)) // #nosec G304 -- fixed file inside the controller's own config dir
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("read console state: %w", err)
	}
	return st, nil
}

// consoleLockHeld reports whether a background console holds the config
// directory's console lock.
func consoleLockHeld(configDir string) bool {
	lock, err := os.OpenFile(consolePath(configDir, consoleLockFile), os.O_RDWR, 0) // #nosec G304 -- fixed file inside the controller's own config dir
	if err != nil {
		return false
	}
	defer lock.Close()
	acquired, err := tryAdvisoryFileLock(lock)
	if err != nil {
		return false
	}
	return !acquired
}

// ReadConsole reports the config directory's background console. A console
// is running only while it holds the lock: the state file alone, or
// something answering on its port, never counts, so files left by one that
// crashed (or a program that took its port afterwards) are not trusted.
func ReadConsole(configDir string) ConsoleState {
	if !consoleLockHeld(configDir) {
		return ConsoleState{}
	}
	st, err := readConsoleStateFile(configDir)
	if err != nil || st.Address == "" {
		return ConsoleState{Running: true, Starting: true}
	}
	if !processAlive(st.PID) {
		return ConsoleState{Running: true, Starting: true}
	}
	st.Running, st.Starting = true, false
	st.LogPath = ConsoleLogPath(configDir)
	return st
}

// ReadConsoleLaunchKey returns the running background console's launch key.
func ReadConsoleLaunchKey(configDir string) (string, error) {
	data, err := os.ReadFile(consolePath(configDir, consoleKeyFile)) // #nosec G304 -- fixed file inside the controller's own config dir
	if err != nil {
		return "", fmt.Errorf("read console launch key: %w", err)
	}
	key := strings.TrimSpace(string(data))
	if len(key) != 2*consoleLaunchKeyBytes {
		return "", errors.New("the console launch key file is damaged")
	}
	return key, nil
}

// ConsoleStartOptions controls StartConsole. Only ConfigDir is required.
type ConsoleStartOptions struct {
	ConfigDir string
	// Args follow `--config-dir <dir> console serve`.
	Args []string
	// HandoverToken, when set, is the session token of the console this one
	// replaces, so browser tabs it opened keep working. It is passed on the
	// new process's standard input, never its command line or environment.
	HandoverToken string
	// Executable defaults to this binary; Env to this process's environment
	// without FLEET_TOKEN.
	Executable string
	Env        []string
	Timeout    time.Duration
}

// StartConsole launches `fleet console serve` detached from the terminal and
// waits until it publishes its address.
func StartConsole(opts ConsoleStartOptions) (ConsoleState, error) {
	configDir := opts.ConfigDir
	if st := ReadConsole(configDir); st.Running {
		return st, ErrConsoleRunning
	}
	exe := opts.Executable
	if exe == "" {
		self, err := os.Executable()
		if err != nil {
			return ConsoleState{}, fmt.Errorf("locate the fleet binary: %w", err)
		}
		exe = self
	}
	args := append([]string{"--config-dir", configDir, "console", "serve"}, opts.Args...)
	if opts.HandoverToken != "" {
		if len(opts.HandoverToken) > consoleHandoverTokenMaxLength || strings.ContainsAny(opts.HandoverToken, "\r\n") {
			return ConsoleState{}, errors.New("invalid session token to hand over")
		}
		args = append(args, "--token-stdin")
	}
	env := opts.Env
	if env == nil {
		env = daemonEnv(os.Environ())
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = consoleStartTimeout
	}
	logPath := ConsoleLogPath(configDir)
	logFile, offset, err := openRotatingLog(logPath, consoleLogRotateBytes)
	if err != nil {
		return ConsoleState{}, err
	}
	fmt.Fprintf(logFile, "=== fleet console: starting in the background at %s ===\n", time.Now().UTC().Format(time.RFC3339))
	cmd := exec.Command(exe, args...) // #nosec G204 -- this binary with fixed, credential-free arguments
	cmd.Dir = configDir
	cmd.Env = env
	if opts.HandoverToken != "" {
		cmd.Stdin = strings.NewReader(opts.HandoverToken + "\n")
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachedProcAttr()
	err = cmd.Start()
	_ = logFile.Close()
	if err != nil {
		return ConsoleState{}, fmt.Errorf("start the background console: %w", err)
	}
	pid := cmd.Process.Pid
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
			return ConsoleState{}, fmt.Errorf("the background console failed to start (%s)%s", status, daemonLogExcerpt(logPath, offset))
		case <-deadline.C:
			return ConsoleState{}, fmt.Errorf("the background console (pid %d) did not start within %s; check `fleet console status`%s", pid, timeout, daemonLogExcerpt(logPath, offset))
		case <-tick.C:
			if st := ReadConsole(configDir); st.Running && !st.Starting && st.PID == pid {
				return st, nil
			}
		}
	}
}

// ConsoleStopResult describes what StopConsole did.
type ConsoleStopResult struct {
	WasRunning bool `json:"was_running"`
	PID        int  `json:"pid,omitempty"`
}

// StopConsole stops the config directory's background console and waits
// for it to exit. It only signals a process that holds the console lock and
// (where the OS shows it) runs `fleet console serve`.
func StopConsole(configDir string, timeout time.Duration) (ConsoleStopResult, error) {
	var result ConsoleStopResult
	if timeout <= 0 {
		timeout = consoleStopTimeout
	}
	if !consoleLockHeld(configDir) {
		removeConsoleFiles(configDir)
		return result, nil
	}
	st, err := readConsoleStateFile(configDir)
	if err != nil || st.PID <= 0 {
		return result, errors.New("a fleet console holds the console lock but has not said which process it is; try again in a moment")
	}
	if !processAlive(st.PID) {
		return result, fmt.Errorf("a fleet console holds the console lock but pid %d is not running; stop it manually", st.PID)
	}
	if ok, why := processLooksLikeFleet(st.PID, "console", "serve"); !ok {
		return result, fmt.Errorf("refusing to signal pid %d: %s", st.PID, why)
	}
	result.WasRunning, result.PID = true, st.PID
	if err := terminateProcess(st.PID); err != nil {
		return result, fmt.Errorf("stop the background console (pid %d): %w", st.PID, err)
	}
	// Wait for the lock, not just the process: on Windows a terminated
	// process lets go of its handles (and so the lock) a moment after it is
	// gone, and until then the console still looks like it is running.
	deadline := time.Now().Add(timeout)
	for consoleLockHeld(configDir) {
		if time.Now().After(deadline) {
			if processAlive(st.PID) {
				return result, fmt.Errorf("the background console (pid %d) did not exit within %s", st.PID, timeout)
			}
			return result, fmt.Errorf("the background console (pid %d) exited, but the console lock is still held after %s", st.PID, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
	removeConsoleFiles(configDir) // Windows ends it without a chance to clean up
	return result, nil
}
