// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/pkg/proto"
	"github.com/spf13/cobra"
)

// FL-001 dead-man's-switch CLI.
//
//	fleet guard <server> --revert-after <dur> [--revert-cmd <undo>] <risky cmd...>
//	fleet confirm <id>
//	fleet revert <id>
//
// `guard` runs a risky command on a server and arms a DETACHED, server-side timer
// that runs the undo command after the deadline UNLESS you `fleet confirm <id>`
// in time. The timer lives entirely on the server, so even if the risky change
// locks you out, the revert still fires. Guard records (id, server, status,
// revert command) are stored in <configDir>/guards.json so confirm/revert can
// find the server without re-typing anything. Change-ids are derived from the
// server name plus an incrementing counter — deterministic and charset-safe.

// guardExec is the minimal surface the guard engine needs from *App, so the pure
// flow can be unit-tested with a fake exec function instead of a live App.
type guardExec interface {
	ExecCommand(server, command string) (proto.ExecResult, error)
}

// guardExecFunc adapts a plain function to guardExec for tests.
type guardExecFunc func(server, command string) (proto.ExecResult, error)

func (f guardExecFunc) ExecCommand(server, command string) (proto.ExecResult, error) {
	return f(server, command)
}

func newGuardCommand(configDir *string) *cobra.Command {
	var revertAfter time.Duration
	var revertCmd string
	cmd := &cobra.Command{
		Use:   "guard <server> <risky command...>",
		Short: "Run a risky command with a dead-man's-switch that auto-reverts unless confirmed",
		Long: "Run a risky command on a server and arm a DETACHED, server-side timer that runs\n" +
			"an undo command after a deadline UNLESS you confirm in time. The timer lives on\n" +
			"the server, so even a change that locks you out (firewall, sshd, network) still\n" +
			"reverts automatically. If the change is good, run 'fleet confirm <id>' to cancel\n" +
			"the revert; to undo now, run 'fleet revert <id>'.\n\n" +
			"Examples:\n" +
			"  fleet guard web-01 --revert-after 2m --revert-cmd 'ufw disable' ufw enable\n" +
			"  fleet guard web-01 --revert-after 90s --revert-cmd 'systemctl restart sshd' \\\n" +
			"      'sed -i s/^#Port.*/Port 2200/ /etc/ssh/sshd_config && systemctl restart sshd'",
		Args:         cobra.MinimumNArgs(2),
		SilenceUsage: true, // a remote non-zero exit must not dump usage text
		RunE: func(cmd *cobra.Command, args []string) error {
			server := args[0]
			risky := strings.Join(args[1:], " ")

			secs := int(revertAfter.Round(time.Second).Seconds())
			if secs <= 0 {
				return fmt.Errorf("--revert-after must be a positive duration (e.g. 2m, 90s)")
			}

			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			// Resolve the server up front so we fail fast on a bad name and store
			// the canonical name in the guard record.
			record, err := app.GetServer(server)
			if err != nil {
				return err
			}

			store := core.NewGuardStore(*configDir)
			id, err := store.NextGuardID(record.Name)
			if err != nil {
				return err
			}

			revert := strings.TrimSpace(revertCmd)
			storedRevert := revert
			if revert == "" {
				revert = core.DefaultRevertCommand(id)
				storedRevert = "" // keep the record honest: no real undo configured
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: no --revert-cmd given; the timer will only log a warning, not undo the change\n")
			}

			// Persist the guard before arming so confirm/revert always have a record,
			// even if the remote exec fails partway.
			if err := store.Put(core.GuardRecord{
				ID:          id,
				Server:      record.Name,
				Status:      core.GuardPending,
				RevertCmd:   storedRevert,
				RiskyCmd:    risky,
				RevertAfter: revertAfter.String(),
			}); err != nil {
				return err
			}

			result, runErr := armGuard(app, record.Name, id, risky, revert, secs)
			printExec(cmd, result)
			if runErr != nil {
				return runErr
			}
			if err := guardExecError(record.Name, "arm guard (risky command or scheduling)", result); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(),
				"\nguard armed: id=%s server=%s revert-after=%s\n", id, record.Name, revertAfter)
			fmt.Fprintf(cmd.OutOrStdout(),
				"confirm before the deadline:  fleet confirm %s\n", id)
			fmt.Fprintf(cmd.OutOrStdout(),
				"revert immediately:           fleet revert %s\n", id)
			return nil
		},
	}
	cmd.Flags().DurationVar(&revertAfter, "revert-after", 0, "auto-revert after this duration unless confirmed (e.g. 2m, 90s)")
	cmd.Flags().StringVar(&revertCmd, "revert-cmd", "", "shell command that undoes the risky change (strongly recommended)")
	_ = cmd.MarkFlagRequired("revert-after")
	return cmd
}

func newConfirmCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:          "confirm <id>",
		Short:        "Confirm a guarded change so its dead-man's-switch does not revert",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if err := core.ValidateGuardID(id); err != nil {
				return err
			}
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			store := core.NewGuardStore(*configDir)
			rec, ok := store.Get(id)
			if !ok {
				return fmt.Errorf("guard %q not found — run 'fleet guard ...' first", id)
			}
			// Only a pending guard can be confirmed: confirming an already
			// confirmed/reverted guard is meaningless and must not silently flip
			// its status.
			if rec.Status != core.GuardPending {
				return fmt.Errorf("guard %q is %s, not pending; nothing to confirm", id, rec.Status)
			}

			result, runErr := confirmGuard(app, rec.Server, id)
			printExec(cmd, result)
			if runErr != nil {
				return runErr
			}
			if err := guardExecError(rec.Server, "confirm guard", result); err != nil {
				return err
			}
			if err := store.SetStatus(id, core.GuardConfirmed); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nguard %s confirmed on %s; auto-revert cancelled\n", id, rec.Server)
			return nil
		},
	}
}

func newRevertCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:          "revert <id>",
		Short:        "Revert a guarded change immediately",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if err := core.ValidateGuardID(id); err != nil {
				return err
			}
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			store := core.NewGuardStore(*configDir)
			rec, ok := store.Get(id)
			if !ok {
				return fmt.Errorf("guard %q not found — run 'fleet guard ...' first", id)
			}
			// Only a pending guard can be reverted: a confirmed or already-reverted
			// guard must not be flipped to 'reverted' (a confirmed change was kept
			// on purpose; an already-reverted one has been undone).
			if rec.Status != core.GuardPending {
				return fmt.Errorf("guard %q is %s, not pending; nothing to revert", id, rec.Status)
			}

			result, runErr := revertGuard(app, rec.Server, id, rec.RevertCmd)
			printExec(cmd, result)
			if runErr != nil {
				return runErr
			}
			if err := guardExecError(rec.Server, "revert guard", result); err != nil {
				return err
			}
			if err := store.SetStatus(id, core.GuardReverted); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nguard %s reverted on %s\n", id, rec.Server)
			return nil
		},
	}
}

// armGuard, confirmGuard, and revertGuard are the pure flows: they build the
// remote command and run it through any guardExec (an *App or a fake), so the
// logic is testable without a live controller.

func armGuard(x guardExec, server, id, risky, revert string, secs int) (proto.ExecResult, error) {
	command, err := core.BuildGuardArmCommand(id, risky, revert, secs)
	if err != nil {
		return proto.ExecResult{}, err
	}
	return x.ExecCommand(server, command)
}

func confirmGuard(x guardExec, server, id string) (proto.ExecResult, error) {
	command, err := core.BuildGuardConfirmCommand(id)
	if err != nil {
		return proto.ExecResult{}, err
	}
	return x.ExecCommand(server, command)
}

func revertGuard(x guardExec, server, id, revertCmd string) (proto.ExecResult, error) {
	command, err := core.BuildGuardRevertCommand(id, revertCmd)
	if err != nil {
		return proto.ExecResult{}, err
	}
	return x.ExecCommand(server, command)
}

// guardExecError turns a non-zero remote exit code into an error, mirroring the
// pattern in cron.go/journal.go/drift.go. Without this a failed risky command, a
// failed confirm, or a failed revert would be silently treated as success (and
// the guard record flipped to confirmed/reverted regardless).
func guardExecError(server, action string, result proto.ExecResult) error {
	if result.ExitCode == 0 {
		return nil
	}
	msg := strings.TrimSpace(result.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(result.Stdout)
	}
	if msg == "" {
		msg = fmt.Sprintf("exit code %d", result.ExitCode)
	}
	return fmt.Errorf("%s on %s failed (exit %d): %s", action, server, result.ExitCode, msg)
}

// printExec writes the remote command's stdout/stderr to the command output,
// trimming a single trailing newline so the surrounding messages read cleanly.
func printExec(cmd *cobra.Command, result proto.ExecResult) {
	if out := strings.TrimRight(result.Stdout, "\n"); out != "" {
		fmt.Fprintln(cmd.OutOrStdout(), out)
	}
	if errOut := strings.TrimRight(result.Stderr, "\n"); errOut != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), errOut)
	}
}
