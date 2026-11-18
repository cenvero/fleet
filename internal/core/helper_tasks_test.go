// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"os"
	"strings"
	"testing"
)

// runHelperTask runs one task func as the helper would and returns what it
// said about the run.
func runHelperTask(t *testing.T, task func(context.Context, *App) error, app *App) (string, error) {
	t.Helper()
	report := &helperReport{}
	err := task(context.WithValue(context.Background(), helperReportKey{}, report), app)
	return report.msg, err
}

// The helper's agent-sync task honours every way auto-sync is turned off,
// leaves the work to a running daemon, and otherwise runs the shared hourly
// sync once per interval.
func TestHelperAgentSyncTask(t *testing.T) {
	setControllerVersion(t, "v1.2.3")
	ta := newAgentSyncTestApp(t, map[string]string{"old": "v1.2.0", "current": "v1.2.3"})
	dir := ta.ConfigDir

	t.Setenv("FLEET_AGENT_AUTOSYNC", "off")
	if msg, err := runHelperTask(t, helperAgentSync, ta.App); err != nil || !strings.Contains(msg, "FLEET_AGENT_AUTOSYNC") {
		t.Fatalf("FLEET_AGENT_AUTOSYNC=off: %q, %v", msg, err)
	}
	t.Setenv("FLEET_AGENT_AUTOSYNC", "")

	off := false
	ta.Config.Updates.AgentAutoSync = &off // the runner re-reads config.toml for every run
	if msg, err := runHelperTask(t, helperAgentSync, ta.App); err != nil || !strings.Contains(msg, "sync-agent auto on") {
		t.Fatalf("auto-sync off: %q, %v", msg, err)
	}
	ta.Config.Updates.AgentAutoSync = nil

	daemon, err := ClaimDaemon(dir)
	if err != nil {
		t.Fatal(err)
	}
	if msg, err := runHelperTask(t, helperAgentSync, ta.App); err != nil || !strings.Contains(msg, "daemon") {
		t.Fatalf("with a daemon: %q, %v", msg, err)
	}
	daemon.Release()
	if calls := ta.takeCalls(); len(calls) != 0 || !ReadAgentSyncState(dir).LastStartedAt.IsZero() {
		t.Fatalf("a skipped run touched agents %v or the schedule", calls)
	}

	msg, err := runHelperTask(t, helperAgentSync, ta.App)
	if err != nil || !strings.Contains(msg, "first run") {
		t.Fatalf("due run: %q, %v", msg, err)
	}
	if calls := ta.takeCalls(); len(calls) != 1 || calls[0] != "old" {
		t.Fatalf("synced %v, want only the older agent", calls)
	}
	state := ReadAgentSyncState(dir)
	if state.LastTrigger != "helper" || state.LastSynced != 1 {
		t.Fatalf("state after the run = %+v", state)
	}
	if data, err := os.ReadFile(AgentSyncLogPath(dir)); err != nil || !strings.Contains(string(data), "agent auto-sync done") {
		t.Fatalf("agent-sync.log: %q, %v", data, err)
	}

	// Within the hour: nothing, not even a claim.
	if msg, err := runHelperTask(t, helperAgentSync, ta.App); err != nil || !strings.Contains(msg, "next run due") || len(ta.takeCalls()) != 0 {
		t.Fatalf("second run within the hour: %q, %v", msg, err)
	}
}

func TestHelperJobLogPruneTask(t *testing.T) {
	configDir := newHelperTestConfig(t)
	app, err := Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	app.Config.Runtime.JobLogRetention = "0"
	if msg, err := runHelperTask(t, helperPruneJobLogs, app); err != nil || !strings.Contains(msg, "off") {
		t.Fatalf("retention off: %q, %v", msg, err)
	}
	app.Config.Runtime.JobLogRetention = "7d"
	daemon, err := ClaimDaemon(configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Release()
	if msg, err := runHelperTask(t, helperPruneJobLogs, app); err != nil || !strings.Contains(msg, "daemon") {
		t.Fatalf("with a daemon: %q, %v", msg, err)
	}
}
