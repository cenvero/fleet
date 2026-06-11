// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// newApprovalsCommand builds the approval-workflow commands:
//
//	fleet approvals list             list staged command approvals
//	fleet approvals reject <id>      reject a pending approval
//	fleet approve <id>               approve a pending approval (top-level)
//
// Approvals are staged by `fleet exec --require-approval` (wired by the main
// loop via core.ApprovalStore.Stage) and stored locally in the controller config
// dir (approvals.json); they don't touch the managed servers.
func newApprovalsCommand(configDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approvals",
		Short: "List and reject staged command approvals",
		Long: "Manage command-execution approvals. A command run with\n" +
			"`fleet exec --require-approval` is staged as a pending approval instead of\n" +
			"running immediately; an operator then approves or rejects it before its TTL\n" +
			"elapses. Approvals are stored locally (approvals.json) in the config dir.\n\n" +
			"Examples:\n" +
			"  fleet approvals list                 # show all approvals\n" +
			"  fleet approve <id>                   # approve a pending request\n" +
			"  fleet approvals reject <id>          # reject a pending request",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	var asJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List staged command approvals",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store := core.NewApprovalStore(*configDir)
			approvals, err := store.List()
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd, approvals)
			}
			return writeApprovalTable(cmd, approvals)
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	cmd.AddCommand(list)

	cmd.AddCommand(&cobra.Command{
		Use:   "reject <id>",
		Short: "Reject a pending approval",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store := core.NewApprovalStore(*configDir)
			approval, err := store.Reject(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "rejected approval %s (%s on %s)\n", approval.ID, approval.Command, approval.Server)
			return nil
		},
	})

	return cmd
}

// newApproveCommand is the top-level `fleet approve <id>` convenience command.
func newApproveCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "approve <id>",
		Short: "Approve a pending command approval",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store := core.NewApprovalStore(*configDir)
			approval, err := store.Approve(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "approved approval %s (%s on %s)\n", approval.ID, approval.Command, approval.Server)
			return nil
		},
	}
}

// writeApprovalTable renders approvals as a sorted table.
func writeApprovalTable(cmd *cobra.Command, approvals []core.Approval) error {
	out := cmd.OutOrStdout()
	if len(approvals) == 0 {
		fmt.Fprintln(out, "no approvals")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "ID\tSERVER\tSTATUS\tEXPIRES\tCOMMAND"); err != nil {
		return err
	}
	for _, a := range approvals {
		expires := "-"
		if !a.Expires.IsZero() {
			expires = a.Expires.Local().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.ID, a.Server, a.Status, expires, a.Command); err != nil {
			return err
		}
	}
	return w.Flush()
}
