// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// fakeHelperLaunches pretends this is a release build and replaces the helper
// launcher with a counter. A fake launch claims the helper lock in-process
// (released when the test ends), so later commands see a helper running; no
// process is ever started. Not parallel-safe: it swaps package globals.
func fakeHelperLaunches(t *testing.T) *int {
	t.Helper()
	origSupported, origLaunch := helperSupported, launchHelperProcess
	var held []*core.HelperInstance
	t.Cleanup(func() {
		helperSupported, launchHelperProcess = origSupported, origLaunch
		for _, h := range held {
			h.Release()
		}
	})
	launches := 0
	helperSupported = func() bool { return true }
	launchHelperProcess = func(configDir string) error {
		launches++
		inst, err := core.ClaimHelper(configDir)
		if err == nil {
			held = append(held, inst)
		}
		return err
	}
	t.Setenv("FLEET_TOKEN", "")
	return &launches
}

// fakeHelperStops counts StopHelper calls.
func fakeHelperStops(t *testing.T) *int {
	t.Helper()
	orig := stopHelperProcess
	t.Cleanup(func() { stopHelperProcess = orig })
	stops := 0
	stopHelperProcess = func(string, time.Duration) (bool, error) { stops++; return false, nil }
	return &stops
}

// leaf builds a cobra command at path below a fake root, for calling
// ensureHelper on commands the test can't (or shouldn't) run.
func leaf(path string) *cobra.Command {
	parent := &cobra.Command{Use: "fleet"}
	var cmd *cobra.Command
	for _, name := range strings.Fields(path) {
		cmd = &cobra.Command{Use: name}
		parent.AddCommand(cmd)
		parent = cmd
	}
	return cmd
}

func TestOrdinaryCommandsStartTheHelperOnce(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	launches := fakeHelperLaunches(t)

	out, err := runFleetIn(t, configDir, "server", "list")
	if err != nil {
		t.Fatal(err)
	}
	if *launches != 1 {
		t.Fatalf("launched %d helpers, want 1", *launches)
	}
	if strings.Contains(strings.ToLower(out), "helper") || strings.Contains(out, "agent") {
		t.Fatalf("starting the helper printed something: %q", out)
	}
	// With it running, other commands only probe its lock.
	for _, args := range [][]string{{"server", "list"}, {"status"}, {"version"}, {"jobs"}, {"daemon", "--help"}} {
		if _, err := runFleetIn(t, configDir, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if *launches != 1 {
		t.Fatalf("launched %d helpers while one was running, want 1", *launches)
	}

	// A launch failure is recorded, not printed, and doesn't fail the command.
	launchHelperProcess = func(string) error { return os.ErrPermission }
	dir2 := newServerCommandTestConfig(t)
	out, err = runFleetIn(t, dir2, "server", "list")
	if err != nil || strings.Contains(out, "permission") {
		t.Fatalf("a failed helper start leaked into the command: %q, %v", out, err)
	}
	if st, _ := core.ReadHelperStatus(dir2); !strings.Contains(st.LastLaunchError, "permission denied") {
		t.Fatalf("launch failure not recorded: %+v", st)
	}
}

func TestHelperNotStartedForSkippedCommandsTokensOrDevBuilds(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	t.Setenv("FLEET_CONFIG_DIR", configDir) // __complete ignores --config-dir
	launches := fakeHelperLaunches(t)
	fakeHelperStops(t)

	for _, args := range [][]string{{"completion", "zsh"}, {"__complete", "server", ""}, {"helper", "status"}} {
		if _, err := runFleetIn(t, configDir, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	for _, path := range []string{"init", "adjust-init", "backup", "config import", "update check", "update channel", core.HelperCommand, "helper restart"} {
		ensureHelper(leaf(path), configDir, "")
	}
	if *launches != 0 {
		t.Fatalf("skipped commands launched %d helper(s)", *launches)
	}

	// A --token or FLEET_TOKEN must not start fleet-wide work, even indirectly.
	ensureHelper(leaf("server list"), configDir, "some-token")
	t.Setenv("FLEET_TOKEN", "some-token")
	ensureHelper(leaf("server list"), configDir, "")
	t.Setenv("FLEET_TOKEN", "")
	if *launches != 0 {
		t.Fatal("a token-authenticated command started the helper")
	}

	// Before init, and in a development build.
	ensureHelper(leaf("server list"), t.TempDir(), "")
	helperSupported = func() bool { return false }
	ensureHelper(leaf("server list"), configDir, "")
	if *launches != 0 {
		t.Fatal("started a helper before init or in a development build")
	}
	helperSupported = func() bool { return true }
	ensureHelper(leaf("server list"), configDir, "")
	if *launches != 1 {
		t.Fatalf("an ordinary command after all that launched %d helpers, want 1", *launches)
	}
}

func TestControllerReplacingCommandsStopTheHelper(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	launches := fakeHelperLaunches(t)
	stops := fakeHelperStops(t)
	for _, path := range []string{"self-uninstall", "recover", "config restore", "update apply", "update rollback"} {
		ensureHelper(leaf(path), configDir, "")
	}
	if *stops != 5 || *launches != 0 {
		t.Fatalf("stops=%d launches=%d, want 5 and 0", *stops, *launches)
	}
	ensureHelper(leaf("update check"), configDir, "")
	if *stops != 5 {
		t.Fatal("`update check` stopped the helper")
	}
}

func TestHelperRestart(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	launches := fakeHelperLaunches(t)

	out, err := runFleetIn(t, configDir, "helper", "restart")
	if err != nil || !strings.Contains(out, "Background helper restarted (pid") {
		t.Fatalf("helper restart: %q, %v", out, err)
	}
	if *launches != 1 || !core.HelperRunning(configDir) {
		t.Fatalf("launches=%d running=%v", *launches, core.HelperRunning(configDir))
	}
}

// `helper restart` and `__helper` never run under a token — not even an
// unscoped one, which RBAC would let through.
func TestHelperCommandsRefuseTokens(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	launches := fakeHelperLaunches(t)
	// enforceToken records the verified token as the acting operator for the
	// rest of the process; don't let that leak into other tests.
	origOperator := actingOperator
	t.Cleanup(func() { actingOperator = origOperator })
	tok, err := core.NewTokenStore(configDir).Create(core.Token{Name: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, viaEnv := range []bool{false, true} {
		args := []string{"--token", tok.ID}
		if viaEnv {
			t.Setenv("FLEET_TOKEN", tok.ID)
			args = nil
		}
		if _, err := runFleetIn(t, configDir, append(args, "helper", "restart")...); err == nil || !strings.Contains(err.Error(), "token") {
			t.Fatalf("helper restart under a token (env=%v): %v", viaEnv, err)
		}
		if _, err := runFleetIn(t, configDir, append(args, core.HelperCommand)...); err == nil || !strings.Contains(err.Error(), "token") {
			t.Fatalf("__helper under a token (env=%v): %v", viaEnv, err)
		}
	}
	if *launches != 0 || core.HelperRunning(configDir) {
		t.Fatalf("a token started the helper: launches=%d", *launches)
	}
}

func TestHelperStatusCommand(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	fakeHelperLaunches(t)

	out, err := runFleetIn(t, configDir, "helper", "status")
	if err != nil || !strings.Contains(out, "Background helper: not running") {
		t.Fatalf("status with no helper: %q, %v", out, err)
	}

	// A status file as the helper writes it (see core.HelperStatus).
	now := time.Now().UTC()
	status := core.HelperStatus{
		Format: 1, PID: os.Getpid(), StartedAt: now.Add(-time.Hour), Version: "v9.9.9", UpdatedAt: now,
		Tasks: map[string]core.HelperTaskStatus{
			"jobs": {Interval: "5s", LastRunAt: now, NextRunAt: now.Add(5 * time.Second), LastResult: "ok", LastMessage: "2 running job(s)"},
			"agent-sync": {Interval: "5m0s", LastRunAt: now, LastResult: "error",
				LastError: "the agent sync failed for 1 server(s): web-03", LastErrorAt: now},
		},
	}
	data, _ := json.Marshal(status)
	if err := os.WriteFile(filepath.Join(configDir, "data", "helper-status.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	inst, err := core.ClaimHelper(configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Release()

	out, err = runFleetIn(t, configDir, "helper", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Background helper: running (pid", "up 1h", "v9.9.9",
		"jobs", "ok: 2 running job(s)", "agent-sync", "5m", "job-log-prune", "24h",
		"Last errors:", "web-03", "helper.log",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}

	out, err = runFleetIn(t, configDir, "helper", "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var state core.HelperState
	if err := json.Unmarshal([]byte(out), &state); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	if !state.Running || state.PID != os.Getpid() || len(state.Tasks) != 3 || state.Status.Tasks["agent-sync"].LastResult != "error" {
		t.Fatalf("status --json = %+v", state)
	}
}

// `fleet __helper` runs in-process here: it gets through its own pre-run
// without trying to start another helper, holds the lock, and returns nil when
// it exits by design (its config dir going away).
func TestHelperRunCommandRunsAndExitsCleanly(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	launches := fakeHelperLaunches(t)
	done := make(chan error, 1)
	go func() {
		_, err := runFleetIn(t, configDir, core.HelperCommand)
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !core.HelperRunning(configDir) {
		select {
		case err := <-done:
			t.Fatalf("__helper returned before taking its lock: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("__helper never took its lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if *launches != 0 {
		t.Fatalf("__helper's pre-run launched %d helper(s)", *launches)
	}
	if st, err := core.ReadHelperStatus(configDir); err != nil || st.PID != os.Getpid() || len(st.Tasks) != 3 {
		t.Fatalf("status of the running helper = %+v, %v", st, err)
	}
	if err := os.Remove(core.ConfigPath(configDir)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("__helper = %v, want nil after its config went away", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("__helper kept running without a config")
	}
	if core.HelperRunning(configDir) {
		t.Fatal("lock still held")
	}
}
