// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// appExec adapts App.ExecCommand to the core.ExecFunc shape the job engine uses.
func appExec(app *core.App) core.ExecFunc {
	return func(server, command string) (string, int, error) {
		res, err := app.ExecCommand(server, command)
		if err != nil {
			return "", 0, err
		}
		return res.Stdout, res.ExitCode, nil
	}
}

// parseJobID parses a job id argument into an int.
func parseJobID(arg string) (int, error) {
	id, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid job id %q", arg)
	}
	return id, nil
}

// newJobCommand returns the `fleet job` parent (run/status/wait/logs).
func newJobCommand(configDir *string) *cobra.Command {
	jobCmd := &cobra.Command{
		Use:   "job",
		Short: "Run and track detached background jobs on a server",
		Long: `Background jobs run a shell command in a detached process on a server and
capture its output to a logfile, so the command keeps running even if the
controller disconnects.

  fleet job run <server> <command>   start a job; prints its id
  fleet job status <id>              show a job's state (running/done + exit)
  fleet job wait <id> [--timeout d]  block until the job finishes
  fleet job logs <id> [--follow]     print (or follow) the job's output
  fleet jobs                         list all tracked jobs`,
	}

	jobCmd.AddCommand(newJobRunCommand(configDir))
	jobCmd.AddCommand(newJobStatusCommand(configDir))
	jobCmd.AddCommand(newJobWaitCommand(configDir))
	jobCmd.AddCommand(newJobLogsCommand(configDir))
	return jobCmd
}

// newJobsListCommand returns the top-level `fleet jobs` list command.
func newJobsListCommand(configDir *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "List tracked background jobs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// `fleet jobs` only reads the local jobs.json store, so it must not
			// open the app (which requires an initialized host); it is in the
			// pre-init allowlist and needs no server/SSH connection.
			store := core.NewJobStore(*configDir)
			jobs, err := store.List()
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd, jobs)
			}
			return writeJobsTable(cmd, jobs)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON instead of a table")
	return cmd
}

func newJobRunCommand(configDir *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:          "run <server> <command>",
		Short:        "Start a detached background job on a server",
		Args:         cobra.MinimumNArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			command := strings.Join(args[1:], " ")
			// `job run` executes an arbitrary operator-supplied command on a server,
			// so it MUST pass the same cmd-policy deny/confirm gate that `exec` does;
			// otherwise it was a complete bypass of the policy. job run has no
			// --confirm flag, so a confirm-required pattern blocks it (fail-safe).
			if err := enforceCmdPolicy(*configDir, command, false); err != nil {
				return err
			}
			store := core.NewJobStore(*configDir)
			rec, err := store.Start(appExec(app), args[0], command)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd, rec)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "started job %d on %s (log: %s)\n", rec.ID, rec.Server, rec.Logfile)
			fmt.Fprintf(cmd.OutOrStdout(), "track it with: fleet job status %d\n", rec.ID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output the job record as JSON")
	return cmd
}

func newJobStatusCommand(configDir *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:          "status <id>",
		Short:        "Show a job's status (detects completion + exit code)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseJobID(args[0])
			if err != nil {
				return err
			}
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			store := core.NewJobStore(*configDir)
			rec, _, err := store.Tail(appExec(app), id)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd, rec)
			}
			return writeJobsTable(cmd, []core.JobRecord{rec})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output the job record as JSON")
	return cmd
}

func newJobWaitCommand(configDir *string) *cobra.Command {
	var timeout, poll time.Duration
	var asJSON bool
	cmd := &cobra.Command{
		Use:          "wait <id>",
		Short:        "Block until a job finishes, then report its exit code",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseJobID(args[0])
			if err != nil {
				return err
			}
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			store := core.NewJobStore(*configDir)
			rec, err := store.Wait(appExec(app), id, timeout, poll, nil)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd, rec)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "job %d done (exit %d)\n", rec.ID, rec.ExitCode)
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up after this duration (0 = wait forever)")
	cmd.Flags().DurationVar(&poll, "poll", 2*time.Second, "interval between status checks")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output the final job record as JSON")
	return cmd
}

func newJobLogsCommand(configDir *string) *cobra.Command {
	var follow bool
	var poll time.Duration
	cmd := &cobra.Command{
		Use:          "logs <id>",
		Short:        "Print (or follow) a job's captured output",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseJobID(args[0])
			if err != nil {
				return err
			}
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			store := core.NewJobStore(*configDir)
			exec := appExec(app)

			if !follow {
				_, output, err := store.Tail(exec, id)
				if err != nil {
					return err
				}
				if output != "" {
					fmt.Fprintln(cmd.OutOrStdout(), output)
				}
				return nil
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			printed := 0
			for {
				rec, output, err := store.Tail(exec, id)
				if err != nil {
					return err
				}
				// Print only the newly appended tail since the last poll.
				if len(output) > printed {
					fmt.Fprint(cmd.OutOrStdout(), output[printed:])
					printed = len(output)
				}
				if rec.Status == core.JobDone {
					fmt.Fprintln(cmd.OutOrStdout())
					fmt.Fprintf(cmd.OutOrStdout(), "-- job %d done (exit %d) --\n", rec.ID, rec.ExitCode)
					return nil
				}
				select {
				case <-ctx.Done():
					fmt.Fprintln(cmd.OutOrStdout())
					return nil
				case <-time.After(poll):
				}
			}
		},
	}
	cmd.Flags().BoolVar(&follow, "follow", false, "stream new output until the job finishes")
	cmd.Flags().DurationVar(&poll, "poll", 2*time.Second, "interval between log reads when following")
	return cmd
}

// writeJobsTable renders job records as an aligned table.
func writeJobsTable(cmd *cobra.Command, jobs []core.JobRecord) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "ID\tSERVER\tSTATUS\tEXIT\tSTARTED\tCOMMAND"); err != nil {
		return err
	}
	for _, j := range jobs {
		exit := "-"
		if j.Status == core.JobDone {
			exit = strconv.Itoa(j.ExitCode)
		}
		started := j.Started.Local().Format("2006-01-02 15:04:05")
		if _, err := fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n",
			j.ID, j.Server, j.Status, exit, started, truncateCommand(j.Command)); err != nil {
			return err
		}
	}
	return w.Flush()
}

// truncateCommand shortens long commands for single-line table display.
func truncateCommand(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	const max = 60
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}
