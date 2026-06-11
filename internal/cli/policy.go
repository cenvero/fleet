// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// newPolicyCommand builds `fleet policy` for managing controller output policy.
//
//	fleet policy set redact-pattern '<regex>[,<regex>...]'
//	fleet policy set redact-defaults on|off
//	fleet policy show
//
// Redaction patterns are stored locally in the controller config dir
// (policy.json). Wiring Redact() into exec output is the main loop's job; this
// command only manages the stored policy.
func newPolicyCommand(configDir *string) *cobra.Command {
	policyCmd := &cobra.Command{
		Use:   "policy",
		Short: "Manage controller output policy (e.g. redaction patterns)",
		Long: "Manage local controller output policy. Redaction patterns are regexes; every\n" +
			"match in command output is replaced with " + core.RedactPlaceholder + ".\n\n" +
			"Examples:\n" +
			"  fleet policy set redact-pattern 'AKIA[0-9A-Z]{16}'      # one pattern\n" +
			"  fleet policy set redact-pattern 'foo,bar=\\S+'          # comma = two patterns\n" +
			"  fleet policy set redact-pattern ''                      # clear all patterns\n" +
			"  fleet policy set redact-defaults on                     # also redact secret-ish defaults\n" +
			"  fleet policy show",
	}

	setCmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a policy value (redact-pattern, redact-defaults)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPolicySet(cmd, *configDir, args[0], args[1])
		},
	}
	policyCmd.AddCommand(setCmd)

	policyCmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the current output policy",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPolicyShow(cmd, *configDir)
		},
	})

	return policyCmd
}

// runPolicySet applies a policy key/value to the redact store.
func runPolicySet(cmd *cobra.Command, configDir, key, value string) error {
	store, err := core.NewRedactStore(configDir)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "redact-pattern", "redact-patterns":
		patterns := splitPatterns(value)
		if err := store.SetPatterns(patterns); err != nil {
			return err
		}
		fmt.Fprintf(out, "redact patterns set (%d)\n", len(patterns))
		return nil
	case "redact-defaults":
		enabled, err := parseOnOff(value)
		if err != nil {
			return err
		}
		if err := store.SetDefaults(enabled); err != nil {
			return err
		}
		fmt.Fprintf(out, "redact defaults %s\n", onOff(enabled))
		return nil
	default:
		return fmt.Errorf("unknown policy key %q (expected: redact-pattern, redact-defaults)", key)
	}
}

// runPolicyShow prints the configured patterns and defaults flag.
func runPolicyShow(cmd *cobra.Command, configDir string) error {
	store, err := core.NewRedactStore(configDir)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	patterns := store.Patterns()

	fmt.Fprintf(out, "redact-defaults: %s\n", onOff(store.DefaultsEnabled()))
	if len(patterns) == 0 {
		fmt.Fprintln(out, "redact-pattern: (none)")
		return nil
	}
	fmt.Fprintln(out, "redact-pattern:")
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "  #\tPATTERN"); err != nil {
		return err
	}
	for i, p := range patterns {
		if _, err := fmt.Fprintf(w, "  %d\t%s\n", i+1, p); err != nil {
			return err
		}
	}
	return w.Flush()
}

// splitPatterns splits a comma-separated pattern value, trimming whitespace and
// dropping empties. An empty value clears all patterns (returns nil).
func splitPatterns(value string) []string {
	out := make([]string, 0)
	for _, p := range strings.Split(value, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseOnOff parses on/off/true/false/yes/no/1/0.
func parseOnOff(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "on", "true", "yes", "1", "enable", "enabled":
		return true, nil
	case "off", "false", "no", "0", "disable", "disabled":
		return false, nil
	default:
		return false, fmt.Errorf("invalid value %q: expected on or off", value)
	}
}

// onOff renders a bool as "on"/"off".
func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
