// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/store"
)

type DatabaseShiftResult struct {
	FromBackend    store.Backend `json:"from_backend"`
	ToBackend      store.Backend `json:"to_backend"`
	StateEntries   int           `json:"state_entries"`
	MetricsEntries int           `json:"metrics_entries"`
	EventEntries   int           `json:"event_entries"`
}

func ShiftDatabase(configDir string, backend store.Backend, dsn string) (DatabaseShiftResult, error) {
	if configDir == "" {
		configDir = DefaultConfigDir("")
	}
	cfg, err := LoadConfig(ConfigPath(configDir))
	if err != nil {
		if os.IsNotExist(err) {
			return DatabaseShiftResult{}, ErrNotInitialized
		}
		return DatabaseShiftResult{}, err
	}

	target, err := ResolveTargetDatabaseConfig(configDir, cfg.Database, backend, dsn)
	if err != nil {
		return DatabaseShiftResult{}, err
	}
	return shiftDatabaseToTarget(configDir, cfg, target)
}

func ResolveTargetDatabaseConfig(configDir string, current store.DatabaseConfig, backend store.Backend, dsn string) (store.DatabaseConfig, error) {
	current = store.WithDefaults(current, configDir)
	target := current
	target.Backend = backend

	switch backend {
	case store.BackendSQLite:
		target.SQLite = store.DefaultDatabaseConfig(configDir).SQLite
	case store.BackendPostgres:
		if dsn == "" {
			dsn = current.Postgres.DSN
		}
		target.Postgres.DSN = dsn
	case store.BackendMySQL:
		if dsn == "" {
			dsn = current.MySQL.DSN
		}
		target.MySQL.DSN = dsn
	case store.BackendMariaDB:
		if dsn == "" {
			dsn = current.MariaDB.DSN
		}
		target.MariaDB.DSN = dsn
	default:
		return store.DatabaseConfig{}, fmt.Errorf("unsupported database backend %q", backend)
	}

	target = store.WithDefaults(target, configDir)
	if err := target.Validate(); err != nil {
		return store.DatabaseConfig{}, fmt.Errorf("target database config: %w", err)
	}
	if equivalentDatabaseConfig(current, target) {
		return store.DatabaseConfig{}, fmt.Errorf("database is already using backend %q with the same target settings", backend)
	}
	return target, nil
}

func shiftDatabaseToTarget(configDir string, cfg Config, target store.DatabaseConfig) (DatabaseShiftResult, error) {
	cfg.Database = store.WithDefaults(cfg.Database, configDir)
	target = store.WithDefaults(target, configDir)

	stateEntries, err := migrateStateWorkload(cfg.Database, target, store.WorkloadState)
	if err != nil {
		return DatabaseShiftResult{}, fmt.Errorf("migrate state workload: %w", err)
	}
	metricsEntries, err := migrateStateWorkload(cfg.Database, target, store.WorkloadMetrics)
	if err != nil {
		return DatabaseShiftResult{}, fmt.Errorf("migrate metrics workload: %w", err)
	}
	eventEntries, err := migrateEventWorkload(cfg.Database, target)
	if err != nil {
		return DatabaseShiftResult{}, fmt.Errorf("migrate events workload: %w", err)
	}

	previous := cfg.Database.Backend
	cfg.Database = target
	if err := SaveConfig(ConfigPath(configDir), cfg); err != nil {
		return DatabaseShiftResult{}, fmt.Errorf("save shifted database config: %w", err)
	}

	// Best-effort: migration is done; don't fail on audit log error.
	audit := logs.NewAuditLog(filepath.Join(configDir, "logs", "_audit.log"))
	_ = audit.Append(logs.AuditEntry{
		Action:   "database.shift",
		Target:   string(target.Backend),
		Operator: cfg.Operator,
		Details:  fmt.Sprintf("from=%s state=%d metrics=%d events=%d", previous, stateEntries, metricsEntries, eventEntries),
	})

	return DatabaseShiftResult{
		FromBackend:    previous,
		ToBackend:      target.Backend,
		StateEntries:   stateEntries,
		MetricsEntries: metricsEntries,
		EventEntries:   eventEntries,
	}, nil
}

func migrateStateWorkload(sourceCfg, targetCfg store.DatabaseConfig, workload store.Workload) (int, error) {
	source, err := store.Open(sourceCfg, workload)
	if err != nil {
		return 0, err
	}
	defer source.Close()

	target, err := store.Open(targetCfg, workload)
	if err != nil {
		return 0, err
	}
	defer target.Close()

	entries, err := source.ListStateEntries()
	if err != nil {
		return 0, err
	}
	if err := target.ReplaceStateEntries(entries); err != nil {
		return 0, err
	}
	return len(entries), nil
}

func migrateEventWorkload(sourceCfg, targetCfg store.DatabaseConfig) (int, error) {
	source, err := store.Open(sourceCfg, store.WorkloadEvents)
	if err != nil {
		return 0, err
	}
	defer source.Close()

	target, err := store.Open(targetCfg, store.WorkloadEvents)
	if err != nil {
		return 0, err
	}
	defer target.Close()

	entries, err := source.ListEvents()
	if err != nil {
		return 0, err
	}
	if err := target.ReplaceEvents(entries); err != nil {
		return 0, err
	}
	return len(entries), nil
}

func equivalentDatabaseConfig(a, b store.DatabaseConfig) bool {
	a = store.WithDefaults(a, "")
	b = store.WithDefaults(b, "")
	if a.Backend != b.Backend {
		return false
	}
	switch a.Backend {
	case store.BackendSQLite:
		return a.SQLite.StatePath == b.SQLite.StatePath &&
			a.SQLite.MetricsPath == b.SQLite.MetricsPath &&
			a.SQLite.EventsPath == b.SQLite.EventsPath
	case store.BackendPostgres:
		return a.Postgres.DSN == b.Postgres.DSN
	case store.BackendMySQL:
		return a.MySQL.DSN == b.MySQL.DSN
	case store.BackendMariaDB:
		return a.MariaDB.DSN == b.MariaDB.DSN
	default:
		return false
	}
}
