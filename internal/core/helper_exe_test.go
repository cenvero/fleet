// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The helper's copy of the binary matches it, is reused while the binary is
// unchanged, and is replaced (with the old copy removed) after an upgrade.
func TestHelperExecutableCopy(t *testing.T) {
	configDir := t.TempDir()
	src := filepath.Join(t.TempDir(), "fleet.exe")
	if err := os.WriteFile(src, []byte("v1 binary"), 0o700); err != nil { // #nosec G306 -- a stand-in executable
		t.Fatal(err)
	}
	first, err := helperExecutableCopy(configDir, src)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(first); string(data) != "v1 binary" {
		t.Fatalf("copy holds %q", data)
	}
	if !strings.HasPrefix(filepath.Base(first), helperCopyPrefix) || filepath.Ext(first) != ".exe" {
		t.Fatalf("copy named %q", first)
	}
	again, err := helperExecutableCopy(configDir, src)
	if err != nil || again != first {
		t.Fatalf("an unchanged binary was copied again: %q vs %q (%v)", again, first, err)
	}
	if err := os.WriteFile(src, []byte("v2 binary"), 0o700); err != nil { // #nosec G306 -- a stand-in executable
		t.Fatal(err)
	}
	second, err := helperExecutableCopy(configDir, src)
	if err != nil || second == first {
		t.Fatalf("an upgraded binary was not copied afresh: %q (%v)", second, err)
	}
	if data, _ := os.ReadFile(second); string(data) != "v2 binary" {
		t.Fatalf("new copy holds %q", data)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("the old copy was left behind (%v)", err)
	}
	// A copy that was tampered with is replaced, not run.
	if err := os.WriteFile(second, []byte("tampered"), 0o700); err != nil { // #nosec G306 -- a stand-in executable
		t.Fatal(err)
	}
	third, err := helperExecutableCopy(configDir, src)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(third); string(data) != "v2 binary" {
		t.Fatalf("a tampered copy was kept: %q", data)
	}
}

// Backups leave out the helper's copy of the binary and a console's launch
// key: one is a regenerable 40 MB file, the other a secret for a process
// that is gone by the time the backup is restored.
func TestBackupSkipsHelperCopyAndConsoleKey(t *testing.T) {
	for name, skip := range map[string]bool{
		"data/helper-bin/fleet-helper-0123456789abcdef.exe": true,
		"data/helper-bin/.helper-copy-123":                  true,
		"data/console.key":                                  true,
		"data/console.json":                                 false,
		"data/helper-status.json":                           false,
		"config.toml":                                       false,
	} {
		if got := skipBackupPattern(filepath.FromSlash(name)); got != skip {
			t.Errorf("skipBackupPattern(%s) = %v, want %v", name, got, skip)
		}
	}
}
