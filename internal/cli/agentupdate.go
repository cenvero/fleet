// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// newAgentUpdateCommand builds `fleet agent update [--all|--group EXPR] [--canary N]`.
//
// It performs a HEALTH-GATED rolling agent update: it first updates a small
// "canary" batch of N servers, re-probes them for reachability/health, and only
// proceeds to update the remaining servers if every canary came back healthy. If
// any canary fails to update or fails the post-update health probe, the rollout
// is aborted before the rest of the fleet is touched.
//
// The per-server work reuses App.SyncAgent (the same mechanism behind
// `fleet sync-agent`), called one server at a time so the rollout can stop
// between batches; this command only adds the batching + health gate around it.
//
// NOTE: no top-level command is registered here. The main loop should attach this
// under the existing `fleet agent` parent, e.g.
//
//	agent.AddCommand(newAgentUpdateCommand(&configDir))
//
// (root.go already builds that parent and is edited by another agent.)
func newAgentUpdateCommand(configDir *string) *cobra.Command {
	var (
		all    bool
		group  string
		canary int
	)
	cmd := &cobra.Command{
		Use:   "update [--all | --group EXPR] [--canary N]",
		Short: "Roll out agent updates in a health-gated canary order",
		Long: "Update managed agents in a rolling, health-gated manner.\n\n" +
			"A first canary batch of N servers is updated and then re-probed; only if\n" +
			"every canary is reachable and healthy does the rollout continue to the rest\n" +
			"of the fleet. If any canary fails to update or fails its health re-probe, the\n" +
			"rollout aborts before the remaining servers are touched.\n\n" +
			"Targets default to every server; narrow them with --group (a tag expression)\n" +
			"or state --all explicitly.\n\n" +
			"  fleet agent update                         # all servers, canary of 1\n" +
			"  fleet agent update --all --canary 2        # all servers, canary of 2\n" +
			"  fleet agent update --group role=web        # only servers tagged role=web\n" +
			"  fleet agent update --canary 0              # no canary gate; update all at once",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if all && strings.TrimSpace(group) != "" {
				return fmt.Errorf("pass either --all or --group, not both")
			}
			if canary < 0 {
				return fmt.Errorf("--canary must be >= 0")
			}
			return runAgentUpdate(cmd, *configDir, group, canary)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "update every server (the default)")
	cmd.Flags().StringVar(&group, "group", "", "update only servers whose tags match EXPR (e.g. role=web,env=prod)")
	cmd.Flags().IntVar(&canary, "canary", 1, "number of servers to update and health-check first before the rest (0 = no canary gate)")
	return cmd
}

// runAgentUpdate resolves the target servers, then drives the canary-gated
// rollout: update the canary batch, re-probe it, and (only on success) update
// the remainder.
func runAgentUpdate(cmd *cobra.Command, configDir, group string, canary int) error {
	app, err := openApp(configDir)
	if err != nil {
		return err
	}
	defer app.Close()

	servers, err := selectAgentUpdateServers(app, configDir, group)
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		if strings.TrimSpace(group) != "" {
			return fmt.Errorf("no servers match %q", group)
		}
		return fmt.Errorf("no servers registered")
	}

	out := cmd.OutOrStdout()

	// Clamp the canary size to the fleet size. A canary >= the whole set just
	// means "update everything as the canary" with no second phase.
	canarySize := canary
	if canarySize > len(servers) {
		canarySize = len(servers)
	}

	canaryGroup := servers[:canarySize]
	rest := servers[canarySize:]

	if len(canaryGroup) > 0 {
		fmt.Fprintf(out, "canary: updating %d/%d server(s): %s\n", len(canaryGroup), len(servers), strings.Join(canaryGroup, ", "))
		if err := updateBatch(cmd, app, canaryGroup); err != nil {
			return fmt.Errorf("canary update failed, aborting rollout: %w", err)
		}
		if err := verifyHealthy(cmd, app, canaryGroup); err != nil {
			return fmt.Errorf("canary health check failed, aborting rollout: %w", err)
		}
		fmt.Fprintf(out, "canary OK (%d server(s) reachable and healthy)\n\n", len(canaryGroup))
	}

	if len(rest) == 0 {
		fmt.Fprintf(out, "rollout complete: %d server(s) updated\n", len(canaryGroup))
		return nil
	}

	fmt.Fprintf(out, "rolling out to remaining %d server(s): %s\n", len(rest), strings.Join(rest, ", "))
	if err := updateBatch(cmd, app, rest); err != nil {
		return fmt.Errorf("rollout to remaining servers failed: %w", err)
	}
	fmt.Fprintf(out, "rollout complete: %d server(s) updated\n", len(servers))
	return nil
}

// selectAgentUpdateServers resolves the target server set, applying an optional
// --group tag filter via TagStore.ServersMatching. An empty group means all
// servers. Names are returned in sorted order so the canary slice is stable.
func selectAgentUpdateServers(app *core.App, configDir, group string) ([]string, error) {
	records, err := app.ListServers()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(records))
	for _, r := range records {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	if strings.TrimSpace(group) == "" {
		return names, nil
	}
	return core.NewTagStore(configDir).ServersMatching(group, names)
}

// updateBatch updates the agent on each server in order, reusing App.SyncAgent
// (called per server so the rollout can gate between batches). It prints a line
// per server and returns an error listing every server whose update failed.
func updateBatch(cmd *cobra.Command, app *core.App, servers []string) error {
	out := cmd.OutOrStdout()
	var failed []string
	for _, name := range servers {
		res, err := app.SyncAgent(cmd.Context(), []string{name}, nil)
		if err != nil {
			fmt.Fprintf(out, "  %-24s ERROR  %v\n", name, err)
			failed = append(failed, name)
			continue
		}
		// SyncAgent on a single server yields exactly one agent result.
		if len(res.Agents) == 0 {
			fmt.Fprintf(out, "  %-24s ERROR  no result returned\n", name)
			failed = append(failed, name)
			continue
		}
		a := res.Agents[0]
		switch {
		case a.Error != "":
			fmt.Fprintf(out, "  %-24s ERROR  %s\n", name, a.Error)
			failed = append(failed, name)
		case a.AlreadySynced:
			fmt.Fprintf(out, "  %-24s up-to-date (%s)\n", name, a.AgentVersion)
		case a.Updated:
			fmt.Fprintf(out, "  %-24s updated -> %s\n", name, a.AgentVersion)
		default:
			fmt.Fprintf(out, "  %-24s processed (%s)\n", name, a.AgentVersion)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d server(s) failed: %s", len(failed), strings.Join(failed, ", "))
	}
	return nil
}

// verifyHealthy re-probes each server and fails if any is unreachable or
// unhealthy. This is the gate that decides whether the rollout proceeds past the
// canary: an updated agent that did not come back reachable/healthy stops it.
func verifyHealthy(cmd *cobra.Command, app *core.App, servers []string) error {
	out := cmd.OutOrStdout()
	th := core.DefaultHealthThresholds()
	report := core.EvaluateHealth(app.ExecCommand, servers, th, time.Now().UTC())

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  SERVER\tREACHABLE\tHEALTHY")
	var unhealthy []string
	for _, r := range report.Results {
		fmt.Fprintf(w, "  %s\t%t\t%t\n", r.Server, r.Reachable, r.Healthy)
		if !r.Reachable || !r.Healthy {
			unhealthy = append(unhealthy, r.Server)
		}
	}
	_ = w.Flush()
	if len(unhealthy) > 0 {
		return fmt.Errorf("%d canary server(s) not healthy after update: %s", len(unhealthy), strings.Join(unhealthy, ", "))
	}
	return nil
}
