// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/cenvero/fleet/internal/store"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
)

// CurrentInitVersion is the init schema version shipped with this binary.
// Bump it whenever the init wizard gains or loses options, so existing
// installs know to run 'fleet adjust-init'.
const CurrentInitVersion = 2

// initChange describes one change made to the init wizard between two versions.
type initChange struct {
	// Field is the config field name affected, for display.
	Field string
	// Kind is "removed" or "added".
	Kind string
	// Description explains what the field was / is.
	Description string
	// Prompt is shown for "added" fields so the user can supply a value.
	// nil means no interactive input is needed (value is derived automatically).
	Prompt func(cfg *Config, reader *bufio.Reader, out io.Writer) error
}

// initMigration bundles the changes for one version step.
type initMigration struct {
	// FromVersion is the InitVersion this migration upgrades FROM.
	FromVersion int
	Changes     []initChange
}

// initMigrations is the full history of init schema changes.
// Add a new entry here whenever you change the init wizard.
// Keep entries in ascending FromVersion order.
var initMigrations = []initMigration{
	{
		// v1 → v2: removed the "short alias" step from the interactive wizard.
		// The alias field still exists in the config but is no longer prompted.
		// If the user had a custom alias symlink, offer to clean it up.
		FromVersion: 1,
		Changes: []initChange{
			{
				Field:       "alias",
				Kind:        "removed",
				Description: "Custom alias step removed from wizard — 'fleet' is always the binary name now. " +
					"Your alias setting is preserved in the config but the symlink creation step is gone.",
				Prompt: func(cfg *Config, reader *bufio.Reader, out io.Writer) error {
					if cfg.Alias == "" || cfg.Alias == "fleet" {
						// Nothing to do.
						return nil
					}
					fmt.Fprintf(out, "  Your config has alias=%q.\n", cfg.Alias)
					fmt.Fprintln(out, "  This symlink will no longer be maintained automatically.")
					ans, err := prompt(reader, out, "  Reset alias to 'fleet'? [Y/n]: ", "y")
					if err != nil {
						return err
					}
					if strings.ToLower(strings.TrimSpace(ans)) != "n" {
						cfg.Alias = "fleet"
					}
					return nil
				},
			},
		},
	},
	// Example of a future "added" migration — uncomment and fill when needed:
	//
	// {
	//     FromVersion: 2,
	//     Changes: []initChange{
	//         {
	//             Field: "runtime.metrics_poll_interval",
	//             Kind:  "added",
	//             Description: "How often metrics are polled from agents.",
	//             Prompt: func(cfg *Config, reader *bufio.Reader, out io.Writer) error {
	//                 v, err := prompt(reader, out, "  Metrics poll interval [30s]: ", "30s")
	//                 if err != nil { return err }
	//                 cfg.Runtime.MetricsPollInterval = v
	//                 return nil
	//             },
	//         },
	//     },
	// },
}

// NeedsAdjustInit returns true when the stored config's InitVersion is behind
// the current binary's init schema version.
func NeedsAdjustInit(cfg Config) bool {
	return cfg.InitVersion < CurrentInitVersion
}

// AdjustInit interactively walks the user through all pending config migrations
// and saves the updated config. It prints a summary of every change made.
func AdjustInit(configDir string, in io.Reader, out io.Writer) error {
	configPath := ConfigPath(configDir)
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if !NeedsAdjustInit(cfg) {
		fmt.Fprintln(out, "Your config is already up to date — nothing to do.")
		return nil
	}

	reader := bufio.NewReader(in)

	fmt.Fprintln(out, "┌─────────────────────────────────────────────────────────────┐")
	fmt.Fprintln(out, "│  fleet adjust-init                                          │")
	fmt.Fprintln(out, "│  Updating your config to match the current fleet version.   │")
	fmt.Fprintln(out, "└─────────────────────────────────────────────────────────────┘")
	fmt.Fprintln(out)

	anyChanges := false

	for _, migration := range initMigrations {
		if cfg.InitVersion > migration.FromVersion {
			continue // already applied
		}

		fmt.Fprintf(out, "── Changes in init v%d → v%d ──\n\n", migration.FromVersion, migration.FromVersion+1)

		for _, ch := range migration.Changes {
			icon := "✕"
			if ch.Kind == "added" {
				icon = "+"
			}
			fmt.Fprintf(out, "  [%s] %s (%s)\n", icon, ch.Field, ch.Kind)
			fmt.Fprintf(out, "      %s\n\n", ch.Description)

			if ch.Prompt != nil {
				if err := ch.Prompt(&cfg, reader, out); err != nil {
					return err
				}
				fmt.Fprintln(out)
			}
			anyChanges = true
		}
	}

	cfg.InitVersion = CurrentInitVersion
	if err := SaveConfig(configPath, cfg); err != nil {
		return fmt.Errorf("save updated config: %w", err)
	}

	if anyChanges {
		fmt.Fprintln(out, "Config updated successfully.")
	}
	fmt.Fprintf(out, "init_version is now %d.\n", CurrentInitVersion)
	return nil
}

// AdjustInitHint returns a non-empty hint string when the config needs migration.
// This is shown inline after every command so the user knows to act.
func AdjustInitHint(cfg Config) string {
	if !NeedsAdjustInit(cfg) {
		return ""
	}
	return fmt.Sprintf(
		"Your fleet config (init_version=%d) is behind this version (init_version=%d).\n"+
			"Run 'fleet adjust-init' to review and apply configuration changes.",
		cfg.InitVersion, CurrentInitVersion,
	)
}

// initOptionsForVersion returns a human-readable summary of what init
// options exist at a given init version, for display in adjust-init.
func initOptionsForVersion(_ int) []string {
	// This is informational — just list what options the wizard currently has.
	return []string{
		"config_dir      — where fleet data is stored",
		"default_mode    — reverse (agent dials in) or direct (controller SSHes out)",
		"networking      — agent SSH port / controller listen address",
		"crypto          — key algorithm (Ed25519, RSA-4096, both)",
		"update_channel  — stable or beta",
		"database        — SQLite (local files) or PostgreSQL/MySQL/MariaDB",
	}
}

// These are referenced by init.go — keep them in sync.
var (
	_ = store.BackendSQLite
	_ = transport.ModeReverse
	_ = update.PolicyNotifyOnly
)
