// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultDatabaseConfigUsesSeparateSQLiteFiles(t *testing.T) {
	t.Parallel()

	cfg := DefaultDatabaseConfig("/tmp/cenvero-fleet")
	if cfg.Backend != BackendSQLite {
		t.Fatalf("expected sqlite backend, got %q", cfg.Backend)
	}
	if cfg.SQLite.StatePath == cfg.SQLite.MetricsPath || cfg.SQLite.StatePath == cfg.SQLite.EventsPath || cfg.SQLite.MetricsPath == cfg.SQLite.EventsPath {
		t.Fatalf("expected distinct sqlite files for each workload")
	}
}

func TestSQLiteStateAndEventStores(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	cfg := DefaultDatabaseConfig(base)

	stateStore, err := Open(cfg, WorkloadState)
	if err != nil {
		t.Fatalf("Open(state) error = %v", err)
	}
	defer stateStore.Close()

	if err := stateStore.PutState("instance_id", "abc123"); err != nil {
		t.Fatalf("PutState() error = %v", err)
	}

	got, err := stateStore.GetState("instance_id")
	if err != nil {
		t.Fatalf("GetState() error = %v", err)
	}
	if got != "abc123" {
		t.Fatalf("unexpected state value %q", got)
	}

	eventsStore, err := Open(cfg, WorkloadEvents)
	if err != nil {
		t.Fatalf("Open(events) error = %v", err)
	}
	defer eventsStore.Close()

	if err := eventsStore.AppendEvent(time.Now().UTC(), "controller.init", `{"ok":true}`); err != nil {
		t.Fatalf("AppendEvent() error = %v", err)
	}

	for _, path := range []string{
		filepath.Join(base, "data", "state.db"),
		filepath.Join(base, "data", "events.db"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected sqlite file %s: %v", path, err)
		}
	}
}

// TestSQLiteFilesAreOwnerOnly confirms the SQLite database and its WAL/SHM
// sidecars are created (or tightened) to 0600 rather than the driver's
// world-readable default, so on-disk state/secrets aren't readable by others.
func TestSQLiteFilesAreOwnerOnly(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	cfg := DefaultDatabaseConfig(base)

	st, err := Open(cfg, WorkloadState)
	if err != nil {
		t.Fatalf("Open(state) error = %v", err)
	}
	defer st.Close()

	// Force a write so the WAL/SHM sidecars exist too.
	if err := st.PutState("k", "v"); err != nil {
		t.Fatalf("PutState() error = %v", err)
	}

	dataDir := filepath.Join(base, "data")
	if fi, err := os.Stat(dataDir); err != nil {
		t.Fatalf("stat data dir: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("data dir perms = %o, want 0700", perm)
	}

	for _, name := range []string{"state.db", "state.db-wal", "state.db-shm"} {
		p := filepath.Join(dataDir, name)
		fi, err := os.Stat(p)
		if err != nil {
			// Sidecars may not always be present depending on checkpoint timing.
			if os.IsNotExist(err) && name != "state.db" {
				continue
			}
			t.Fatalf("stat %s: %v", name, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s perms = %o, want 0600", name, perm)
		}
	}
}

// TestPrecreateSQLiteFilesAreOwnerOnly verifies that the database file and its
// WAL/SHM/journal sidecars are created 0600 BEFORE the driver opens them. This
// closes the brief world-readable window that existed when the files were
// created with umask-default perms and only chmod'd to 0600 after open. Opening
// with an explicit 0600 mode is umask-safe: a umask can only CLEAR permission
// bits, and 0600 carries no group/other bits for it to clear, so the file is
// born owner-only regardless of the process umask.
func TestPrecreateSQLiteFilesAreOwnerOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "secrets.db")

	if err := precreateSQLiteFiles(dbPath); err != nil {
		t.Fatalf("precreateSQLiteFiles: %v", err)
	}

	for _, suffix := range sqliteFileSuffixes {
		p := dbPath + suffix
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s created with perms %o, want 0600 (readable window not closed)", p, perm)
		}
	}

	// Re-running is a no-op and must not loosen an already-tight file.
	if err := precreateSQLiteFiles(dbPath); err != nil {
		t.Fatalf("precreateSQLiteFiles (re-run): %v", err)
	}
	if fi, err := os.Stat(dbPath); err != nil {
		t.Fatalf("stat after re-run: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("db perms after re-run = %o, want 0600", perm)
	}
}
