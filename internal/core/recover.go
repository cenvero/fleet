// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cenvero/fleet/internal/store"
	"github.com/cenvero/fleet/internal/version"
)

// RecoverOptions controls the recover operation.
type RecoverOptions struct {
	// FromDir is the existing fleet config directory to recover from (required).
	FromDir string
	// TargetConfigDir is where fleet is currently configured to look (from --config-dir).
	// If empty, the default config dir is used.
	TargetConfigDir string
	// DBBackend overrides the database backend detected from the existing config.
	DBBackend string
	// DBDSN overrides the DSN for postgres/mysql/mariadb.
	DBDSN string
	// SkipVersionCheck bypasses the version compatibility guard.
	SkipVersionCheck bool
}

// Recover re-attaches fleet to an existing config directory.
// It verifies version compatibility and database file existence, then
// reports what it found. It does NOT move or copy any files — it simply
// validates that the existing config dir is usable and prints next steps.
func Recover(opts RecoverOptions, out io.Writer) error {
	fromDir := filepath.Clean(opts.FromDir)

	// 1. Verify the source directory has a valid config.
	if !IsInitialized(fromDir) {
		return fmt.Errorf(
			"no fleet configuration found at %s\n\n"+
				"Make sure --from-dir points to a directory that was previously set up with 'fleet init'.\n"+
				"It should contain a fleet.toml (or config.toml) file.",
			fromDir,
		)
	}

	oldCfg, err := LoadConfig(ConfigPath(fromDir))
	if err != nil {
		return fmt.Errorf("read config from %s: %w", fromDir, err)
	}

	// 2. Version compatibility check.
	currentVer := version.Version
	lastVer := strings.TrimSpace(oldCfg.LastSeenVersion)
	if !opts.SkipVersionCheck {
		if oldCfg.SchemaVersion > 1 {
			return fmt.Errorf(
				"config at %s has schema version %d but this fleet binary only supports schema version 1\n\n"+
					"You need a newer fleet binary to open this config.\n"+
					"  fleet update check\n\n"+
					"or download from https://github.com/cenvero/fleet/releases",
				fromDir, oldCfg.SchemaVersion,
			)
		}
		// If we know which binary version last used this config, warn on mismatch.
		if lastVer != "" && lastVer != currentVer && isNewerVersion(currentVer, lastVer) {
			fmt.Fprintf(out,
				"⚠  This config was last used with fleet %s; you are now running fleet %s.\n\n"+
					"It is safer to first restore the old binary version, verify everything works,\n"+
					"then upgrade to %s:\n\n"+
					"  1. Install fleet %s (download from releases or use 'fleet update rollback')\n"+
					"  2. Run 'fleet --config-dir %s status' to verify\n"+
					"  3. Then upgrade to the new version\n\n"+
					"To skip this check and proceed anyway: fleet recover --skip-version-check\n\n",
				lastVer, currentVer, currentVer, lastVer, fromDir,
			)
			return fmt.Errorf("version mismatch: config was last used with fleet %s, current binary is %s", lastVer, currentVer)
		}
	}

	// 3. Check database files for SQLite; connectivity for others.
	backend := store.Backend(strings.TrimSpace(strings.ToLower(opts.DBBackend)))
	if backend == "" {
		backend = oldCfg.Database.Backend
	}
	if backend == "" {
		backend = store.BackendSQLite
	}

	switch backend {
	case store.BackendSQLite:
		dbDir := filepath.Join(fromDir, "data")
		missing := []string{}
		for _, name := range []string{"state.db", "metrics.db", "events.db"} {
			p := filepath.Join(dbDir, name)
			if _, err := os.Stat(p); os.IsNotExist(err) {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			fmt.Fprintf(out, "Warning: the following SQLite database files are missing from %s:\n", dbDir)
			for _, m := range missing {
				fmt.Fprintf(out, "  - %s\n", m)
			}
			fmt.Fprintln(out, "\nFleet will create them fresh on first start. Server records and keys")
			fmt.Fprintln(out, "are preserved (stored in the servers/ directory), but metrics/events")
			fmt.Fprintln(out, "history from those files will be lost.")
			fmt.Fprintln(out)
		}
	case store.BackendPostgres, store.BackendMySQL, store.BackendMariaDB:
		dsn := opts.DBDSN
		if dsn == "" {
			switch backend {
		case store.BackendPostgres:
			dsn = oldCfg.Database.Postgres.DSN
		case store.BackendMySQL:
			dsn = oldCfg.Database.MySQL.DSN
		case store.BackendMariaDB:
			dsn = oldCfg.Database.MariaDB.DSN
		}
		}
		if dsn == "" {
			return fmt.Errorf(
				"database backend is %s but no DSN was found in the config.\n\n"+
					"Pass the DSN explicitly:\n\n"+
					"  fleet recover --from-dir %s --db-backend %s --db-dsn \"<your DSN>\"",
				backend, fromDir, backend,
			)
		}
		// For remote DBs, we don't attempt a live connection here —
		// that requires the full app stack. Just confirm the DSN is set.
		fmt.Fprintf(out, "Database backend : %s\n", backend)
		fmt.Fprintf(out, "DSN              : %s\n", maskDSN(dsn))
	}

	// 4. Print results and next steps.
	fmt.Fprintf(out, "\nRecovery check passed for: %s\n\n", fromDir)
	fmt.Fprintf(out, "  Running version   : %s\n", currentVer)
	if lastVer != "" {
		fmt.Fprintf(out, "  Last used version : %s\n", lastVer)
	}
	fmt.Fprintf(out, "  Schema version    : %d\n", oldCfg.SchemaVersion)
	fmt.Fprintf(out, "  Database backend  : %s\n", backend)
	fmt.Fprintf(out, "  Default mode      : %s\n", oldCfg.DefaultMode)
	fmt.Fprintln(out)

	targetDir := opts.TargetConfigDir
	if targetDir == "" {
		targetDir = DefaultConfigDir("")
	}

	if filepath.Clean(targetDir) == fromDir {
		fmt.Fprintln(out, "The config directory is already at the default location.")
		fmt.Fprintln(out, "Run 'fleet status' to verify everything is working.")
	} else {
		fmt.Fprintln(out, "To use this config directory, pass --config-dir on every command:")
		fmt.Fprintf(out, "\n  fleet --config-dir %s status\n\n", fromDir)
		fmt.Fprintln(out, "Or set the FLEET_CONFIG_DIR environment variable:")
		fmt.Fprintf(out, "\n  export FLEET_CONFIG_DIR=%s\n", fromDir)
	}
	return nil
}

// maskDSN hides the password part of a DSN for safe display.
// e.g. "postgres://user:secret@host/db" → "postgres://user:***@host/db"
func maskDSN(dsn string) string {
	// Handle postgres://user:pass@host style
	for _, scheme := range []string{"postgres://", "postgresql://", "mysql://", "mariadb://"} {
		if strings.HasPrefix(dsn, scheme) {
			rest := dsn[len(scheme):]
			if at := strings.Index(rest, "@"); at >= 0 {
				userInfo := rest[:at]
				if colon := strings.Index(userInfo, ":"); colon >= 0 {
					return scheme + userInfo[:colon+1] + "***" + rest[at:]
				}
			}
		}
	}
	return dsn
}
