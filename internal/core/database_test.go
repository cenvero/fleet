// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/store"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
)

func TestShiftDatabaseToAlternateSQLiteFiles(t *testing.T) {
	t.Parallel()

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

	cfg, err := LoadConfig(ConfigPath(configDir))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	stateStore, err := store.Open(cfg.Database, store.WorkloadState)
	if err != nil {
		t.Fatalf("Open(state) error = %v", err)
	}
	if err := stateStore.PutState("controller.start", "2026-04-13T00:00:00Z"); err != nil {
		t.Fatalf("PutState(state) error = %v", err)
	}
	_ = stateStore.Close()

	metricsStore, err := store.Open(cfg.Database, store.WorkloadMetrics)
	if err != nil {
		t.Fatalf("Open(metrics) error = %v", err)
	}
	if err := metricsStore.PutState("servers.total", "3"); err != nil {
		t.Fatalf("PutState(metrics) error = %v", err)
	}
	_ = metricsStore.Close()

	eventsStore, err := store.Open(cfg.Database, store.WorkloadEvents)
	if err != nil {
		t.Fatalf("Open(events) error = %v", err)
	}
	if err := eventsStore.AppendEvent(time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC), "test.event", `{"ok":true}`); err != nil {
		t.Fatalf("AppendEvent() error = %v", err)
	}
	_ = eventsStore.Close()

	target := cfg.Database
	target.SQLite.StatePath = filepath.Join(configDir, "data", "shifted-state.db")
	target.SQLite.MetricsPath = filepath.Join(configDir, "data", "shifted-metrics.db")
	target.SQLite.EventsPath = filepath.Join(configDir, "data", "shifted-events.db")

	result, err := shiftDatabaseToTarget(configDir, cfg, target)
	if err != nil {
		t.Fatalf("shiftDatabaseToTarget() error = %v", err)
	}

	if result.ToBackend != store.BackendSQLite {
		t.Fatalf("unexpected target backend %q", result.ToBackend)
	}
	if result.StateEntries < 2 {
		t.Fatalf("expected state entries to be copied, got %d", result.StateEntries)
	}
	if result.MetricsEntries < 2 {
		t.Fatalf("expected metrics entries to be copied, got %d", result.MetricsEntries)
	}
	if result.EventEntries < 2 {
		t.Fatalf("expected events entries to be copied, got %d", result.EventEntries)
	}

	reloaded, err := LoadConfig(ConfigPath(configDir))
	if err != nil {
		t.Fatalf("LoadConfig(reloaded) error = %v", err)
	}
	if reloaded.Database.SQLite.StatePath != target.SQLite.StatePath {
		t.Fatalf("expected shifted sqlite state path %q, got %q", target.SQLite.StatePath, reloaded.Database.SQLite.StatePath)
	}

	shiftedState, err := store.Open(reloaded.Database, store.WorkloadState)
	if err != nil {
		t.Fatalf("Open(shifted state) error = %v", err)
	}
	defer shiftedState.Close()
	got, err := shiftedState.GetState("controller.start")
	if err != nil {
		t.Fatalf("GetState(shifted) error = %v", err)
	}
	if got != "2026-04-13T00:00:00Z" {
		t.Fatalf("unexpected shifted state value %q", got)
	}
}
