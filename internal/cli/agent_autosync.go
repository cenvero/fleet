// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// Replaceable in tests.
var (
	agentAutoSyncSupported   = core.AgentAutoSyncSupported
	startBackgroundAgentSync = core.StartBackgroundAgentSync
	stderrIsTerminal         = func() bool { return term.IsTerminal(int(os.Stderr.Fd())) }
)

// agentAutoSyncSkip lists the command paths (below `fleet`) that never start a
// background agent sync: the sync itself, the daemon (which runs its own),
// commands that create, replace or remove the controller's config or binary,
// and shell completion, which runs on every Tab press.
var agentAutoSyncSkip = map[string]bool{
	"sync-agent": true, "daemon": true,
	"init": true, "self-uninstall": true, "recover": true, "adjust-init": true, "backup": true,
	"config restore": true, "config import": true,
	"update": true, "completion": true, "__complete": true, "__completeNoDesc": true,
}

func skipAgentAutoSyncFor(cmd *cobra.Command) bool {
	path := commandSecurityPath(cmd)
	if agentAutoSyncSkip[path] {
		return true
	}
	top, _, _ := strings.Cut(path, " ")
	return agentAutoSyncSkip[top]
}

// maybeStartBackgroundAgentSync runs before every fleet command: when the last
// agent sync started more than an hour ago it launches `fleet sync-agent
// --background` detached, so out-of-date agents are fixed without anyone
// having to know about `fleet sync-agent`, and the command itself never waits.
//
// It does nothing for a dev build, before init, when auto-sync is turned off
// (`fleet sync-agent auto off`, or FLEET_AGENT_AUTOSYNC=off for one shell),
// or when a --token is in use: a scoped credential must not be able to start a
// fleet-wide agent update, even indirectly.
func maybeStartBackgroundAgentSync(cmd *cobra.Command, configDir, tokenFlag string) {
	if !agentAutoSyncSupported() || skipAgentAutoSyncFor(cmd) {
		return
	}
	if v := strings.TrimSpace(os.Getenv("FLEET_AGENT_AUTOSYNC")); v != "" {
		if on, err := parseOnOff(v); err == nil && !on {
			return
		}
	}
	if strings.TrimSpace(tokenFlag) != "" || strings.TrimSpace(os.Getenv("FLEET_TOKEN")) != "" {
		return
	}
	if !core.IsInitialized(configDir) {
		return
	}
	cfg, err := core.LoadConfigShared(core.ConfigPath(configDir))
	if err != nil || !cfg.Updates.AgentAutoSyncEnabled() {
		return
	}
	previous := core.ReadAgentSyncState(configDir)
	if !core.AgentAutoSyncDue(previous, time.Now()) {
		return // the common case: no lock, no write
	}
	if !core.ClaimAgentAutoSync(configDir, "cli", time.Now()) {
		return // another command or the daemon got there first
	}
	if err := startBackgroundAgentSync(configDir); err != nil {
		// Never fail the operator's command over this; the next due check
		// (an hour on) tries again.
		if stderrIsTerminal() {
			fmt.Fprintf(cmd.ErrOrStderr(), "fleet: could not start the background agent sync: %v\n", err)
		}
		return
	}
	if stderrIsTerminal() {
		printAgentAutoSyncNotice(cmd.ErrOrStderr(), configDir, previous)
	}
}

// printAgentAutoSyncNotice tells an operator at a terminal about the first
// background sync and about failures in the previous one. Hourly runs that
// went fine stay silent.
func printAgentAutoSyncNotice(w io.Writer, configDir string, previous core.AgentSyncState) {
	switch {
	case previous.LastStartedAt.IsZero():
		fmt.Fprintf(w, "↻  Syncing fleet agents to this controller's version in the background (every hour).\n"+
			"   Log: %s  ·  Status: fleet sync-agent auto status  ·  Turn off: fleet sync-agent auto off\n\n",
			core.AgentSyncLogPath(configDir))
	case previous.LastFailed > 0:
		names := strings.Join(previous.LastFailedNames, ", ")
		if names == "" {
			names = "see the log"
		}
		fmt.Fprintf(w, "⚠  The last background agent sync failed for %d server(s): %s.\n"+
			"   Retrying now in the background. Details: %s\n\n",
			previous.LastFailed, names, core.AgentSyncLogPath(configDir))
	}
}

func newSyncAgentAutoCommand(configDir *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:       "auto [on|off|status]",
		Short:     "Show or switch automatic hourly agent sync (on by default)",
		ValidArgs: []string{"on", "off", "status"},
		Long: `Fleet keeps managed agents on the controller's version automatically. Any
fleet command whose last agent sync started more than an hour ago launches
` + "`fleet sync-agent`" + ` in the background, and a running daemon (fleet start)
does the same hourly and syncs an agent as soon as it connects with an older
version. Only agents known to be older than the controller are touched.

  fleet sync-agent auto          show status (same as "status")
  fleet sync-agent auto off      stop syncing automatically
  fleet sync-agent auto on       start again

FLEET_AGENT_AUTOSYNC=off skips the background start for commands run in one
shell without changing the setting.`,
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
			fmt.Fprintln(w, "Next run:         due now (starts with the next fleet command, or the daemon)")
		} else {
			fmt.Fprintf(w, "Next run:         %s\n", s.NextDueAt.Local().Format("2006-01-02 15:04 MST"))
		}
	}
	if s.Running {
		fmt.Fprintln(w, "Running now:      yes")
	}
	fmt.Fprintf(w, "Log:              %s\n", s.LogPath)
}
