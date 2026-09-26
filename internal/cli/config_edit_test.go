// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
)

// writeEditorScript returns a fake $EDITOR that runs the given shell body with
// the file to edit as $1.
func writeEditorScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "editor.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil { // #nosec G306 -- test helper must be executable
		t.Fatal(err)
	}
	return path
}

func TestConfigEditSavesOnlyValidConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a /bin/sh editor script")
	}
	configDir := newServerCommandTestConfig(t)
	configPath := core.ConfigPath(configDir)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	// An edit that breaks the TOML is refused and config.toml is untouched.
	t.Setenv("EDITOR", writeEditorScript(t, `printf 'this is = = not toml\n' >> "$1"`))
	_, err = runFleetIn(t, configDir, "config", "edit")
	if err == nil || !strings.Contains(err.Error(), "config.toml was not changed") {
		t.Fatalf("invalid edit: err = %v", err)
	}
	after, _ := os.ReadFile(configPath)
	if string(after) != string(before) {
		t.Fatal("config.toml changed after an invalid edit")
	}
	drafts, _ := filepath.Glob(filepath.Join(configDir, ".config.edit-*.toml"))
	if len(drafts) != 1 {
		t.Fatalf("want the rejected draft kept for the operator, got %v", drafts)
	}
	_ = os.Remove(drafts[0])

	// An edit that fails validation (not just parsing) is refused too.
	t.Setenv("EDITOR", writeEditorScript(t, `sed -i.bak 's/^ *policy = .*/  policy = ""/' "$1" && rm -f "$1.bak"`))
	if _, err := runFleetIn(t, configDir, "config", "edit"); err == nil {
		t.Fatal("edit that empties the update policy was saved")
	}
	drafts, _ = filepath.Glob(filepath.Join(configDir, ".config.edit-*.toml"))
	for _, d := range drafts {
		_ = os.Remove(d)
	}

	// No change: nothing written.
	t.Setenv("EDITOR", writeEditorScript(t, `true`))
	out, err := runFleetIn(t, configDir, "config", "edit")
	if err != nil || !strings.Contains(out, "config unchanged") {
		t.Fatalf("no-op edit: out=%q err=%v", out, err)
	}

	// A valid edit is saved in place.
	t.Setenv("EDITOR", writeEditorScript(t, `sed -i.bak 's/channel = "stable"/channel = "beta"/' "$1" && rm -f "$1.bak"`))
	if out, err := runFleetIn(t, configDir, "config", "edit"); err != nil {
		t.Fatalf("valid edit: %v\n%s", err, out)
	}
	cfg, err := core.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Updates.Channel != "beta" {
		t.Fatalf("channel = %q, want beta", cfg.Updates.Channel)
	}
	if drafts, _ := filepath.Glob(filepath.Join(configDir, ".config.edit-*")); len(drafts) != 0 {
		t.Fatalf("drafts left behind: %v", drafts)
	}
	if info, err := os.Stat(configPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config.toml mode = %v (err %v), want 0600", info.Mode().Perm(), err)
	}
}
