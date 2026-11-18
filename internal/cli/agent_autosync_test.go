// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
)

// newAutoSyncTestConfig initializes a controller in a temp dir and pins
// FLEET_CONFIG_DIR to it, so a command that doesn't honour --config-dir
// (cobra's __complete) can never fall back to the machine's real config. It
// pretends this is a release build. Not parallel-safe: it swaps package
// globals.
func newAutoSyncTestConfig(t *testing.T) string {
	t.Helper()
	configDir := newServerCommandTestConfig(t)
	t.Setenv("FLEET_CONFIG_DIR", configDir)
	origSupported := agentAutoSyncSupported
	t.Cleanup(func() { agentAutoSyncSupported = origSupported })
	agentAutoSyncSupported = func() bool { return true }
	t.Setenv("FLEET_AGENT_AUTOSYNC", "")
	t.Setenv("FLEET_TOKEN", "")
	return configDir
}

func TestSyncAgentAutoOffAndOn(t *testing.T) {
	configDir := newAutoSyncTestConfig(t)
	launches := fakeHelperLaunches(t)

	out, err := runFleetIn(t, configDir, "sync-agent", "auto", "off")
	if err != nil || !strings.Contains(out, "Agent auto-sync is off") {
		t.Fatalf("auto off: %q, %v", out, err)
	}
	cfg, err := core.LoadConfig(core.ConfigPath(configDir))
	if err != nil || cfg.Updates.AgentAutoSyncEnabled() {
		t.Fatalf("auto off not stored: %v", err)
	}
	out, err = runFleetIn(t, configDir, "sync-agent", "auto")
	if err != nil || !strings.Contains(out, "Agent auto-sync:  off") {
		t.Fatalf("auto status: %q, %v", out, err)
	}

	if out, err := runFleetIn(t, configDir, "config", "set", "agent-auto-sync", "on"); err != nil {
		t.Fatalf("config set: %q, %v", out, err)
	}
	out, err = runFleetIn(t, configDir, "sync-agent", "auto", "status")
	if err != nil || !strings.Contains(out, "Agent auto-sync:  on") || !strings.Contains(out, "background helper") {
		t.Fatalf("auto status after turning it on: %q, %v", out, err)
	}

	// Ordinary commands no longer launch a sync of their own, nor print one.
	if strings.Contains(out, "Syncing fleet agents") {
		t.Fatalf("a command printed the old sync notice: %q", out)
	}
	if *launches == 0 {
		t.Fatal("no command started the background helper")
	}

	if _, err := runFleetIn(t, configDir, "sync-agent", "auto", "sideways"); err == nil {
		t.Fatal("accepted an unknown argument")
	}
}
