// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// newAutoSyncTestConfig initializes a controller in a temp dir and pins
// FLEET_CONFIG_DIR to it, so a command that doesn't honour --config-dir
// (cobra's __complete) can never fall back to the machine's real config. It
// then pretends this is a release build and counts the background syncs a
// command would launch. Not parallel-safe: it swaps package globals.
func newAutoSyncTestConfig(t *testing.T) (string, *int) {
	t.Helper()
	configDir := newServerCommandTestConfig(t)
	t.Setenv("FLEET_CONFIG_DIR", configDir)
	origSupported, origStart, origTerm := agentAutoSyncSupported, startBackgroundAgentSync, stderrIsTerminal
	t.Cleanup(func() {
		agentAutoSyncSupported, startBackgroundAgentSync, stderrIsTerminal = origSupported, origStart, origTerm
	})
	launched := 0
	agentAutoSyncSupported = func() bool { return true }
	startBackgroundAgentSync = func(string) error { launched++; return nil }
	stderrIsTerminal = func() bool { return false }
	t.Setenv("FLEET_AGENT_AUTOSYNC", "")
	t.Setenv("FLEET_TOKEN", "")
	return configDir, &launched
}

func TestAnyCommandStartsBackgroundAgentSyncHourly(t *testing.T) {
	configDir, launched := newAutoSyncTestConfig(t)

	if _, err := runFleetIn(t, configDir, "server", "list"); err != nil {
		t.Fatal(err)
	}
	if *launched != 1 {
		t.Fatalf("launched %d background syncs, want 1", *launched)
	}
	// Within the hour, further commands don't start another.
	for _, args := range [][]string{{"server", "list"}, {"status"}, {"version"}} {
		if _, err := runFleetIn(t, configDir, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if *launched != 1 {
		t.Fatalf("launched %d background syncs within the hour, want 1", *launched)
	}

	// Once the last run is over an hour old, the next command starts one.
	if err := os.Remove(core.AgentSyncStatePath(configDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := runFleetIn(t, configDir, "version"); err != nil {
		t.Fatal(err)
	}
	if *launched != 2 {
		t.Fatalf("launched %d background syncs, want 2", *launched)
	}
}

func TestBackgroundAgentSyncSkips(t *testing.T) {
	configDir, launched := newAutoSyncTestConfig(t)
	due := func() {
		t.Helper()
		if err := os.Remove(core.AgentSyncStatePath(configDir)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}

	// The sync itself, shell completion and config-replacing commands.
	for _, args := range [][]string{{"sync-agent", "auto", "status"}, {"completion", "zsh"}, {"__complete", "server", ""}} {
		due()
		if _, err := runFleetIn(t, configDir, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if *launched != 0 {
		t.Fatalf("launched %d background syncs from skipped commands", *launched)
	}

	// FLEET_AGENT_AUTOSYNC=off for one shell.
	due()
	t.Setenv("FLEET_AGENT_AUTOSYNC", "off")
	if _, err := runFleetIn(t, configDir, "server", "list"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_AGENT_AUTOSYNC", "")
	if *launched != 0 {
		t.Fatal("launched a background sync with FLEET_AGENT_AUTOSYNC=off")
	}

	// A scoped --token must not be able to start a fleet-wide update.
	due()
	cmd := &cobra.Command{Use: "list"}
	(&cobra.Command{Use: "server"}).AddCommand(cmd)
	maybeStartBackgroundAgentSync(cmd, configDir, "some-token")
	if *launched != 0 {
		t.Fatal("launched a background sync for a token-authenticated command")
	}

	// Uninitialized controller.
	maybeStartBackgroundAgentSync(cmd, t.TempDir(), "")
	if *launched != 0 {
		t.Fatal("launched a background sync before init")
	}
}

func TestSyncAgentAutoOffAndOn(t *testing.T) {
	configDir, launched := newAutoSyncTestConfig(t)

	out, err := runFleetIn(t, configDir, "sync-agent", "auto", "off")
	if err != nil || !strings.Contains(out, "Agent auto-sync is off") {
		t.Fatalf("auto off: %q, %v", out, err)
	}
	if _, err := runFleetIn(t, configDir, "server", "list"); err != nil {
		t.Fatal(err)
	}
	if *launched != 0 {
		t.Fatal("launched a background sync while auto-sync is off")
	}
	out, err = runFleetIn(t, configDir, "sync-agent", "auto")
	if err != nil || !strings.Contains(out, "Agent auto-sync:  off") {
		t.Fatalf("auto status: %q, %v", out, err)
	}

	if out, err := runFleetIn(t, configDir, "config", "set", "agent-auto-sync", "on"); err != nil {
		t.Fatalf("config set: %q, %v", out, err)
	}
	if _, err := runFleetIn(t, configDir, "server", "list"); err != nil {
		t.Fatal(err)
	}
	if *launched != 1 {
		t.Fatalf("launched %d background syncs after turning auto-sync on, want 1", *launched)
	}

	if _, err := runFleetIn(t, configDir, "sync-agent", "auto", "sideways"); err == nil {
		t.Fatal("accepted an unknown argument")
	}
}
