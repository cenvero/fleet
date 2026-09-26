// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
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
			"  fleet approve <id>                   # approve a pending request and run it\n" +
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

// newApproveCommand is the top-level `fleet approve <id>` command: it approves a
// pending request and releases it — the staged command runs on its server right
// away, with the options it was staged with, and the outcome is recorded on the
// approval (`fleet approvals list` shows executed/failed).
func newApproveCommand(configDir *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "approve <id>",
		Short: "Approve a pending command approval and run it",
		Long: "Approve a command staged with `fleet exec --require-approval` and run it now.\n\n" +
			"The command runs through the normal `fleet exec` path with the options it was\n" +
			"staged with (timeout, retries, --guard, --confirm, --on-fail, secrets by\n" +
			"reference), so cmd-policy, guard and RBAC checks apply again at run time. An\n" +
			"approval runs at most once; its outcome (executed or failed, with the exit\n" +
			"code) is recorded and shown by `fleet approvals list`. A scoped RBAC token\n" +
			"cannot approve.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			store := core.NewApprovalStore(*configDir)
			approval, err := store.Approve(args[0])
			if err != nil {
				return err
			}
			notes := cmd.OutOrStdout()
			if asJSON {
				notes = cmd.ErrOrStderr()
			}
			fmt.Fprintf(notes, "approved approval %s (%s on %s) — running it now\n", approval.ID, approval.Command, approval.Server)

			tokenFlag, _ := cmd.Flags().GetString("token")
			exitCode, runErr := runApprovedCommand(cmd, *configDir, tokenFlag, approval, asJSON)
			recorded, recErr := store.RecordResult(approval.ID, exitCode, runErr)
			if recErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not record the outcome of approval %s: %v\n", approval.ID, recErr)
			} else {
				fmt.Fprintf(cmd.ErrOrStderr(), "approval %s: %s (exit %d)\n", recorded.ID, recorded.Status, exitCode)
			}
			if runErr != nil {
				return fmt.Errorf("approved command could not run on %s: %w", approval.Server, runErr)
			}
			if exitCode != 0 {
				return fmt.Errorf("approved command failed on %s (exit %d)", approval.Server, exitCode)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the exec result as JSON (like fleet exec --json)")
	return cmd
}

// runApprovedCommand runs an approved command as a child `fleet exec` process so
// it goes through exactly the same gates as a direct invocation (root pre-run,
// RBAC token enforcement, cmd-policy, guard, redaction, audit). It returns the
// child's exit code, or an error when it could not be started at all.
func runApprovedCommand(cmd *cobra.Command, configDir, tokenFlag string, approval core.Approval, asJSON bool) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return -1, fmt.Errorf("locate the fleet binary: %w", err)
	}
	child := exec.CommandContext(cmd.Context(), exe, approvedExecArgs(configDir, tokenFlag, approval, asJSON)...) // #nosec G204 -- re-invokes this same fleet binary with a fixed argv shape
	child.Stdin = nil
	child.Stdout = cmd.OutOrStdout()
	child.Stderr = cmd.ErrOrStderr()
	if err := child.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

// approvedExecArgs builds the `fleet exec` argv that runs an approved command
// with the options it was staged with. The command is passed after `--` as a
// single argument, so it can never be parsed as flags.
func approvedExecArgs(configDir, tokenFlag string, approval core.Approval, asJSON bool) []string {
	args := []string{}
	if strings.TrimSpace(configDir) != "" {
		args = append(args, "--config-dir", configDir)
	}
	if strings.TrimSpace(tokenFlag) != "" {
		args = append(args, "--token", tokenFlag)
	}
	// --propagate-exit makes the child's exit status the remote command's own
	// exit code (also in --json mode, which otherwise exits 0 on a remote
	// failure), so the recorded outcome is exact.
	args = append(args, "exec", approval.Server, "--propagate-exit")
	if asJSON {
		args = append(args, "--json")
	}
	if e := approval.Exec; e != nil {
		if e.Timeout != "" {
			args = append(args, "--timeout", e.Timeout)
		}
		if e.Retry > 0 {
			args = append(args, "--retry", strconv.Itoa(e.Retry))
		}
		if e.Backoff != "" {
			args = append(args, "--backoff", e.Backoff)
		}
		if e.Guard {
			args = append(args, "--guard")
		}
		if e.GuardWarn {
			args = append(args, "--guard-warn")
		}
		if e.Confirm {
			args = append(args, "--confirm")
		}
		if e.OnFail != "" {
			args = append(args, "--on-fail", e.OnFail)
		}
		if e.IdempotencyKey != "" {
			args = append(args, "--idempotency-key", e.IdempotencyKey)
		}
		for _, spec := range e.Secrets {
			args = append(args, "--secret", spec)
		}
	}
	return append(args, "--", approval.Command)
}

// stagedApprovalExec captures the exec options a --require-approval request must
// run with once approved. Only flags the operator actually set are recorded, so
// defaults keep following the current fleet version.
func stagedApprovalExec(cmd *cobra.Command, timeout time.Duration, retries int, backoff time.Duration, guard, guardWarn, confirm bool, onFail, idempotencyKey string, secretSpecs []string) *core.ApprovalExec {
	e := &core.ApprovalExec{
		Retry:          retries,
		Guard:          guard,
		GuardWarn:      guardWarn,
		Confirm:        confirm,
		OnFail:         onFail,
		IdempotencyKey: idempotencyKey,
		Secrets:        append([]string(nil), secretSpecs...),
	}
	if timeout > 0 {
		e.Timeout = timeout.String()
	}
	if cmd.Flags().Changed("backoff") {
		e.Backoff = backoff.String()
	}
	return e
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
