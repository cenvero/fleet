// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cenvero/fleet/internal/transport"
)

// The background helper's tasks (see helperRegistry in helper.go).

// Job tracking schedule. Variables so tests can shorten them.
var (
	// jobProbeFirst is how long after a job starts it is first probed, and
	// the gap after a probe that found it running; each such probe grows the
	// gap by half, up to jobProbeMax.
	jobProbeFirst = 10 * time.Second
	jobProbeMax   = time.Minute
	// serverRetryFirst is the wait after a probe could not reach a server;
	// it doubles with each further failure, up to serverRetryMax. Every job
	// on that server waits with it.
	serverRetryFirst = 15 * time.Second
	serverRetryMax   = 5 * time.Minute
	// reverseRetry is how long jobs on reverse-mode servers wait when no
	// daemon is running to reach them.
	reverseRetry = time.Minute
)

const (
	// jobProbesPerRound caps the probes in one run, so many overdue jobs
	// (after the machine slept) are spread over several runs.
	jobProbesPerRound = 8
	// jobProbeTimeout bounds one probe.
	jobProbeTimeout = 30 * time.Second
)

// jobTracker is the "jobs" task: it probes running jobs with Check, so they
// are marked done (firing "job-failed" for a failure) or lost without anyone
// polling them. It keeps per-job and per-server backoff in memory.
type jobTracker struct {
	mu      sync.Mutex
	jobs    map[int]*jobWatch
	servers map[string]*serverWatch
}

type jobWatch struct {
	next  time.Time
	delay time.Duration
}

type serverWatch struct {
	next  time.Time
	delay time.Duration
	err   string // why it is waiting: an error, or "" for a reverse-mode server without a daemon
}

func newJobTracker() *jobTracker {
	return &jobTracker{jobs: map[int]*jobWatch{}, servers: map[string]*serverWatch{}}
}

// dueLocked reports whether job j should be probed at now.
func (t *jobTracker) dueLocked(j JobRecord, now time.Time) bool {
	if s := t.servers[j.Server]; s != nil && now.Before(s.next) {
		return false
	}
	w := t.jobs[j.ID]
	if w == nil {
		w = &jobWatch{next: j.Started.Round(0).Add(jobProbeFirst), delay: jobProbeFirst}
		t.jobs[j.ID] = w
	}
	return !now.Before(w.next)
}

// pending reports whether any running job is due for a probe, reading only
// jobs.json. It also forgets jobs that are no longer running.
func (t *jobTracker) pending(configDir string, now time.Time) bool {
	jobs, err := NewJobStore(configDir).List()
	if err != nil {
		return true // let run report it
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	running := map[int]bool{}
	due := false
	for _, j := range jobs {
		if j.Status != JobRunning {
			continue
		}
		running[j.ID] = true
		if t.dueLocked(j, now) {
			due = true
		}
	}
	for id := range t.jobs {
		if !running[id] {
			delete(t.jobs, id)
		}
	}
	return due
}

func (t *jobTracker) run(ctx context.Context, app *App) error {
	store := app.Jobs()
	jobs, err := store.List()
	if err != nil {
		return err
	}
	// Oldest first, so a backlog is worked through in order.
	sort.Slice(jobs, func(i, k int) bool { return jobs[i].ID < jobs[k].ID })

	now := wallNow()
	var reverseOK *bool
	reverseReachable := func() bool {
		if reverseOK == nil {
			ok := daemonLockHeld(app.ConfigDir) || controlAddressAnswers(app.Config.Runtime.ControlAddress)
			reverseOK = &ok
		}
		return *reverseOK
	}

	running, probed, finished, lost, waitingReverse := 0, 0, 0, 0, 0
	for _, j := range jobs {
		if j.Status != JobRunning {
			continue
		}
		running++
		t.mu.Lock()
		due := t.dueLocked(j, now)
		t.mu.Unlock()
		if !due || probed >= jobProbesPerRound {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		server, gerr := app.GetServer(j.Server)
		if gerr == nil && server.Mode == transport.ModeReverse && !reverseReachable() {
			t.serverWait(j.Server, now, reverseRetry, "")
			waitingReverse++
			continue
		}
		probed++
		probeCtx, cancel := context.WithTimeout(ctx, jobProbeTimeout)
		rec, cerr := store.Check(app.jobExec(probeCtx), j.ID)
		cancel()
		if cerr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			t.serverFailed(j.Server, now, cerr)
			continue
		}
		t.serverOK(j.Server)
		switch rec.Status {
		case JobDone:
			finished++
		case JobLost:
			lost++
		default:
			t.jobBackoff(j.ID, now)
		}
	}

	parts := []string{fmt.Sprintf("%d running job(s)", running-finished-lost)}
	if finished > 0 {
		parts = append(parts, fmt.Sprintf("%d just finished", finished))
	}
	if lost > 0 {
		parts = append(parts, fmt.Sprintf("%d lost", lost))
	}
	if waitingReverse > 0 {
		parts = append(parts, fmt.Sprintf("%d on reverse-mode servers wait for the daemon (fleet start)", waitingReverse))
	}
	helperSay(ctx, "%s", strings.Join(parts, ", "))
	if jobs, err := store.List(); err == nil {
		t.forgetServersWithout(jobs)
	}
	return t.unreachableError()
}

func (t *jobTracker) jobBackoff(id int, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.jobs[id]
	if w == nil {
		w = &jobWatch{delay: jobProbeFirst}
		t.jobs[id] = w
	}
	w.next = now.Add(w.delay)
	w.delay = min(w.delay+w.delay/2, jobProbeMax)
}

func (t *jobTracker) serverWait(name string, now time.Time, d time.Duration, why string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.servers[name] = &serverWatch{next: now.Add(d), delay: d, err: why}
}

func (t *jobTracker) serverFailed(name string, now time.Time, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.servers[name]
	delay := serverRetryFirst
	if s != nil && s.err != "" {
		delay = min(s.delay*2, serverRetryMax)
	}
	t.servers[name] = &serverWatch{next: now.Add(delay), delay: delay, err: err.Error()}
}

func (t *jobTracker) serverOK(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.servers, name)
}

// unreachableError describes the servers whose jobs are waiting after a
// failed probe, or returns nil.
func (t *jobTracker) unreachableError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	var names []string
	for name, s := range t.servers {
		if s.err != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return fmt.Errorf("could not check jobs on %s (%s); retrying with backoff up to every %s",
		strings.Join(names, ", "), t.servers[names[0]].err, serverRetryMax)
}

// forgetServersWithout drops backoff for servers that have no running job.
func (t *jobTracker) forgetServersWithout(jobs []JobRecord) {
	active := map[string]bool{}
	for _, j := range jobs {
		if j.Status == JobRunning {
			active[j.Server] = true
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for name := range t.servers {
		if !active[name] {
			delete(t.servers, name)
		}
	}
}

// jobExec adapts ExecCommandContext to the job engine's ExecFunc.
func (a *App) jobExec(ctx context.Context) ExecFunc {
	return func(server, command string) (string, int, error) {
		res, err := a.ExecCommandContext(ctx, server, command)
		if err != nil {
			return "", 0, err
		}
		return res.Stdout, res.ExitCode, nil
	}
}

// agentAutoSyncEnvOff reports whether FLEET_AGENT_AUTOSYNC turns automatic
// agent sync off for this process (for the helper: the environment of the
// command that started it).
func agentAutoSyncEnvOff() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FLEET_AGENT_AUTOSYNC"))) {
	case "off", "false", "no", "0", "disable", "disabled":
		return true
	}
	return false
}

// helperAgentSync is the "agent-sync" task: the hourly automatic agent sync,
// on the schedule, lock, state and log it shares with the daemon and manual
// runs (data/agent-sync.*, logs/agent-sync.log).
func helperAgentSync(ctx context.Context, app *App) error {
	switch {
	case !AgentAutoSyncSupported():
		helperSay(ctx, "not available in a development build")
		return nil
	case agentAutoSyncEnvOff():
		helperSay(ctx, "off: the helper was started with FLEET_AGENT_AUTOSYNC=off")
		return nil
	case !app.Config.Updates.AgentAutoSyncEnabled():
		helperSay(ctx, "off (turn on: fleet sync-agent auto on)")
		return nil
	case daemonLockHeld(app.ConfigDir):
		helperSay(ctx, "left to the daemon (fleet start), which runs its own agent sync")
		return nil
	}
	now := time.Now()
	previous := ReadAgentSyncState(app.ConfigDir)
	if !AgentAutoSyncDue(previous, now) {
		helperSay(ctx, "next run due %s", previous.LastStartedAt.Add(AgentAutoSyncInterval).UTC().Format(time.RFC3339))
		return nil
	}
	if !ClaimAgentAutoSync(app.ConfigDir, "helper", now) {
		helperSay(ctx, "another fleet process started this run")
		return nil
	}
	logFile, _, err := openRotatingLog(AgentSyncLogPath(app.ConfigDir), agentSyncLogRotateBytes)
	if err != nil {
		return err
	}
	defer logFile.Close()
	result, skipped, err := app.runAutomaticAgentSync(ctx, logFile, "helper")
	switch {
	case skipped != "":
		helperSay(ctx, "skipped: %s", skipped)
		return nil
	case err != nil:
		return fmt.Errorf("agent sync failed: %w (log: %s)", err, AgentSyncLogPath(app.ConfigDir))
	case result.Failed > 0:
		names := strings.Join(ReadAgentSyncState(app.ConfigDir).LastFailedNames, ", ")
		if names == "" {
			names = "see the log"
		}
		return fmt.Errorf("the agent sync failed for %d server(s): %s; it is retried in an hour (details: %s)",
			result.Failed, names, AgentSyncLogPath(app.ConfigDir))
	}
	summary := fmt.Sprintf("%d updated/delivered, %d up to date", result.Synced, result.AlreadyUpToDate)
	if previous.LastStartedAt.IsZero() {
		summary = "first run: agents are now kept on this controller's version every hour; " + summary
	}
	helperSay(ctx, "%s", summary)
	return nil
}

// helperPruneJobLogs is the "job-log-prune" task: PruneJobLogs, daily.
func helperPruneJobLogs(ctx context.Context, app *App) error {
	retention := app.Config.JobLogRetentionDuration()
	if retention <= 0 {
		helperSay(ctx, "off (job-log-retention is 0)")
		return nil
	}
	if daemonLockHeld(app.ConfigDir) {
		helperSay(ctx, "left to the daemon (fleet start), which prunes hourly")
		return nil
	}
	pruneCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	removed, err := app.PruneJobLogs(pruneCtx)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		helperSay(ctx, "removed %d job record(s); the orphan sweep ran out of time and continues next run", removed)
		return nil
	}
	if err != nil {
		return err
	}
	helperSay(ctx, "removed %d job record(s) older than %s", removed, retention)
	return nil
}
