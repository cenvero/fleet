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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/version"
)

// Agent auto-sync keeps every managed agent on the controller's version without
// the operator having to remember `fleet sync-agent`. It is on by default and
// is turned off only explicitly (`fleet sync-agent auto off`, or
// `updates.agent_auto_sync = false`). Two things start a run:
//
//   - any `fleet` command, when the last run started more than
//     AgentAutoSyncInterval ago: the command claims the slot and launches a
//     detached `fleet sync-agent --background`, so it never waits on the sync;
//   - the daemon (`fleet start`), which checks the same schedule itself and also
//     syncs an agent that connects running an older version.
//
// Both share data/agent-sync.json, so between them a fleet is synced about once
// an hour no matter how it is used. A run lock (data/agent-sync.lock) keeps the
// background, daemon and manual runs from ever updating agents at the same time.

// AgentAutoSyncInterval is how often agents are synced automatically.
const AgentAutoSyncInterval = time.Hour

const (
	// agentAutoSyncRunTimeout bounds one automatic run, so a hung agent can't
	// keep the run lock (and the background process) forever.
	agentAutoSyncRunTimeout = 30 * time.Minute
	// agentSyncLogRotateBytes rotates agent-sync.log to agent-sync.log.1.
	agentSyncLogRotateBytes = 5 << 20
	// agentAutoSyncClockSkew lets a run whose recorded start lies this far in
	// the future (the clock was set back) count as due instead of never.
	agentAutoSyncClockSkew = 5 * time.Minute
)

// Daemon schedule, variables so tests can shorten them.
var (
	// agentAutoSyncStartDelay gives reverse agents time to reconnect and report
	// their versions before the daemon's first check.
	agentAutoSyncStartDelay = time.Minute
	// agentAutoSyncPollInterval is how often the daemon checks whether a run is
	// due. Checking more often than the interval means a run started by a CLI
	// command resets the daemon's schedule too.
	agentAutoSyncPollInterval = 5 * time.Minute
	// agentConnectSyncDebounce collects agents that connect close together
	// (a daemon restart, a network blip) into a single run.
	agentConnectSyncDebounce = 15 * time.Second
)

// ErrAgentSyncBusy reports that another agent sync holds the run lock.
var ErrAgentSyncBusy = errors.New("another agent sync is already running")

// ErrAgentAutoSyncOff reports that automatic agent sync is turned off.
var ErrAgentAutoSyncOff = errors.New("agent auto-sync is off (turn it on with `fleet sync-agent auto on`)")

// AgentAutoSyncEnabled reports whether automatic agent sync is on: it is unless
// the operator turned it off.
func (u UpdateConfig) AgentAutoSyncEnabled() bool {
	return u.AgentAutoSync == nil || *u.AgentAutoSync
}

// AgentAutoSyncSupported reports whether this build syncs agents automatically.
// A dev build has no release to sync agents to, so it never does.
func AgentAutoSyncSupported() bool {
	v := strings.TrimSpace(version.Version)
	return v != "" && v != "dev" && version.Canonical(v) != ""
}

// AgentSyncStatePath is where the auto-sync schedule and last result are kept.
func AgentSyncStatePath(configDir string) string {
	return filepath.Join(configDir, "data", "agent-sync.json")
}

// AgentSyncLogPath is where background agent syncs write their output.
func AgentSyncLogPath(configDir string) string {
	return filepath.Join(configDir, "logs", "agent-sync.log")
}

func agentSyncRunLockPath(configDir string) string {
	return filepath.Join(configDir, "data", "agent-sync.lock")
}

func agentSyncStateLockPath(configDir string) string {
	return filepath.Join(configDir, "data", "agent-sync.state.lock")
}

// AgentSyncState is data/agent-sync.json.
type AgentSyncState struct {
	// LastStartedAt is when the last run was claimed; the next automatic run is
	// due AgentAutoSyncInterval after it.
	LastStartedAt  time.Time `json:"last_started_at"`
	LastFinishedAt time.Time `json:"last_finished_at,omitempty"`
	// LastTrigger is what started the last run: "cli", "daemon", "connect" or
	// "manual".
	LastTrigger     string   `json:"last_trigger,omitempty"`
	LastSynced      int      `json:"last_synced"`
	LastUpToDate    int      `json:"last_up_to_date"`
	LastFailed      int      `json:"last_failed"`
	LastSkipped     int      `json:"last_skipped"`
	LastError       string   `json:"last_error,omitempty"`
	LastFailedNames []string `json:"last_failed_servers,omitempty"`
	// Pending maps a server whose new agent was delivered but can't be
	// activated unattended (Windows, macOS) to the controller version it was
	// delivered for. Automatic runs skip it until the controller version
	// changes, rather than re-delivering the same binary every hour; it is
	// dropped once the agent reports the new version.
	Pending map[string]string `json:"pending,omitempty"`
}

// ReadAgentSyncState returns the stored state, or a zero state when there is
// none (or it is unreadable — the next run rewrites it).
func ReadAgentSyncState(configDir string) AgentSyncState {
	var state AgentSyncState
	if data, err := os.ReadFile(AgentSyncStatePath(configDir)); err == nil { // #nosec G304 -- fixed file inside the controller's own config dir
		_ = json.Unmarshal(data, &state)
	}
	return state
}

// AgentAutoSyncDue reports whether the next automatic run is due at now.
func AgentAutoSyncDue(state AgentSyncState, now time.Time) bool {
	elapsed := now.Sub(state.LastStartedAt)
	return elapsed >= AgentAutoSyncInterval || elapsed < -agentAutoSyncClockSkew
}

// ClaimAgentAutoSync records a run as started now when one is due, and reports
// whether it did. The check and the write happen under a lock, so when several
// fleet commands (and the daemon) race, exactly one of them starts the run.
func ClaimAgentAutoSync(configDir, trigger string, now time.Time) bool {
	claimed := false
	_ = updateAgentSyncState(configDir, func(state *AgentSyncState) bool {
		if !AgentAutoSyncDue(*state, now) {
			return false
		}
		state.LastStartedAt = now.UTC()
		state.LastTrigger = trigger
		claimed = true
		return true
	})
	return claimed
}

// updateAgentSyncState applies fn to the stored state under the state lock and
// writes it back when fn reports a change.
func updateAgentSyncState(configDir string, fn func(*AgentSyncState) bool) error {
	return withAdvisoryFileLock(agentSyncStateLockPath(configDir), func() error {
		state := ReadAgentSyncState(configDir)
		if !fn(&state) {
			return nil
		}
		return writeAgentSyncState(configDir, state)
	})
}

// writeAgentSyncState replaces the state file atomically (temp file + rename).
func writeAgentSyncState(configDir string, state AgentSyncState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	path := AgentSyncStatePath(configDir)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".agent-sync-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// AgentSyncRunning reports whether an agent sync currently holds the run lock.
func AgentSyncRunning(configDir string) bool {
	file, err := os.OpenFile(agentSyncRunLockPath(configDir), os.O_RDWR, 0) // #nosec G304 -- fixed file inside the controller's own config dir
	if err != nil {
		return false
	}
	defer file.Close()
	acquired, err := tryAdvisoryFileLock(file)
	return err == nil && !acquired
}

// acquireAgentSyncLock takes the run lock. Without wait it fails with
// ErrAgentSyncBusy when another sync holds it; with wait it calls onWait once
// and blocks until the lock is free or ctx ends.
func acquireAgentSyncLock(ctx context.Context, configDir string, wait bool, onWait func()) (func(), error) {
	path := agentSyncRunLockPath(configDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- fixed file inside the controller's own config dir
	if err != nil {
		return nil, fmt.Errorf("open agent sync lock: %w", err)
	}
	waited := false
	for {
		acquired, err := tryAdvisoryFileLock(file)
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("acquire agent sync lock: %w", err)
		}
		if acquired {
			return func() { _ = file.Close() }, nil
		}
		if !wait {
			_ = file.Close()
			return nil, ErrAgentSyncBusy
		}
		if !waited && onWait != nil {
			onWait()
		}
		waited = true
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// AgentSyncOptions controls RunAgentSync.
type AgentSyncOptions struct {
	// Trigger is recorded in the state and audit log: "manual", "cli",
	// "daemon" or "connect".
	Trigger string
	// Auto applies the automatic-run rules: refuse when auto-sync is off, and
	// touch only agents known to be older than the controller (skipping
	// unobserved agents, agents awaiting activation of a delivered update, and
	// reverse agents this process can't reach).
	Auto bool
	// Wait blocks for a sync already in progress instead of failing with
	// ErrAgentSyncBusy; OnWait is called once if it has to.
	Wait   bool
	OnWait func()
	// Progress streams per-server status (see SyncAgent).
	Progress func(SyncAgentProgress)
}

// RunAgentSync is SyncAgent under the run lock, recording the result in the
// auto-sync state. A manual full-fleet run also restarts the automatic
// schedule, since it just did the same work.
func (a *App) RunAgentSync(ctx context.Context, serverNames []string, opts AgentSyncOptions) (FleetSyncAgentResult, error) {
	if opts.Auto {
		if !AgentAutoSyncSupported() {
			return FleetSyncAgentResult{}, fmt.Errorf("agent auto-sync is not available in a %q build", version.Version)
		}
		// Read the setting from disk: a long-running daemon must notice
		// `fleet sync-agent auto off` without a restart.
		if cfg, err := LoadConfig(ConfigPath(a.ConfigDir)); err == nil && !cfg.Updates.AgentAutoSyncEnabled() {
			return FleetSyncAgentResult{}, ErrAgentAutoSyncOff
		}
	}
	release, err := acquireAgentSyncLock(ctx, a.ConfigDir, opts.Wait, opts.OnWait)
	if err != nil {
		return FleetSyncAgentResult{}, err
	}
	defer release()

	started := time.Now().UTC()
	targets := serverNames
	skipped := 0
	pending := ReadAgentSyncState(a.ConfigDir).Pending
	if opts.Auto {
		targets, skipped, err = a.autoSyncTargets(serverNames, pending)
		if err != nil {
			a.recordAgentSync(opts, serverNames, started, FleetSyncAgentResult{}, 0, err)
			return FleetSyncAgentResult{}, err
		}
		if len(targets) == 0 {
			result := FleetSyncAgentResult{ControllerVersion: version.Canonical(version.Version), Agents: []SyncAgentResult{}}
			a.recordAgentSync(opts, serverNames, started, result, skipped, nil)
			return result, nil
		}
	}
	result, err := a.SyncAgent(ctx, targets, opts.Progress)
	a.recordAgentSync(opts, serverNames, started, result, skipped, err)
	return result, err
}

// autoSyncTargets picks the servers an automatic run updates, and counts the
// ones it leaves alone.
func (a *App) autoSyncTargets(serverNames []string, pending map[string]string) ([]string, int, error) {
	var servers []ServerRecord
	if len(serverNames) == 0 {
		all, err := a.ListServers()
		if err != nil {
			return nil, 0, err
		}
		servers = all
	}
	for _, name := range serverNames {
		// A queued server may have been removed since; skip it rather than
		// failing the run for the others.
		if server, err := a.GetServer(name); err == nil {
			servers = append(servers, server)
		}
	}
	want := version.Canonical(version.Version)
	// Reverse agents are reached through the daemon. Without one, this process
	// can't reach them, and trying would only log a failure every hour.
	_, isDaemon := daemonApps.Load(a)
	reverseReachable := isDaemon || a.ReverseRPCContext != nil || a.ReverseRPC != nil ||
		DaemonStatus(a.ConfigDir, a.Config.Runtime.ControlAddress).Running

	var targets []string
	skipped := 0
	for _, server := range servers {
		have := version.Canonical(strings.TrimSpace(server.Observed.AgentVersion))
		switch {
		case have == "" || !isNewerVersion(want, have):
			// Never observed (nothing known to fix), already current, or newer
			// than the controller (the controller is what needs updating).
			continue
		case pending[server.Name] == want:
			skipped++ // delivered; waiting for a restart to activate it
		case server.Mode == transport.ModeReverse && !reverseReachable:
			skipped++
		default:
			targets = append(targets, server.Name)
		}
	}
	return targets, skipped, nil
}

// recordAgentSync stores a run's outcome and audits it. Failing to record is
// not an error for the caller: the agents were synced either way.
func (a *App) recordAgentSync(opts AgentSyncOptions, serverNames []string, started time.Time, result FleetSyncAgentResult, skipped int, runErr error) {
	fullFleet := len(serverNames) == 0
	want := version.Canonical(version.Version)
	_ = updateAgentSyncState(a.ConfigDir, func(state *AgentSyncState) bool {
		if fullFleet {
			if opts.Trigger == "manual" && started.After(state.LastStartedAt) {
				state.LastStartedAt = started
			}
			if state.LastTrigger == "" || opts.Trigger == "manual" {
				state.LastTrigger = opts.Trigger
			}
			state.LastFinishedAt = time.Now().UTC()
			state.LastSynced, state.LastUpToDate = result.Synced, result.AlreadyUpToDate
			state.LastFailed, state.LastSkipped = result.Failed, skipped
			state.LastError = ""
			if runErr != nil {
				state.LastError = truncateUpdateError(runErr.Error())
			}
			state.LastFailedNames = nil
		}
		if state.Pending == nil {
			state.Pending = map[string]string{}
		}
		for _, r := range result.Agents {
			switch {
			case r.Error != "":
				if fullFleet {
					state.LastFailedNames = append(state.LastFailedNames, r.Server)
				}
			case r.ActivationPending:
				state.Pending[r.Server] = want
			default:
				delete(state.Pending, r.Server)
			}
		}
		a.prunePendingAgentSync(state.Pending)
		if len(state.Pending) == 0 {
			state.Pending = nil
		}
		return true
	})

	if len(result.Agents) == 0 && runErr == nil {
		return // nothing to do is not worth an audit entry every hour
	}
	details := fmt.Sprintf("trigger=%s synced=%d up_to_date=%d failed=%d skipped=%d",
		opts.Trigger, result.Synced, result.AlreadyUpToDate, result.Failed, skipped)
	if runErr != nil {
		details += " error=" + truncateUpdateError(runErr.Error())
	}
	operator := a.operator()
	if opts.Auto {
		operator = "system"
	}
	_ = a.AuditLog.Append(logs.AuditEntry{
		Action:   "agent.sync",
		Target:   "fleet",
		Operator: operator,
		Details:  details,
	})
}

// prunePendingAgentSync drops pending entries for servers that were removed or
// whose agent now reports the controller's version.
func (a *App) prunePendingAgentSync(pending map[string]string) {
	want := version.Canonical(version.Version)
	for name := range pending {
		server, err := a.GetServer(name)
		if err != nil {
			delete(pending, name)
			continue
		}
		have := version.Canonical(strings.TrimSpace(server.Observed.AgentVersion))
		if have != "" && !isNewerVersion(want, have) {
			delete(pending, name)
		}
	}
}

// AgentAutoSyncStatus is what `fleet sync-agent auto status` reports.
type AgentAutoSyncStatus struct {
	Enabled   bool           `json:"enabled"`
	Supported bool           `json:"supported"`
	Running   bool           `json:"running"`
	Interval  string         `json:"interval"`
	NextDueAt time.Time      `json:"next_due_at"`
	LogPath   string         `json:"log_path"`
	State     AgentSyncState `json:"state"`
}

// ReadAgentAutoSyncStatus gathers the auto-sync status for configDir.
func ReadAgentAutoSyncStatus(configDir string, cfg Config) AgentAutoSyncStatus {
	state := ReadAgentSyncState(configDir)
	next := state.LastStartedAt.Add(AgentAutoSyncInterval)
	if state.LastStartedAt.IsZero() {
		next = time.Time{} // due now
	}
	return AgentAutoSyncStatus{
		Enabled:   cfg.Updates.AgentAutoSyncEnabled(),
		Supported: AgentAutoSyncSupported(),
		Running:   AgentSyncRunning(configDir),
		Interval:  AgentAutoSyncInterval.String(),
		NextDueAt: next,
		LogPath:   AgentSyncLogPath(configDir),
		State:     state,
	}
}

// SetAgentAutoSync turns automatic agent sync on or off in config.toml.
func SetAgentAutoSync(configDir string, on bool) error {
	path := ConfigPath(configDir)
	cfg, err := LoadConfig(path)
	if err != nil {
		return err
	}
	cfg.Updates.AgentAutoSync = &on
	return SaveConfig(path, cfg)
}

// BackgroundAgentSyncArgs is the command line a background sync runs with. It
// carries no credentials.
func BackgroundAgentSyncArgs(configDir string) []string {
	return []string{"--config-dir", configDir, "sync-agent", "--background"}
}

// StartBackgroundAgentSync launches `fleet sync-agent --background` detached
// from the terminal, appending its output to AgentSyncLogPath, and returns
// without waiting for it. The invoking operator's FLEET_TOKEN is not passed on.
func StartBackgroundAgentSync(configDir string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the fleet binary: %w", err)
	}
	logFile, _, err := openRotatingLog(AgentSyncLogPath(configDir), agentSyncLogRotateBytes)
	if err != nil {
		return err
	}
	defer logFile.Close()                                           // the child holds its own descriptor
	cmd := exec.Command(exe, BackgroundAgentSyncArgs(configDir)...) // #nosec G204 -- this binary with fixed, credential-free arguments
	cmd.Dir = configDir
	cmd.Env = daemonEnv(os.Environ())
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachedProcAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch background agent sync: %w", err)
	}
	// Reap the child if this process outlives it (a long dashboard session);
	// otherwise it is simply re-parented when this process exits.
	go func() { _ = cmd.Wait() }()
	return nil
}

// RunBackgroundAgentSync is the body of `fleet sync-agent --background`: one
// automatic run with timestamped progress written to out (the sync log).
func (a *App) RunBackgroundAgentSync(ctx context.Context, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, agentAutoSyncRunTimeout)
	defer cancel()
	logf := func(format string, args ...any) {
		fmt.Fprintf(out, "%s "+format+"\n", append([]any{time.Now().UTC().Format(time.RFC3339)}, args...)...)
	}
	logf("agent auto-sync started (controller %s, pid %d)", version.Canonical(version.Version), os.Getpid())
	result, err := a.RunAgentSync(ctx, nil, AgentSyncOptions{
		Trigger:  "cli",
		Auto:     true,
		Progress: agentSyncProgressLogger(logf),
	})
	switch {
	case errors.Is(err, ErrAgentSyncBusy), errors.Is(err, ErrAgentAutoSyncOff):
		logf("agent auto-sync skipped: %v", err)
		return nil
	case err != nil:
		logf("agent auto-sync failed: %v", err)
		return err
	}
	logf("agent auto-sync done: %d updated/delivered, %d up to date, %d failed", result.Synced, result.AlreadyUpToDate, result.Failed)
	return nil
}

func agentSyncProgressLogger(logf func(string, ...any)) func(SyncAgentProgress) {
	return func(p SyncAgentProgress) {
		switch p.State {
		case "updated":
			logf("  %s: updated %s -> %s", p.Server, p.From, p.To)
		case "pending-activation":
			logf("  %s: delivered %s -> %s; restart the agent service to activate it", p.Server, p.From, p.To)
		case "uptodate":
			logf("  %s: up to date (%s)", p.Server, p.From)
		case "error":
			logf("  %s: failed: %s", p.Server, p.Err)
		}
	}
}

// openRotatingLog opens path for appending (owner-only), first moving it to
// path.1 when it is larger than rotateAt, and returns the offset new output
// starts at.
func openRotatingLog(path string, rotateAt int64) (*os.File, int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, 0, fmt.Errorf("create log directory: %w", err)
	}
	if info, err := os.Stat(path); err == nil && info.Size() > rotateAt {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) // #nosec G304 -- fixed file inside the controller's own config dir
	if err != nil {
		return nil, 0, fmt.Errorf("open log %s: %w", path, err)
	}
	_ = f.Chmod(0o600)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("open log %s: %w", path, err)
	}
	return f, info.Size(), nil
}

// agentConnectSyncQueue collects agents the daemon saw connect with an older
// version, for runAgentAutoSync to sync together.
type agentConnectSyncQueue struct {
	mu        sync.Mutex
	queued    map[string]struct{}
	lastQueue map[string]time.Time
	kick      chan struct{}
}

// queueConnectedAgentSync asks the daemon to sync an agent that just connected
// running agentVersion. It reports false — leaving the caller's own handling
// in place — when this App is not a daemon running auto-sync, auto-sync is
// off, or the agent is not older than the controller. Each server is queued at
// most once per AgentAutoSyncInterval, so an agent whose update keeps failing
// isn't retried on every reconnect.
func (a *App) queueConnectedAgentSync(serverName, agentVersion string) bool {
	q := a.agentSyncQueue()
	if q == nil {
		return false
	}
	want := version.Canonical(version.Version)
	have := version.Canonical(strings.TrimSpace(agentVersion))
	if have == "" || !isNewerVersion(want, have) {
		return false
	}
	// The setting on disk, not the daemon's startup copy: `fleet sync-agent
	// auto off` takes effect without a daemon restart.
	enabled := a.Config.Updates.AgentAutoSyncEnabled()
	if cfg, err := LoadConfig(ConfigPath(a.ConfigDir)); err == nil {
		enabled = cfg.Updates.AgentAutoSyncEnabled()
	}
	if !enabled {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if last, ok := q.lastQueue[serverName]; ok && time.Since(last) < AgentAutoSyncInterval {
		return true // handled: already synced (or tried) recently
	}
	q.lastQueue[serverName] = time.Now()
	q.queued[serverName] = struct{}{}
	select {
	case q.kick <- struct{}{}:
	default:
	}
	return true
}

func (a *App) agentSyncQueue() *agentConnectSyncQueue {
	a.agentSyncMu.Lock()
	defer a.agentSyncMu.Unlock()
	return a.agentSyncQ
}

// agentAutoSyncStopWait bounds how long a stopping daemon waits for an agent
// sync it cancelled to wind down.
const agentAutoSyncStopWait = 5 * time.Second

// startAgentAutoSync sets up the daemon's connect queue — before the daemon
// accepts agents, so none connects unnoticed — and starts runAgentAutoSync.
// The returned func stops the loop (cancelling a run in progress), waits
// briefly for it to exit, and clears the queue.
func (a *App) startAgentAutoSync(ctx context.Context) func() {
	if !AgentAutoSyncSupported() {
		return func() {}
	}
	q := &agentConnectSyncQueue{
		queued:    map[string]struct{}{},
		lastQueue: map[string]time.Time{},
		kick:      make(chan struct{}, 1),
	}
	a.agentSyncMu.Lock()
	a.agentSyncQ = q
	a.agentSyncMu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.runAgentAutoSync(ctx, q)
	}()
	return func() {
		a.agentSyncMu.Lock()
		a.agentSyncQ = nil
		a.agentSyncMu.Unlock()
		cancel()
		select {
		case <-done:
		case <-time.After(agentAutoSyncStopWait):
		}
	}
}

// runAgentAutoSync is the daemon's agent auto-sync loop: an hourly full-fleet
// run on the shared schedule, plus runs for agents that connect out of date.
func (a *App) runAgentAutoSync(ctx context.Context, q *agentConnectSyncQueue) {
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "%s agent auto-sync: "+format+"\n", append([]any{time.Now().UTC().Format(time.RFC3339)}, args...)...)
	}
	run := func(names []string, trigger string) {
		defer guardPanic("agent auto-sync")
		runCtx, cancel := context.WithTimeout(ctx, agentAutoSyncRunTimeout)
		defer cancel()
		result, err := a.RunAgentSync(runCtx, names, AgentSyncOptions{
			Trigger: trigger,
			Auto:    true,
			// A connect-triggered run waits out a sync in progress (which
			// started before this agent was back); the hourly one is skipped
			// if something else is already syncing.
			Wait:     trigger == "connect",
			Progress: agentSyncProgressLogger(logf),
		})
		switch {
		case errors.Is(err, ErrAgentSyncBusy), errors.Is(err, ErrAgentAutoSyncOff), errors.Is(err, context.Canceled):
		case err != nil:
			logf("%s run failed: %v", trigger, err)
		case len(result.Agents) > 0:
			logf("%s run: %d updated/delivered, %d up to date, %d failed", trigger, result.Synced, result.AlreadyUpToDate, result.Failed)
		}
	}
	checkSchedule := func() {
		cfg, err := LoadConfig(ConfigPath(a.ConfigDir))
		if err != nil || !cfg.Updates.AgentAutoSyncEnabled() {
			return
		}
		if ClaimAgentAutoSync(a.ConfigDir, "daemon", time.Now()) {
			run(nil, "daemon")
		}
	}

	poll := time.NewTimer(agentAutoSyncStartDelay)
	defer poll.Stop()
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			checkSchedule()
			poll.Reset(agentAutoSyncPollInterval)
		case <-q.kick:
			if debounce == nil {
				debounce = time.After(agentConnectSyncDebounce)
			}
		case <-debounce:
			debounce = nil
			q.mu.Lock()
			names := make([]string, 0, len(q.queued))
			for name := range q.queued {
				names = append(names, name)
			}
			q.queued = map[string]struct{}{}
			q.mu.Unlock()
			sort.Strings(names)
			if len(names) > 0 {
				run(names, "connect")
			}
		}
	}
}
