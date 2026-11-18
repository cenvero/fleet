// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// Replaceable in tests.
var agentAutoSyncSupported = core.AgentAutoSyncSupported

// Automatic runs are started by the background helper's agent-sync task (see
// internal/core/helper_tasks.go) and by the daemon; ordinary commands no
// longer launch one, nor print anything about it. How the last run went is in
// `fleet sync-agent auto status` and `fleet helper status`.

func newSyncAgentAutoCommand(configDir *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:       "auto [on|off|status]",
		Short:     "Show or switch automatic hourly agent sync (on by default)",
		ValidArgs: []string{"on", "off", "status"},
		Long: `Fleet keeps managed agents on the controller's version automatically. The
background helper (see ` + "`fleet helper status`" + `), which every fleet command starts,
runs ` + "`fleet sync-agent`" + ` hourly; a running daemon (fleet start) does it instead
while it runs, and also syncs an agent as soon as it connects with an older
version. Only agents known to be older than the controller are touched.

  fleet sync-agent auto          show status (same as "status")
  fleet sync-agent auto off      stop syncing automatically
  fleet sync-agent auto on       start again

FLEET_AGENT_AUTOSYNC=off in the environment the helper starts from (your shell
profile, say) keeps it from syncing without changing the setting; it takes
effect for a helper that is already running after ` + "`fleet helper restart`" + `.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			action := "status"
			if len(args) == 1 {
				action = strings.ToLower(strings.TrimSpace(args[0]))
			}
			out := cmd.OutOrStdout()
			switch action {
			case "status":
			case "on", "off":
				on := action == "on"
				if err := core.SetAgentAutoSync(*configDir, on); err != nil {
					return err
				}
				if on {
					fmt.Fprintln(out, "Agent auto-sync is on: agents are synced to the controller's version every hour.")
				} else {
					fmt.Fprintln(out, "Agent auto-sync is off. Run `fleet sync-agent` to sync agents yourself.")
				}
				return nil
			default:
				return fmt.Errorf("unknown argument %q: use on, off or status", args[0])
			}

			cfg, err := core.LoadConfig(core.ConfigPath(*configDir))
			if err != nil {
				return err
			}
			status := core.ReadAgentAutoSyncStatus(*configDir, cfg)
			status.Supported = agentAutoSyncSupported()
			if asJSON {
				return writeJSON(cmd, status)
			}
			printAgentAutoSyncStatus(out, status)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the status as JSON")
	return cmd
}

func printAgentAutoSyncStatus(w io.Writer, s core.AgentAutoSyncStatus) {
	state := "on"
	switch {
	case !s.Supported:
		state = "unavailable in this development build"
	case !s.Enabled:
		state = "off (turn on: fleet sync-agent auto on)"
	}
	fmt.Fprintf(w, "Agent auto-sync:  %s\n", state)
	fmt.Fprintf(w, "Interval:         %s\n", s.Interval)
	if s.State.LastStartedAt.IsZero() {
		fmt.Fprintln(w, "Last run:         never")
	} else {
		fmt.Fprintf(w, "Last run:         %s (%s)\n", s.State.LastStartedAt.Local().Format("2006-01-02 15:04 MST"), s.State.LastTrigger)
		if !s.State.LastFinishedAt.IsZero() {
			fmt.Fprintf(w, "Result:           %d updated/delivered, %d up to date, %d failed, %d skipped\n",
				s.State.LastSynced, s.State.LastUpToDate, s.State.LastFailed, s.State.LastSkipped)
		}
		if len(s.State.LastFailedNames) > 0 {
			fmt.Fprintf(w, "Failed:           %s\n", strings.Join(s.State.LastFailedNames, ", "))
		}
		if s.State.LastError != "" {
			fmt.Fprintf(w, "Error:            %s\n", s.State.LastError)
		}
	}
	pending := make([]string, 0, len(s.State.Pending))
	for name := range s.State.Pending {
		pending = append(pending, name)
	}
	sort.Strings(pending)
	for _, name := range pending {
		fmt.Fprintf(w, "Awaiting restart: %s (agent %s delivered; restart its service to activate)\n", name, s.State.Pending[name])
	}
	if s.Supported && s.Enabled {
		if s.NextDueAt.IsZero() || !s.NextDueAt.After(time.Now()) {
			fmt.Fprintln(w, "Next run:         due now (the background helper or the daemon starts it within minutes)")
		} else {
			fmt.Fprintf(w, "Next run:         %s\n", s.NextDueAt.Local().Format("2006-01-02 15:04 MST"))
		}
	}
	if s.Running {
		fmt.Fprintln(w, "Running now:      yes")
	}
	fmt.Fprintf(w, "Log:              %s\n", s.LogPath)
}
