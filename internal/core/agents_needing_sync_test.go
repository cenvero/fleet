// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"path/filepath"
	"testing"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/internal/version"
)

// TestAgentsNeedingSync verifies that AgentsNeedingSync reports exactly the
// managed servers whose last-observed agent version differs (canonically) from
// the controller, skipping never-seen agents and going silent on a dev build.
//
// NOT t.Parallel: it mutates the package-global version.Version. Keeping it
// serial guarantees no t.Parallel test reads that global concurrently.
func TestAgentsNeedingSync(t *testing.T) {
	orig := version.Version
	version.Version = "v2.2.1"
	defer func() { version.Version = orig }()

	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeDirect,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer app.Close()

	add := func(name, ver string) {
		t.Helper()
		if err := app.AddServer(ServerRecord{
			Name: name, Address: "127.0.0.1", Port: 2301, Mode: transport.ModeDirect, User: "cenvero-agent",
			Agent:    AgentInstall{Managed: true, ServiceName: "cenvero-fleet-agent"},
			Observed: ServerObservation{AgentVersion: ver},
		}); err != nil {
			t.Fatalf("AddServer(%s): %v", name, err)
		}
	}
	add("in-sync", "v2.2.1")  // exact match → not stale
	add("canonical", "2.2.1") // matches after canonicalization → not stale
	add("stale", "v2.1.0")    // differs → stale
	add("never-seen", "")     // unknown version → skipped

	stale, err := app.AgentsNeedingSync()
	if err != nil {
		t.Fatalf("AgentsNeedingSync: %v", err)
	}
	if len(stale) != 1 || stale[0].Server != "stale" || stale[0].AgentVersion != "v2.1.0" {
		t.Fatalf("want only [stale@v2.1.0], got %+v", stale)
	}

	// A dev controller treats every comparison as noise and reports none.
	version.Version = "dev"
	if got, _ := app.AgentsNeedingSync(); len(got) != 0 {
		t.Fatalf("dev controller should report none, got %+v", got)
	}
}
