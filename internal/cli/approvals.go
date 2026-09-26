// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/spf13/cobra"
	"golang.org/x/term"
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
			auditLocal(*configDir, "approval.reject", approval.Server,
				fmt.Sprintf("id=%s command=%q", approval.ID, redactForAudit(*configDir, approval.Command)))
			fmt.Fprintf(cmd.OutOrStdout(), "rejected approval %s (%s on %s)\n", approval.ID, approval.Command, approval.Server)
			return nil
		},
	})

	return cmd
}

// newApproveCommand is the top-level `fleet approve <id>` command: it shows the
// full staged request, asks for confirmation, then approves it and releases it —
// the staged command runs on its server right away, with the options it was
// staged with, and the outcome is recorded on the approval (`fleet approvals
// list` shows executed/failed).
func newApproveCommand(configDir *string) *cobra.Command {
	var asJSON, yes bool
	cmd := &cobra.Command{
		Use:   "approve <id>",
		Short: "Review a pending command approval, then approve and run it",
		Long: "Review a command staged with `fleet exec --require-approval`, then approve it and\n" +
			"run it now.\n\n" +
			"The full request is shown first — server, command, every exec option it was\n" +
			"staged with (timeout, retries, --guard, --confirm, --on-fail, secrets by\n" +
			"reference) and who staged it — and you are asked to confirm. Without a\n" +
			"terminal, pass --yes after reviewing the request (`fleet approvals list --json`).\n\n" +
			"The command runs through the normal `fleet exec` path with those options, so\n" +
			"cmd-policy, guard and RBAC checks apply again at run time. An approval runs\n" +
			"at most once; its outcome (executed or failed, with the exit code) is\n" +
			"recorded and shown by `fleet approvals list`. A scoped RBAC token cannot\n" +
			"approve.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			store := core.NewApprovalStore(*configDir)
			pending, err := store.Get(args[0])
			if err != nil {
				return err
			}
			if pending.Status != core.ApprovalPending {
				return fmt.Errorf("approval %q is %s, not pending", pending.ID, pending.Status)
			}
			// An approval staged by an older fleet was not validated: never hand
			// anything but a plain server name to `fleet exec`.
			if err := core.ValidateApprovalServer(pending.Server); err != nil {
				return fmt.Errorf("refusing to approve %s: %w", pending.ID, err)
			}
			notes := cmd.OutOrStdout()
			if asJSON {
				notes = cmd.ErrOrStderr()
			}
			describeApproval(notes, pending)
			if !yes {
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("refusing to approve %s without confirmation: review the request above and re-run with --yes", pending.ID)
				}
				fmt.Fprint(notes, "Approve and run it now? [y/N]: ")
				answer := strings.ToLower(strings.TrimSpace(transport.ReadLine(bufio.NewReader(os.Stdin))))
				if answer != "y" && answer != "yes" {
					return fmt.Errorf("not approved; %s stays pending", pending.ID)
				}
			}

			approval, err := store.Approve(pending.ID)
			if err != nil {
				return err
			}
			// The run itself is audited by the child `fleet exec` (exec.run).
			auditLocal(*configDir, "approval.approve", approval.Server,
				fmt.Sprintf("id=%s command=%q", approval.ID, redactForAudit(*configDir, approval.Command)))
			fmt.Fprintf(notes, "approved approval %s — running it now on %s\n", approval.ID, approval.Server)

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
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "approve without the interactive confirmation (review the request first)")
	return cmd
}

// describeApproval prints everything the approver is about to release: the
// server, the exact command, every exec option it will run with, and who staged
// it when. Nothing staged is hidden from the approver.
func describeApproval(w io.Writer, a core.Approval) {
	requestedBy := a.RequestedBy
	if requestedBy == "" {
		requestedBy = "unknown"
	}
	fmt.Fprintf(w, "approval %s — staged by %s at %s\n", a.ID, requestedBy, a.Requested.Local().Format(time.RFC3339))
	fmt.Fprintf(w, "  server : %s\n", a.Server)
	fmt.Fprintf(w, "  command: %s\n", a.Command)
	if opts := approvalOptionArgs(a.Exec); len(opts) > 0 {
		fmt.Fprintf(w, "  options: %s\n", shellJoin(opts))
	} else {
		fmt.Fprintln(w, "  options: (none)")
	}
}

// runApprovedCommand runs an approved command as a child `fleet exec` process so
// it goes through exactly the same gates as a direct invocation (root pre-run,
// RBAC token enforcement, cmd-policy, guard, redaction, audit). It returns the
// child's exit code, or an error when it could not be started at all. A --token
// given to approve is handed to the child through FLEET_TOKEN, never on its
// argv, where other local users could read it.
func runApprovedCommand(cmd *cobra.Command, configDir, tokenFlag string, approval core.Approval, asJSON bool) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return -1, fmt.Errorf("locate the fleet binary: %w", err)
	}
	child := exec.CommandContext(cmd.Context(), exe, approvedExecArgs(configDir, approval, asJSON)...) // #nosec G204 -- re-invokes this same fleet binary with a fixed argv shape
	child.Env = approvedExecEnv(os.Environ(), tokenFlag)
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

// approvedExecEnv is env with FLEET_TOKEN set to tokenFlag when one was given
// (a --token flag takes precedence over the environment, as in the parent).
func approvedExecEnv(env []string, tokenFlag string) []string {
	tokenFlag = strings.TrimSpace(tokenFlag)
	if tokenFlag == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "FLEET_TOKEN=") {
			out = append(out, kv)
		}
	}
	return append(out, "FLEET_TOKEN="+tokenFlag)
}

// approvedExecArgs builds the `fleet exec` argv that runs an approved command
// with the options it was staged with. The server and the command both come
// after `--`, so neither can ever be parsed as a flag.
func approvedExecArgs(configDir string, approval core.Approval, asJSON bool) []string {
	args := []string{}
	if strings.TrimSpace(configDir) != "" {
		args = append(args, "--config-dir", configDir)
	}
	// --propagate-exit makes the child's exit status the remote command's own
	// exit code (also in --json mode, which otherwise exits 0 on a remote
	// failure), so the recorded outcome is exact.
	args = append(args, "exec", "--propagate-exit")
	if asJSON {
		args = append(args, "--json")
	}
	args = append(args, approvalOptionArgs(approval.Exec)...)
	return append(args, "--", approval.Server, approval.Command)
}

// approvalOptionArgs renders staged exec options as `fleet exec` flags.
func approvalOptionArgs(e *core.ApprovalExec) []string {
	if e == nil {
		return nil
	}
	var args []string
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
	return args
}

// shellJoin renders args for display, quoting any that need it.
func shellJoin(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if a != "" && !strings.ContainsAny(a, " \t\n'\"\\$`;&|<>(){}*?[]#~!") {
			parts[i] = a
		} else {
			parts[i] = shellQuote(a)
		}
	}
	return strings.Join(parts, " ")
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
	if _, err := fmt.Fprintln(w, "ID\tSERVER\tSTATUS\tEXPIRES\tOPTIONS\tCOMMAND"); err != nil {
		return err
	}
	for _, a := range approvals {
		expires := "-"
		if !a.Expires.IsZero() {
			expires = a.Expires.Local().Format(time.RFC3339)
		}
		options := "-"
		if opts := approvalOptionArgs(a.Exec); len(opts) > 0 {
			options = shellJoin(opts)
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Server, a.Status, expires, options, a.Command); err != nil {
			return err
		}
	}
	return w.Flush()
}
