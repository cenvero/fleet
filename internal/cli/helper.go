// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// The background helper (see internal/core/helper.go) is started by every
// fleet command and stopped by the ones that replace or remove the controller.

// Replaceable in tests.
var (
	helperSupported     = core.HelperSupported
	launchHelperProcess = core.StartHelperProcess
	stopHelperProcess   = core.StopHelper
)

// helperPreRunStopWait bounds how long a command that replaces the
// controller waits for the helper to exit before it carries on.
const helperPreRunStopWait = 5 * time.Second

// helperRestartWait bounds how long `fleet helper restart` waits for the new
// helper to come up.
const helperRestartWait = 5 * time.Second

// helperSkip lists the command paths (below `fleet`) that never start the
// helper, mirroring agent auto-sync's old skip list: the helper's own
// commands, commands that create, replace or remove the controller's config
// or binary, and shell completion, which runs on every Tab press.
var helperSkip = map[string]bool{
	core.HelperCommand: true, "helper": true,
	"init": true, "adjust-init": true, "self-uninstall": true, "recover": true, "backup": true,
	"config restore": true, "config import": true,
	"update": true, "completion": true, "__complete": true, "__completeNoDesc": true,
}

// helperStopFor lists the commands that replace or remove the controller's
// binary or config dir. They stop the helper before they run (it would hold
// the old binary, or keep writing into a dir being replaced); the next
// ordinary command starts it again.
var helperStopFor = map[string]bool{
	"self-uninstall": true, "recover": true, "config restore": true,
	"update apply": true, "update rollback": true,
}

func skipHelperFor(cmd *cobra.Command) bool {
	path := commandSecurityPath(cmd)
	if helperSkip[path] {
		return true
	}
	top, _, _ := strings.Cut(path, " ")
	return helperSkip[top]
}

// tokenPresented reports whether this invocation carries a --token or
// FLEET_TOKEN.
func tokenPresented(cmd *cobra.Command, tokenFlag string) bool {
	return strings.TrimSpace(tokenFlag) != "" || resolveTokenID(cmd) != ""
}

// ensureHelper runs before every fleet command (root.go's pre-run). It starts
// the background helper when it isn't running — one lock probe and nothing
// else when it is — and, for the commands in helperStopFor, stops it instead.
// It never prints anything and never fails the command: a failure to start the
// helper is recorded in data/helper-status.json for `fleet helper status`.
//
// It does not start the helper for a development build, before init, for the
// commands in helperSkip, or for a command run with --token / FLEET_TOKEN: a
// scoped credential must not start fleet-wide work, even indirectly. Jobs
// started under a token are tracked once any unscoped command (or the daemon)
// runs.
func ensureHelper(cmd *cobra.Command, configDir, tokenFlag string) {
	if helperStopFor[commandSecurityPath(cmd)] {
		_, _ = stopHelperProcess(configDir, helperPreRunStopWait)
		return
	}
	if !helperSupported() || skipHelperFor(cmd) || tokenPresented(cmd, tokenFlag) || !core.IsInitialized(configDir) {
		return
	}
	_, _ = core.EnsureHelper(configDir, launchHelperProcess)
}

// newHelperRunCommand is the hidden `fleet __helper` the helper runs as.
func newHelperRunCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:    core.HelperCommand,
		Short:  "Run the background helper (fleet starts it automatically)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if resolveTokenID(cmd) != "" {
				return errors.New("the background helper never runs under a token (--token / FLEET_TOKEN)")
			}
			if !helperSupported() {
				return errors.New("the background helper does not run in a development build")
			}
			// A helper started from a copy of the binary (Windows) watches the
			// installed one, so an upgrade still ends it.
			err := core.RunHelper(cmd.Context(), *configDir, core.HelperOptions{Log: cmd.ErrOrStderr(), Executable: os.Getenv(core.HelperSourceExeEnv)})
			switch {
			case errors.Is(err, core.ErrHelperRunning):
				return nil // another helper got there first
			case errors.Is(err, core.ErrHelperBinaryChanged), errors.Is(err, core.ErrHelperConfigGone):
				return nil // exiting by design; the next command starts a fresh one
			}
			return err
		},
	}
}

// newHelperCommand is `fleet helper`.
func newHelperCommand(configDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "helper",
		Short: "Inspect the background helper (job tracking, agent auto-sync, job-log pruning)",
		Long: `Fleet runs one small background helper per config dir. It keeps checking
running jobs (so ` + "`fleet jobs`" + ` is current and job-failed notifications fire), runs
the hourly agent auto-sync, and prunes old job logs. Every fleet command starts
it when it isn't running, and it exits by itself when fleet is upgraded; there
is nothing to turn on or off. Each task follows its own setting instead (for
example ` + "`fleet sync-agent auto off`" + `).

  fleet helper status [--json]   is it running, and what each task last did
  fleet helper restart           restart it (after changing its environment)

It is not the controller daemon (` + "`fleet start`" + `), which is optional.`,
	}
	cmd.AddCommand(newHelperStatusCommand(configDir))
	cmd.AddCommand(newHelperRestartCommand(configDir))
	return cmd
}

func newHelperStatusCommand(configDir *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the background helper runs, and each task's last and next run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			state, err := core.ReadHelperState(*configDir)
			if err != nil {
				return err
			}
			state.Supported = helperSupported()
			if asJSON {
				return writeJSON(cmd, state)
			}
			printHelperState(cmd.OutOrStdout(), state)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the status as JSON")
	return cmd
}

func newHelperRestartCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the background helper",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if tokenPresented(cmd, "") {
				return errors.New("`fleet helper restart` cannot run under a token (--token / FLEET_TOKEN): the helper does fleet-wide work")
			}
			if !helperSupported() {
				return errors.New("the background helper does not run in a development build")
			}
			if err := core.RestartHelper(*configDir, launchHelperProcess, helperRestartWait); err != nil {
				return err
			}
			msg := "Background helper restarted."
			if pid, err := core.ReadHelperPID(*configDir); err == nil {
				msg = fmt.Sprintf("Background helper restarted (pid %d).", pid)
			}
			fmt.Fprintln(cmd.OutOrStdout(), msg)
			return nil
		},
	}
}

func printHelperState(w io.Writer, s core.HelperState) {
	st := s.Status
	switch {
	case !s.Supported:
		fmt.Fprintln(w, "Background helper: not available in a development build")
	case s.Running:
		details := []string{}
		if s.PID > 0 {
			details = append(details, fmt.Sprintf("pid %d", s.PID))
		}
		if s.Uptime != "" {
			details = append(details, "up "+s.Uptime)
		}
		if st.Version != "" {
			details = append(details, st.Version)
		}
		line := "Background helper: running"
		if len(details) > 0 {
			line += " (" + strings.Join(details, ", ") + ")"
		}
		fmt.Fprintln(w, line)
	default:
		fmt.Fprintln(w, "Background helper: not running (the next fleet command starts it)")
		if !st.StoppedAt.IsZero() && st.StopReason != "" {
			fmt.Fprintf(w, "Last stopped:      %s: %s\n", helperTime(st.StoppedAt), st.StopReason)
		}
	}
	if st.LastLaunchError != "" && st.LastLaunchErrorAt.After(st.StartedAt) {
		fmt.Fprintf(w, "Last start failed: %s: %s\n", helperTime(st.LastLaunchErrorAt), st.LastLaunchError)
	}
	fmt.Fprintf(w, "Log:               %s\n", s.LogPath)
	fmt.Fprintf(w, "Status file:       %s\n\n", s.StatusPath)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TASK\tEVERY\tLAST RUN\tNEXT RUN\tRESULT")
	names := make([]string, 0, len(s.Tasks))
	intervals := map[string]string{}
	for _, t := range s.Tasks {
		names = append(names, t.Name)
		intervals[t.Name] = t.Interval
	}
	// Tasks a newer or older helper recorded but this build doesn't run.
	var extra []string
	for name := range st.Tasks {
		if _, ok := intervals[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	names = append(names, extra...)
	var failures []string
	for _, name := range names {
		ts := st.Tasks[name]
		interval := intervals[name]
		if interval == "" {
			interval = ts.Interval
		}
		last, next, result := "-", "-", "-"
		if !ts.LastRunAt.IsZero() {
			last = helperTime(ts.LastRunAt)
		}
		if s.Running && !ts.NextRunAt.IsZero() {
			next = "due"
			if ts.NextRunAt.After(time.Now()) {
				next = helperTime(ts.NextRunAt)
			}
		}
		if ts.LastResult != "" {
			result = ts.LastResult
			if ts.LastMessage != "" {
				result += ": " + ts.LastMessage
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, shortDuration(interval), last, next, result)
		if ts.LastError != "" {
			failures = append(failures, fmt.Sprintf("  %s (%s): %s", name, helperTime(ts.LastErrorAt), ts.LastError))
		}
	}
	_ = tw.Flush()
	if len(failures) > 0 {
		fmt.Fprintln(w, "\nLast errors:")
		for _, f := range failures {
			fmt.Fprintln(w, f)
		}
	}
}

func helperTime(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04:05")
}

// shortDuration trims a Go duration string's zero units: "24h0m0s" -> "24h".
func shortDuration(s string) string {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return s
	}
	out := d.String()
	if strings.HasSuffix(out, "m0s") {
		out = strings.TrimSuffix(out, "0s")
	}
	if strings.HasSuffix(out, "h0m") {
		out = strings.TrimSuffix(out, "0m")
	}
	return out
}
