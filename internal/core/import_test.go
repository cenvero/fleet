// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
)

func newImportTestApp(t *testing.T) *App {
	t.Helper()
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeDirect,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { app.Close() })
	return app
}

// A server name becomes a path under servers/, so an import file (or any other
// caller) must not be able to name a server "../config" and overwrite
// config.toml, and a refused import must change nothing at all.
func TestImportRefusesUnsafeServerNames(t *testing.T) {
	app := newImportTestApp(t)
	configPath := ConfigPath(app.ConfigDir)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	export, err := app.Export()
	if err != nil {
		t.Fatal(err)
	}
	export.Config.Alias = "changed"

	for name, servers := range map[string][]ServerRecord{
		"traversal": {{Name: "web-01", Address: "10.0.0.1", Mode: transport.ModeDirect}, {Name: "../config", Address: "10.0.0.2", Mode: transport.ModeDirect}},
		"duplicate": {{Name: "web-01", Address: "10.0.0.1", Mode: transport.ModeDirect}, {Name: "web-01", Address: "10.0.0.3", Mode: transport.ModeDirect}},
		"slash":     {{Name: "sub/dir", Address: "10.0.0.4", Mode: transport.ModeDirect}},
	} {
		export.Servers = servers
		if err := app.Import(export); err == nil {
			t.Fatalf("%s: import accepted", name)
		}
		after, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("%s: a refused import changed config.toml", name)
		}
		if _, err := os.Stat(filepath.Join(app.ConfigDir, "servers", "web-01.toml")); !os.IsNotExist(err) {
			t.Fatalf("%s: a refused import still wrote a server record (%v)", name, err)
		}
	}

	// The lower layers refuse unsafe names on their own too.
	if err := app.SaveServer(ServerRecord{Name: "../config", Address: "10.0.0.2", Mode: transport.ModeDirect}); err == nil {
		t.Fatal("SaveServer accepted ../config")
	}
	if _, err := app.GetServer("../config"); err == nil {
		t.Fatal("GetServer accepted ../config")
	}

	// A well-formed import still works.
	export.Servers = []ServerRecord{{Name: "web-01", Address: "10.0.0.1", Mode: transport.ModeDirect}}
	if err := app.Import(export); err != nil {
		t.Fatalf("valid import: %v", err)
	}
	if got, err := app.GetServer("web-01"); err != nil || got.Address != "10.0.0.1" {
		t.Fatalf("imported server = %+v, %v", got, err)
	}
}
