// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenIsLazyAndConfigShared: Open no longer opens the databases, and the
// shared config cache still sees every change to the file.
func TestOpenIsLazyAndConfigShared(t *testing.T) {
	t.Parallel()
	app := newFanoutTestApp(t, 0)
	dir := app.ConfigDir
	_ = app.Close()
	for _, name := range []string{"state.db", "metrics.db"} {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			_ = os.Remove(filepath.Join(dir, "data", name+suffix))
		}
	}
	app2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "state.db")); !os.IsNotExist(err) {
		t.Fatalf("Open touched the state database (stat err %v)", err)
	}
	if err := app2.StateDB.PutState("k", "v"); err != nil {
		t.Fatalf("first use of the lazy store: %v", err)
	}
	_ = app2.Close()

	cfg1, err := LoadConfigShared(ConfigPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	cfg1.Operator = "someone-else"
	if err := SaveConfig(ConfigPath(dir), cfg1); err != nil {
		t.Fatal(err)
	}
	cfg2, err := LoadConfigShared(ConfigPath(dir))
	if err != nil || cfg2.Operator != "someone-else" {
		t.Fatalf("shared config missed a change: %q %v", cfg2.Operator, err)
	}
}
