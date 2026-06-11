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

// newCronCommand builds `fleet cron` for managing scheduled jobs in a server's
// user crontab. Each managed job lives inside a marker-wrapped block so fleet
// can add/list/remove a single job without disturbing other crontab entries.
//
//	fleet cron add <server> --name <n> --schedule '<5-field cron>' --cmd '<command>'
//	fleet cron list <server>
//	fleet cron rm <server> --name <n>
func newCronCommand(configDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cron",
		Short: "Manage scheduled jobs (cron) on a server",
		Long: "Manage scheduled jobs in a server's user crontab. Fleet wraps each job in\n" +
			"marker comments (# >>> fleet:<name> >>> ... # <<< fleet:<name> <<<) so it can\n" +
			"add, list, and remove a single job without touching hand-written crontab lines.\n\n" +
			"Examples:\n" +
			"  fleet cron add web-01 --name backup --schedule '0 3 * * *' --cmd '/opt/backup.sh'\n" +
			"  fleet cron list web-01\n" +
			"  fleet cron rm web-01 --name backup",
	}
	cmd.AddCommand(newCronAddCommand(configDir))
	cmd.AddCommand(newCronListCommand(configDir))
	cmd.AddCommand(newCronRemoveCommand(configDir))
	return cmd
}

func newCronAddCommand(configDir *string) *cobra.Command {
	var name, schedule, command string
	cmd := &cobra.Command{
		Use:   "add <server>",
		Short: "Add or replace a managed scheduled job on a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name = strings.TrimSpace(name)
			schedule = strings.TrimSpace(schedule)
			command = strings.TrimSpace(command)
			if err := core.ValidateCronName(name); err != nil {
				return err
			}
			if err := core.ValidateCronSchedule(schedule); err != nil {
				return err
			}
			if err := core.ValidateCronCommand(command); err != nil {
				return err
			}

			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			current, err := readCrontab(app, args[0])
			if err != nil {
				return err
			}
			updated := core.UpsertManagedCron(current, core.CronJob{
				Name:     name,
				Schedule: schedule,
				Command:  command,
			})
			if err := writeCrontab(app, args[0], updated); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "scheduled job %q on %s: %s %s\n", name, args[0], schedule, command)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "unique job name (letters, digits, '.', '_', '-')")
	cmd.Flags().StringVar(&schedule, "schedule", "", "5-field cron schedule, e.g. '0 3 * * *'")
	cmd.Flags().StringVar(&command, "cmd", "", "command to run")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("schedule")
	_ = cmd.MarkFlagRequired("cmd")
	return cmd
}

func newCronListCommand(configDir *string) *cobra.Command {
	var group string
	cmd := &cobra.Command{
		Use:   "list <server>",
		Short: "List managed scheduled jobs on a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// --group is accepted for forward compatibility; for now an unknown
			// value is treated as the single named server (args[0]).
			_ = group

			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			current, err := readCrontab(app, args[0])
			if err != nil {
				return err
			}
			jobs := core.ParseManagedCron(current)
			if len(jobs) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "no managed scheduled jobs on %s\n", args[0])
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			if _, err := fmt.Fprintln(w, "NAME\tSCHEDULE\tCOMMAND"); err != nil {
				return err
			}
			for _, j := range jobs {
				if _, err := fmt.Fprintf(w, "%s\t%s\t%s\n", j.Name, j.Schedule, j.Command); err != nil {
					return err
				}
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&group, "group", "", "server group (accepted; treated as the named server for now)")
	return cmd
}

func newCronRemoveCommand(configDir *string) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:     "rm <server>",
		Aliases: []string{"remove", "delete"},
		Short:   "Remove a managed scheduled job from a server",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name = strings.TrimSpace(name)
			if err := core.ValidateCronName(name); err != nil {
				return err
			}

			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			current, err := readCrontab(app, args[0])
			if err != nil {
				return err
			}
			found := false
			for _, j := range core.ParseManagedCron(current) {
				if j.Name == name {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("no managed job named %q on %s", name, args[0])
			}
			updated := core.RemoveManagedCron(current, name)
			if err := writeCrontab(app, args[0], updated); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed scheduled job %q from %s\n", name, args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "name of the job to remove")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

// readCrontab returns the server's current user crontab. A missing crontab
// ("no crontab for <user>") is reported by `crontab -l` with a non-zero exit;
// we treat that as an empty crontab rather than an error.
func readCrontab(app *core.App, server string) (string, error) {
	result, err := app.ExecCommand(server, "crontab -l 2>/dev/null || true")
	if err != nil {
		return "", err
	}
	return result.Stdout, nil
}

// writeCrontab replaces the server's user crontab with content. An empty
// content clears the crontab (`crontab -r`); otherwise it is piped via a quoted
// heredoc so nothing in the schedule or command is shell-expanded.
func writeCrontab(app *core.App, server, content string) error {
	cmd := core.CronWriteCommand(content)
	if strings.TrimSpace(content) == "" {
		// Nothing left to manage: remove the crontab entirely (ignore "no
		// crontab" errors so clearing an already-empty crontab succeeds).
		cmd = "crontab -r 2>/dev/null || true"
	}
	result, err := app.ExecCommand(server, cmd)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		msg := strings.TrimSpace(result.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(result.Stdout)
		}
		return fmt.Errorf("crontab update on %s failed (exit %d): %s", server, result.ExitCode, msg)
	}
	return nil
}
