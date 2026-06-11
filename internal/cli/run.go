// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"strings"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// newRunCommand builds `fleet run <playbook.yaml> [<server>] [--group EXPR]
// [--on-fail rollback] [--dry-run]`.
//
// A playbook is an ordered list of idempotent steps. For each target server,
// every step runs in order: if its `check` exits 0 the step is already
// satisfied and its `apply` is skipped; otherwise `apply` runs. With
// `--on-fail rollback`, a failed step triggers the rollback of each previously
// applied step in reverse order before stopping that server.
//
// Targets are resolved as: an explicit positional <server>, else --group EXPR
// (a tag expression like role=plesk), else the playbook's own `hosts`
// expression. --dry-run prints the resolved plan and runs nothing.
func newRunCommand(configDir *string) *cobra.Command {
	var group string
	var onFail string
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "run <playbook.yaml> [<server>]",
		Short: "Run an idempotent playbook against one or more servers",
		Long: "Run a playbook of ordered, idempotent steps against target servers.\n\n" +
			"Each step has an optional `check` (exit 0 means already satisfied — skip),\n" +
			"an `apply` command, and an optional `rollback`. With --on-fail rollback, a\n" +
			"failed step rolls back every previously-applied step in reverse order.\n\n" +
			"Targets: a positional <server>, else --group EXPR (e.g. role=plesk), else the\n" +
			"playbook's own `hosts` expression. Use --dry-run to print the plan only.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			explicit := ""
			if len(args) == 2 {
				explicit = args[1]
			}

			onFailRollback, err := parseOnFail(onFail)
			if err != nil {
				return err
			}

			pb, err := core.LoadPlaybook(args[0])
			if err != nil {
				return err
			}

			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			servers, err := app.ListServers()
			if err != nil {
				return err
			}
			allNames := make([]string, 0, len(servers))
			for _, s := range servers {
				allNames = append(allNames, s.Name)
			}

			targets, err := core.ResolveTargets(pb, explicit, group, allNames, core.NewTagStore(*configDir))
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if dryRun {
				printPlan(out, pb, targets)
				return nil
			}

			result := core.RunPlaybook(playbookExec(app), pb, targets, core.RunOptions{
				OnFailRollback: onFailRollback,
				DryRun:         false,
			})
			printPlaybookResult(out, result)
			if result.Failed() {
				return fmt.Errorf("playbook %q failed on one or more servers", pb.Name)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&group, "group", "", "target a group by tag expression (e.g. role=plesk,env=prod)")
	cmd.Flags().StringVar(&onFail, "on-fail", "", "behavior on a failed step: rollback (undo applied steps) or empty (stop)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the resolved plan without running anything")
	return cmd
}

// parseOnFail interprets the --on-fail flag. Only "rollback" (or empty) is
// accepted today.
func parseOnFail(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return false, nil
	case "rollback":
		return true, nil
	default:
		return false, fmt.Errorf("invalid --on-fail %q: expected \"rollback\"", value)
	}
}

// playbookExec adapts app.ExecCommand to core.ExecFn. A transport error is
// returned as the error; a command that ran but exited non-zero is reported via
// the exit code (err == nil).
func playbookExec(app *core.App) core.ExecFn {
	return func(server, command string) (int, string, string, error) {
		res, err := app.ExecCommand(server, command)
		if err != nil {
			return 0, "", "", err
		}
		return res.ExitCode, res.Stdout, res.Stderr, nil
	}
}

// printPlan renders the dry-run plan: every target and the ordered steps.
func printPlan(out interface{ Write([]byte) (int, error) }, pb core.Playbook, targets []string) {
	fmt.Fprintf(out, "Playbook: %s (dry-run)\n", pb.Name)
	fmt.Fprintf(out, "Targets (%d): %s\n", len(targets), strings.Join(targets, ", "))
	fmt.Fprintln(out, "Steps:")
	for i, step := range pb.Steps {
		fmt.Fprintf(out, "  %d. %s\n", i+1, step.Name)
		if strings.TrimSpace(step.Check) != "" {
			fmt.Fprintf(out, "       check:    %s\n", step.Check)
		}
		fmt.Fprintf(out, "       apply:    %s\n", step.Apply)
		if strings.TrimSpace(step.Rollback) != "" {
			fmt.Fprintf(out, "       rollback: %s\n", step.Rollback)
		}
	}
}

// printPlaybookResult renders per-server, per-step outcomes.
func printPlaybookResult(out interface{ Write([]byte) (int, error) }, result core.PlaybookResult) {
	fmt.Fprintf(out, "Playbook: %s\n", result.Playbook)
	for _, sr := range result.Servers {
		status := "ok"
		if sr.Failed {
			status = "FAILED"
		}
		fmt.Fprintf(out, "\n%s [%s]\n", sr.Server, status)
		for _, step := range sr.Steps {
			fmt.Fprintf(out, "  %-16s %s\n", step.Status, step.Name)
			if step.Status == core.StepFailed && strings.TrimSpace(step.Stderr) != "" {
				fmt.Fprintf(out, "                   %s\n", strings.TrimSpace(step.Stderr))
			}
			if step.Detail != "" {
				fmt.Fprintf(out, "                   %s\n", step.Detail)
			}
			// Surface a failed rollback so the operator knows the undo did not
			// cleanly complete; the apply result above is left intact.
			if rb := step.Rollback; rb != nil && (rb.ExitCode != 0 || rb.Detail != "") {
				fmt.Fprintf(out, "                   rollback failed (exit %d)\n", rb.ExitCode)
				if strings.TrimSpace(rb.Stderr) != "" {
					fmt.Fprintf(out, "                   %s\n", strings.TrimSpace(rb.Stderr))
				}
				if rb.Detail != "" {
					fmt.Fprintf(out, "                   %s\n", rb.Detail)
				}
			}
		}
		if sr.Err != "" {
			fmt.Fprintf(out, "  error: %s\n", sr.Err)
		}
	}
}
