// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// newCmdPolicyCommand builds `fleet cmd-policy` for managing the
// dangerous-command policy used by `fleet exec`.
//
//	fleet cmd-policy set deny <p1,p2,...>      block commands matching these
//	fleet cmd-policy set confirm <p1,p2,...>   require --confirm for these
//	fleet cmd-policy show                       print the current policy
//
// Patterns are stored locally in the controller config dir in a SEPARATE file
// (cmd-policy.json) so they never collide with the output-redaction policy
// (policy.json). A pattern with no glob metacharacters ('*'/'?') matches as a
// substring; a pattern with '*'/'?' matches the whole command as a glob.
//
// Wiring MatchDeny (block) and MatchConfirm (require --confirm) into the exec
// path is the main loop's job; this command only manages the stored policy.
func newCmdPolicyCommand(configDir *string) *cobra.Command {
	cmdPolicyCmd := &cobra.Command{
		Use:   "cmd-policy",
		Short: "Manage the dangerous-command policy (deny / confirm patterns)",
		Long: "Manage the dangerous-command policy enforced by `fleet exec`.\n\n" +
			"Deny patterns block matching commands outright. Confirm patterns require\n" +
			"the operator to pass --confirm before a matching command runs. Patterns are\n" +
			"stored locally in the controller config dir (cmd-policy.json), separate from\n" +
			"the output-redaction policy.\n\n" +
			"A pattern with no glob characters matches anywhere in the command (substring);\n" +
			"a pattern containing '*' or '?' matches the whole command as a glob.\n\n" +
			"Examples:\n" +
			"  fleet cmd-policy set deny 'rm -rf /,mkfs'        # two deny substrings\n" +
			"  fleet cmd-policy set deny 'dd of=/dev/sd*'       # glob deny\n" +
			"  fleet cmd-policy set confirm 'reboot,shutdown'   # require --confirm\n" +
			"  fleet cmd-policy set deny ''                     # clear deny patterns\n" +
			"  fleet cmd-policy show",
	}

	setCmd := &cobra.Command{
		Use:   "set <deny|confirm> <patterns>",
		Short: "Set the deny or confirm patterns (comma-separated)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCmdPolicySet(cmd, *configDir, args[0], args[1])
		},
	}
	cmdPolicyCmd.AddCommand(setCmd)

	cmdPolicyCmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the current dangerous-command policy",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCmdPolicyShow(cmd, *configDir)
		},
	})

	return cmdPolicyCmd
}

// runCmdPolicySet stores the deny or confirm patterns. splitPatterns is reused
// from policy.go (same package) to parse the comma-separated value.
func runCmdPolicySet(cmd *cobra.Command, configDir, kind, value string) error {
	store, err := core.NewCmdPolicyStore(configDir)
	if err != nil {
		return err
	}
	patterns := splitPatterns(value)
	out := cmd.OutOrStdout()
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "deny":
		if err := store.SetDenyPatterns(patterns); err != nil {
			return err
		}
		fmt.Fprintf(out, "deny patterns set (%d)\n", len(patterns))
		return nil
	case "confirm":
		if err := store.SetConfirmPatterns(patterns); err != nil {
			return err
		}
		fmt.Fprintf(out, "confirm patterns set (%d)\n", len(patterns))
		return nil
	default:
		return fmt.Errorf("unknown cmd-policy kind %q (expected: deny, confirm)", kind)
	}
}

// runCmdPolicyShow prints the configured deny and confirm patterns.
func runCmdPolicyShow(cmd *cobra.Command, configDir string) error {
	store, err := core.NewCmdPolicyStore(configDir)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if err := printPolicyList(out, "deny", store.DenyPatterns()); err != nil {
		return err
	}
	return printPolicyList(out, "confirm", store.ConfirmPatterns())
}

// printPolicyList prints a labelled, numbered list of patterns (or "(none)").
func printPolicyList(out io.Writer, label string, patterns []string) error {
	if len(patterns) == 0 {
		_, err := fmt.Fprintf(out, "%s: (none)\n", label)
		return err
	}
	if _, err := fmt.Fprintf(out, "%s:\n", label); err != nil {
		return err
	}
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
