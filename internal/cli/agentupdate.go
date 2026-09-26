// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/version"
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
// `fleet sync-agent`), called per server — concurrently within a batch, while
// the batches themselves run one after another so the rollout can stop between
// them; this command only adds the batching + health gate around it.
//
// NOTE: no top-level command is registered here. The main loop should attach this
// under the existing `fleet agent` parent, e.g.
//
//	agent.AddCommand(newAgentUpdateCommand(&configDir))
//
// (root.go already builds that parent and is edited by another agent.)
func newAgentUpdateCommand(configDir *string) *cobra.Command {
	var (
		all          bool
		group        string
		canary       int
		strictHealth bool
	)
	cmd := &cobra.Command{
		Use:   "update [--all | --group EXPR] [--canary N]",
		Short: "Roll out agent updates in a health-gated canary order",
		Long: "Update managed agents in a rolling, health-gated manner.\n\n" +
			"A first canary batch of N servers is updated and then re-probed; only if\n" +
			"every canary agent reconnects, answers, and reports the expected version\n" +
			"does the rollout continue to the rest of the fleet. If any canary fails to\n" +
			"update or does not come back on the expected version, the rollout aborts\n" +
			"before the remaining servers are touched.\n\n" +
			"Host conditions (no swap, high load, a full disk, a pending reboot, clock\n" +
			"skew) are not affected by an agent update: they are reported for the canary\n" +
			"but do not stop the rollout unless --strict-health is given.\n\n" +
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
			return runAgentUpdate(cmd, *configDir, group, canary, strictHealth)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "update every server (the default)")
	cmd.Flags().StringVar(&group, "group", "", "update only servers whose tags match EXPR (e.g. role=web,env=prod)")
	cmd.Flags().IntVar(&canary, "canary", 1, "number of servers to update and health-check first before the rest (0 = no canary gate)")
	cmd.Flags().BoolVar(&strictHealth, "strict-health", false, "also abort when a canary host fails a health check (swap, load, disk, reboot, clock), not only when its agent does not come back")
	return cmd
}

// runAgentUpdate resolves the target servers, then drives the canary-gated
// rollout: update the canary batch, re-probe it, and (only on success) update
// the remainder.
func runAgentUpdate(cmd *cobra.Command, configDir, group string, canary int, strictHealth bool) error {
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
		results, err := updateBatch(cmd, app, canaryGroup)
		if err != nil {
			return fmt.Errorf("canary update failed, aborting rollout: %w", err)
		}
		if err := verifyCanary(cmd, app, canaryGroup, results, strictHealth); err != nil {
			return fmt.Errorf("canary health check failed, aborting rollout: %w", err)
		}
		fmt.Fprintf(out, "canary OK (%d server(s) reconnected on the expected agent version)\n\n", len(canaryGroup))
	}

	if len(rest) == 0 {
		fmt.Fprintf(out, "rollout complete: %d server(s) updated\n", len(canaryGroup))
		return nil
	}

	fmt.Fprintf(out, "rolling out to remaining %d server(s): %s\n", len(rest), strings.Join(rest, ", "))
	if _, err := updateBatch(cmd, app, rest); err != nil {
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

// agentUpdateParallelism bounds how many servers of one batch are updated at
// once — the same bound App.SyncAgent applies to a multi-server sync.
const agentUpdateParallelism = 8

// updateBatch updates the agent on every server of one batch, reusing
// App.SyncAgent per server (so one server's lookup failure cannot fail the
// others). Servers within the batch update concurrently (bounded by
// agentUpdateParallelism) — they used to go strictly one at a time, bypassing
// SyncAgent's own pool — while batches stay sequential, so the canary gate
// still runs between them. Result lines are printed in batch order as soon as
// each prefix of the batch is done, identical to the sequential output, and
// the returned error lists every failed server in order.
func updateBatch(cmd *cobra.Command, app *core.App, servers []string) ([]core.SyncAgentResult, error) {
	out := cmd.OutOrStdout()
	lines := make([]string, len(servers))
	failedAt := make([]bool, len(servers))
	results := make([]core.SyncAgentResult, len(servers))
	flusher := core.NewOrderedFlusher(len(servers), func(i int) {
		fmt.Fprint(out, lines[i])
	})
	core.ForEachLimit(len(servers), agentUpdateParallelism, func(i int) {
		lines[i], failedAt[i], results[i] = updateOneAgent(cmd, app, servers[i])
		flusher.Done(i)
	})
	var failed []string
	for i, name := range servers {
		if failedAt[i] {
			failed = append(failed, name)
		}
	}
	if len(failed) > 0 {
		return results, fmt.Errorf("%d server(s) failed: %s", len(failed), strings.Join(failed, ", "))
	}
	return results, nil
}

// updateOneAgent syncs one server's agent and returns its result line, whether
// it failed, and the sync result.
func updateOneAgent(cmd *cobra.Command, app *core.App, name string) (string, bool, core.SyncAgentResult) {
	res, err := app.SyncAgent(cmd.Context(), []string{name}, nil)
	if err != nil {
		return fmt.Sprintf("  %-24s ERROR  %v\n", name, err), true, core.SyncAgentResult{Server: name}
	}
	// SyncAgent on a single server yields exactly one agent result.
	if len(res.Agents) == 0 {
		return fmt.Sprintf("  %-24s ERROR  no result returned\n", name), true, core.SyncAgentResult{Server: name}
	}
	a := res.Agents[0]
	displayVersion := version.DisplaySemVer(a.AgentVersion)
	switch {
	case a.Error != "":
		return fmt.Sprintf("  %-24s ERROR  %s\n", name, a.Error), true, a
	case a.AlreadySynced:
		return fmt.Sprintf("  %-24s up-to-date (%s)\n", name, displayVersion), false, a
	case a.Updated:
		return fmt.Sprintf("  %-24s updated -> %s\n", name, displayVersion), false, a
	default:
		return fmt.Sprintf("  %-24s processed (%s)\n", name, displayVersion), false, a
	}
}

// Canary gate timing: an updated agent restarts, so it gets a while to come
// back. Variables so tests can shorten them.
var (
	canaryWaitTimeout  = 90 * time.Second
	canaryPollInterval = 2 * time.Second
)

// canaryStatus is what the gate observed for one canary server.
type canaryStatus struct {
	server    string
	reachable bool
	version   string // agent version last reported by the server
	want      string // expected version; "" when activation is pending (not observable yet)
	problem   string // why the server does not pass the gate ("" = passes)
}

// verifyCanary is the gate that decides whether the rollout proceeds past the
// canary. It checks only what an agent update can break: every canary agent
// must reconnect, answer an RPC, and report the version the update installed
// (for an update whose activation is still pending, answering is all that can
// be checked). Host conditions from the health probe — no swap, load, disk, a
// pending reboot, clock skew — are unrelated to the update: they are printed,
// and block only with --strict-health. Gating on the full health result used to
// abort rollouts on hosts that merely had no swap, even when nothing changed.
func verifyCanary(cmd *cobra.Command, app *core.App, servers []string, results []core.SyncAgentResult, strictHealth bool) error {
	out := cmd.OutOrStdout()
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	statuses := make([]canaryStatus, len(servers))
	core.ForEachLimit(len(servers), agentUpdateParallelism, func(i int) {
		want := ""
		if i < len(results) && !results[i].ActivationPending {
			want = results[i].AgentVersion
		}
		statuses[i] = waitForCanary(ctx, app, servers[i], want)
	})

	var reachable []string
	for _, st := range statuses {
		if st.reachable {
			reachable = append(reachable, st.server)
		}
	}
	host := map[string]core.HealthResult{}
	for _, r := range core.EvaluateHealth(app.ExecCommand, reachable, core.DefaultHealthThresholds(), time.Now().UTC()).Results {
		host[r.Server] = r
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  SERVER\tREACHABLE\tAGENT\tHOST CHECKS")
	var failed, hostWarn []string
	for _, st := range statuses {
		agent := version.DisplaySemVer(st.version)
		if st.problem != "" {
			agent += " (" + st.problem + ")"
			failed = append(failed, st.server)
		} else if st.want == "" {
			agent += " (activation pending)"
		}
		checks := "-"
		if r, ok := host[st.server]; ok {
			checks = "ok"
			if probs := r.Problems(); len(probs) > 0 {
				labels := make([]string, 0, len(probs))
				for _, p := range probs {
					labels = append(labels, core.CheckLabel(p))
				}
				checks = strings.Join(labels, ",")
				hostWarn = append(hostWarn, st.server)
			}
		}
		fmt.Fprintf(w, "  %s\t%t\t%s\t%s\n", st.server, st.reachable, agent, checks)
	}
	_ = w.Flush()
	if len(failed) > 0 {
		return fmt.Errorf("%d canary server(s) did not come back on the expected agent version: %s", len(failed), strings.Join(failed, ", "))
	}
	if len(hostWarn) > 0 {
		if strictHealth {
			return fmt.Errorf("%d canary server(s) failed host health checks (--strict-health): %s", len(hostWarn), strings.Join(hostWarn, ", "))
		}
		fmt.Fprintf(out, "note: host checks reported problems on %s (not caused by the update; not blocking — pass --strict-health to gate on them)\n", strings.Join(hostWarn, ", "))
	}
	return nil
}

// waitForCanary polls one server until its agent reconnects (a fresh hello,
// see App.ProbeAgent), answers an RPC, and reports want — or until
// canaryWaitTimeout passes (an updated agent needs a moment to restart).
func waitForCanary(ctx context.Context, app *core.App, name, want string) canaryStatus {
	st := canaryStatus{server: name, want: want}
	deadline := time.Now().Add(canaryWaitTimeout)
	for {
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		hello, err := app.ProbeAgent(callCtx, name)
		if err == nil {
			// Only the round trip matters, not the command's exit status (`true`
			// is not a cmd.exe command on Windows, but the agent still answers).
			_, err = app.ExecCommandContext(callCtx, name, "true")
		}
		cancel()
		if err != nil {
			st.reachable, st.problem = false, "unreachable: "+err.Error()
		} else {
			st.reachable, st.problem, st.version = true, "", hello.AgentVersion
			if want == "" || version.Canonical(st.version) == version.Canonical(want) {
				return st
			}
			st.problem = "expected " + version.DisplaySemVer(want)
		}
		if !time.Now().Before(deadline) {
			return st
		}
		select {
		case <-ctx.Done():
			return st
		case <-time.After(canaryPollInterval):
		}
	}
}
