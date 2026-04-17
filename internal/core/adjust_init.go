// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	fleetcrypto "github.com/cenvero/fleet/internal/crypto"
	"github.com/cenvero/fleet/internal/store"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
)

// CurrentInitVersion is the init schema version shipped with this binary.
// Bump this whenever the init wizard gains or loses options so that existing
// installs know to run 'fleet adjust-init'.
const CurrentInitVersion = 2

// ─── version-based migration table ──────────────────────────────────────────

// initChange describes one change made to the init wizard between two versions.
type initChange struct {
	Field       string // config field name, for display
	Kind        string // "removed" or "added"
	Description string
	// Prompt is called for changes that need user input.
	// nil means the change is informational / auto-applied.
	Prompt func(cfg *Config, reader *bufio.Reader, out io.Writer) error
}

type initMigration struct {
	FromVersion int
	Changes     []initChange
}

// initMigrations is the ordered history of init-wizard schema changes.
// Add a new entry here whenever the wizard changes; bump CurrentInitVersion.
var initMigrations = []initMigration{
	{
		// v1 → v2: removed the "short alias" step.
		// The alias field still exists in the config but is no longer prompted
		// during init. If the user had set a custom alias, offer to reset it.
		FromVersion: 1,
		Changes: []initChange{
			{
				Field: "alias",
				Kind:  "removed",
				Description: "'fleet' is now always the binary name — " +
					"the custom-alias step has been removed from the wizard.",
				Prompt: func(cfg *Config, reader *bufio.Reader, out io.Writer) error {
					if cfg.Alias == "" || cfg.Alias == "fleet" {
						return nil
					}
					fmt.Fprintf(out, "  Your config has alias=%q.\n", cfg.Alias)
					fmt.Fprintln(out, "  The alias symlink is no longer created automatically.")
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
	// Template for future migrations:
	//
	// {
	//     FromVersion: 2,
	//     Changes: []initChange{
	//         {
	//             Field: "runtime.metrics_poll_interval",
	//             Kind:  "added",
	//             Description: "How often the controller polls metrics from agents.",
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

// ─── structural integrity check ──────────────────────────────────────────────

// structuralIssue describes a required config field that is missing or
// incorrect, independent of the migration version.
type structuralIssue struct {
	Field       string
	Description string
	// Fix is called to repair the field interactively.
	// If Fix is nil the field is auto-corrected without user input.
	Fix func(cfg *Config, reader *bufio.Reader, out io.Writer) error
}

// configStructuralIssues returns every required config field that is either
// missing or set to an invalid/inconsistent value. This runs independently of
// the version-migration table so configs that were hand-edited or very old also
// get caught.
func configStructuralIssues(cfg *Config, isHomebrew bool) []structuralIssue {
	var issues []structuralIssue

	// 1. Default transport mode
	if cfg.DefaultMode == "" {
		issues = append(issues, structuralIssue{
			Field:       "default_transport_mode",
			Description: "Missing — the default transport mode was not set.",
			Fix: func(cfg *Config, reader *bufio.Reader, out io.Writer) error {
				fmt.Fprintln(out, "  [1] reverse  (agent dials the controller — works behind NAT) [default]")
				fmt.Fprintln(out, "  [2] direct   (controller SSHes into the agent)")
				fmt.Fprintln(out, "  [3] per-node (decide per server)")
				choice, err := prompt(reader, out, "  Choice [1]: ", "1")
				if err != nil {
					return err
				}
				switch choice {
				case "2":
					cfg.DefaultMode = transport.ModeDirect
				case "3":
					cfg.DefaultMode = transport.ModePerNode
				default:
					cfg.DefaultMode = transport.ModeReverse
				}
				return nil
			},
		})
	}

	// 2. Crypto algorithm
	if cfg.Crypto.Algorithm == "" {
		issues = append(issues, structuralIssue{
			Field:       "crypto.algorithm",
			Description: "Missing — key algorithm was not recorded.",
			Fix: func(cfg *Config, reader *bufio.Reader, out io.Writer) error {
				fmt.Fprintln(out, "  [1] ed25519  (recommended) [default]")
				fmt.Fprintln(out, "  [2] rsa-4096 (legacy)")
				fmt.Fprintln(out, "  [3] both     (Ed25519 primary, RSA-4096 fallback)")
				choice, err := prompt(reader, out, "  Choice [1]: ", "1")
				if err != nil {
					return err
				}
				switch choice {
				case "2":
					cfg.Crypto.Algorithm = string(fleetcrypto.AlgorithmRSA4096)
					cfg.Crypto.PrimaryKey = "id_rsa4096"
				case "3":
					cfg.Crypto.Algorithm = string(fleetcrypto.AlgorithmBoth)
					cfg.Crypto.PrimaryKey = "id_ed25519"
				default:
					cfg.Crypto.Algorithm = string(fleetcrypto.AlgorithmEd25519)
					cfg.Crypto.PrimaryKey = "id_ed25519"
				}
				return nil
			},
		})
	}

	// 3. Primary key name
	if cfg.Crypto.PrimaryKey == "" {
		issues = append(issues, structuralIssue{
			Field:       "crypto.primary_key",
			Description: "Missing — auto-corrected from algorithm setting.",
			Fix: func(cfg *Config, _ *bufio.Reader, _ io.Writer) error {
				if cfg.Crypto.Algorithm == string(fleetcrypto.AlgorithmRSA4096) {
					cfg.Crypto.PrimaryKey = "id_rsa4096"
				} else {
					cfg.Crypto.PrimaryKey = "id_ed25519"
				}
				return nil
			},
		})
	}

	// 4. Update channel
	if cfg.Updates.Channel == "" {
		issues = append(issues, structuralIssue{
			Field:       "updates.channel",
			Description: "Missing — defaulting to 'stable'.",
			Fix: func(cfg *Config, _ *bufio.Reader, _ io.Writer) error {
				cfg.Updates.Channel = "stable"
				return nil
			},
		})
	}

	// 5. Update policy
	if cfg.Updates.Policy == "" {
		issues = append(issues, structuralIssue{
			Field:       "updates.policy",
			Description: "Missing — will be set now.",
			Fix: func(cfg *Config, reader *bufio.Reader, out io.Writer) error {
				if isHomebrew {
					fmt.Fprintln(out, "  Homebrew manages the controller binary, so 'auto-update' is not available.")
					fmt.Fprintln(out, "  [1] notify-only  (show a reminder when updates are available) [default]")
					fmt.Fprintln(out, "  [2] disabled     (no update reminders)")
					choice, err := prompt(reader, out, "  Choice [1]: ", "1")
					if err != nil {
						return err
					}
					if choice == "2" {
						cfg.Updates.Policy = update.PolicyDisabled
					} else {
						cfg.Updates.Policy = update.PolicyNotifyOnly
					}
				} else {
					fmt.Fprintln(out, "  [1] notify-only  (show a reminder, you apply manually) [default]")
					fmt.Fprintln(out, "  [2] auto-update  (download and apply automatically)")
					fmt.Fprintln(out, "  [3] disabled     (no update checks)")
					choice, err := prompt(reader, out, "  Choice [1]: ", "1")
					if err != nil {
						return err
					}
					switch choice {
					case "2":
						cfg.Updates.Policy = update.PolicyAutoUpdate
					case "3":
						cfg.Updates.Policy = update.PolicyDisabled
					default:
						cfg.Updates.Policy = update.PolicyNotifyOnly
					}
				}
				return nil
			},
		})
	}

	// 6. Homebrew + auto-update is a mismatch — Homebrew owns the binary.
	//    auto-update is silently skipped at runtime, but the config is wrong.
	if isHomebrew && cfg.Updates.Policy == update.PolicyAutoUpdate {
		issues = append(issues, structuralIssue{
			Field: "updates.policy",
			Description: "Set to 'auto-update' but this install is managed by Homebrew. " +
				"The self-updater cannot replace a Homebrew-managed binary, so auto-update " +
				"has no effect. Fleet will only notify you when updates are available.",
			Fix: func(cfg *Config, reader *bufio.Reader, out io.Writer) error {
				fmt.Fprintln(out, "  Current value: auto-update (has no effect under Homebrew)")
				fmt.Fprintln(out, "  [1] notify-only  (show a reminder when updates are available) [default]")
				fmt.Fprintln(out, "  [2] disabled     (no update reminders)")
				choice, err := prompt(reader, out, "  Choice [1]: ", "1")
				if err != nil {
					return err
				}
				if choice == "2" {
					cfg.Updates.Policy = update.PolicyDisabled
				} else {
					cfg.Updates.Policy = update.PolicyNotifyOnly
				}
				return nil
			},
		})
	}

	// 7. Database backend
	if cfg.Database.Backend == "" {
		issues = append(issues, structuralIssue{
			Field:       "database.backend",
			Description: "Missing — defaulting to 'sqlite'.",
			Fix: func(cfg *Config, _ *bufio.Reader, _ io.Writer) error {
				cfg.Database.Backend = store.BackendSQLite
				cfg.Database = store.WithDefaults(cfg.Database, cfg.ConfigDir)
				return nil
			},
		})
	}

	// 8. Reverse-mode controller needs a listen address.
	if (cfg.DefaultMode == transport.ModeReverse || cfg.DefaultMode == transport.ModePerNode) &&
		cfg.Runtime.ListenAddress == "" {
		issues = append(issues, structuralIssue{
			Field:       "runtime.listen_address",
			Description: "Missing — reverse-mode agents need to know where to dial the controller.",
			Fix: func(cfg *Config, reader *bufio.Reader, out io.Writer) error {
				addr, err := prompt(reader, out, "  Controller listen address [0.0.0.0:9443]: ", "0.0.0.0:9443")
				if err != nil {
					return err
				}
				cfg.Runtime.ListenAddress = strings.TrimSpace(addr)
				return nil
			},
		})
	}

	return issues
}

// ─── public API ──────────────────────────────────────────────────────────────

// NeedsAdjustInit returns true when the config either has a lower InitVersion
// than the current binary OR has structural issues (missing required fields).
func NeedsAdjustInit(cfg Config) bool {
	if cfg.InitVersion < CurrentInitVersion {
		return true
	}
	isHomebrew := RuntimeIsHomebrewInstall()
	return len(configStructuralIssues(&cfg, isHomebrew)) > 0
}

// AdjustInitHint returns a one-line hint to show after every command when the
// config needs attention. Returns "" when everything is up to date.
func AdjustInitHint(cfg Config) string {
	isHomebrew := RuntimeIsHomebrewInstall()
	structural := configStructuralIssues(&cfg, isHomebrew)
	versionBehind := cfg.InitVersion < CurrentInitVersion

	switch {
	case versionBehind && len(structural) > 0:
		return fmt.Sprintf(
			"Config needs updates (init_version %d → %d, %d field(s) missing/incorrect). "+
				"Run 'fleet adjust-init' to fix.",
			cfg.InitVersion, CurrentInitVersion, len(structural),
		)
	case versionBehind:
		return fmt.Sprintf(
			"Config (init_version=%d) is behind this binary (init_version=%d). "+
				"Run 'fleet adjust-init' to apply changes.",
			cfg.InitVersion, CurrentInitVersion,
		)
	case len(structural) > 0:
		return fmt.Sprintf(
			"%d required config field(s) are missing or incorrect. "+
				"Run 'fleet adjust-init' to fix them.",
			len(structural),
		)
	}
	return ""
}

// AdjustInit interactively applies every pending version migration and fixes
// every structural issue, then saves the updated config.
func AdjustInit(configDir string, in io.Reader, out io.Writer) error {
	configPath := ConfigPath(configDir)
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	isHomebrew := RuntimeIsHomebrewInstall()
	structural := configStructuralIssues(&cfg, isHomebrew)
	versionBehind := cfg.InitVersion < CurrentInitVersion

	if !versionBehind && len(structural) == 0 {
		fmt.Fprintln(out, "Your config is fully up to date — nothing to do.")
		return nil
	}

	reader := bufio.NewReader(in)

	fmt.Fprintln(out, "┌─────────────────────────────────────────────────────────────┐")
	fmt.Fprintln(out, "│  fleet adjust-init                                          │")
	fmt.Fprintln(out, "│  Bringing your config up to date with this fleet version.   │")
	fmt.Fprintln(out, "└─────────────────────────────────────────────────────────────┘")
	fmt.Fprintln(out)

	changed := false

	// ── Pass 1: structural integrity ────────────────────────────────────────
	if len(structural) > 0 {
		fmt.Fprintf(out, "── Structural issues (%d) ──\n\n", len(structural))
		for _, issue := range structural {
			fmt.Fprintf(out, "  [!] %s\n", issue.Field)
			fmt.Fprintf(out, "      %s\n\n", issue.Description)
			if issue.Fix != nil {
				if err := issue.Fix(&cfg, reader, out); err != nil {
					return err
				}
				fmt.Fprintln(out)
			}
			changed = true
		}
	}

	// ── Pass 2: version-based migrations ────────────────────────────────────
	if versionBehind {
		for _, migration := range initMigrations {
			if cfg.InitVersion > migration.FromVersion {
				continue
			}
			fmt.Fprintf(out, "── init v%d → v%d ──\n\n", migration.FromVersion, migration.FromVersion+1)
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
				changed = true
			}
		}
		cfg.InitVersion = CurrentInitVersion
	}

	if err := SaveConfig(configPath, cfg); err != nil {
		return fmt.Errorf("save updated config: %w", err)
	}

	if changed {
		fmt.Fprintln(out, "Config saved successfully.")
	}
	fmt.Fprintf(out, "init_version is now %d — all checks passed.\n", CurrentInitVersion)
	return nil
}
