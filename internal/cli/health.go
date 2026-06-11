// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// FL-020 — fleet health.
//
//	fleet health [--watch] [--json] [--group EXPR] [--disk 85] [--load N]
//
// Probes every selected server (one exec round trip each) and reports a table
// of per-server health flags: agent offline, no swap, disk full (>threshold),
// reboot required, clock skew, and high load. --json emits a machine-readable
// schema (core.HealthReport). --watch refreshes the table in place until Ctrl-C.
// --group filters the server set via TagStore.ServersMatching.
//
// 'status' already exists for the controller, so this command is named 'health'.
//
// newHealthCommand is exported so root.go can register it with
// root.AddCommand(newHealthCommand(&configDir)).
func newHealthCommand(configDir *string) *cobra.Command {
	var (
		watch    bool
		asJSON   bool
		group    string
		diskPct  float64
		loadPer  float64
		interval time.Duration
	)

	cmd := &cobra.Command{
		Use:   "health",
		Short: "Per-server health checks across the fleet (offline, swap, disk, reboot, clock, load)",
		Long: `Run lightweight health checks against every selected server.

Each server is probed once and evaluated for:
  • agent offline   the agent exec failed (unreachable / down)
  • no-swap         SwapTotal is 0 (no swap configured)
  • disk-full       / usage exceeds --disk (default 85%)
  • reboot          /run/reboot-required (or /var/run) is present
  • clock-skew      the remote clock differs from the controller (>5s)
  • high-load       1-minute loadavg per CPU exceeds --load (default 1.0)

Examples:
  fleet health
  fleet health --json
  fleet health --watch --disk 90 --load 2
  fleet health --group role=web`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if interval <= 0 {
				interval = 3 * time.Second
			}
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			servers, err := selectHealthServers(app, *configDir, group)
			if err != nil {
				return err
			}
			if len(servers) == 0 {
				if asJSON {
					return writeJSON(cmd, core.HealthReport{GeneratedAt: time.Now().UTC(), Results: []core.HealthResult{}})
				}
				fmt.Fprintln(cmd.OutOrStdout(), "no servers to check")
				return nil
			}

			th := core.HealthThresholds{DiskPercent: diskPct, LoadPerCPU: loadPer}

			if asJSON {
				// --json implies a single snapshot even with --watch (a streaming
				// JSON feed would not be a stable schema).
				report := collectHealthReport(cmd.Context(), app, servers, th)
				return writeJSON(cmd, report)
			}

			if !watch {
				report := collectHealthReport(cmd.Context(), app, servers, th)
				return renderHealthTable(cmd, report, group, false)
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			return runHealthWatch(ctx, cmd, app, servers, th, group, interval)
		},
	}
	cmd.Flags().BoolVar(&watch, "watch", false, "refresh the table in place until Ctrl-C")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable JSON report")
	cmd.Flags().StringVar(&group, "group", "", "filter servers by tag expression, e.g. role=web,env=prod")
	cmd.Flags().Float64Var(&diskPct, "disk", 85, "disk usage percent on / that flags a server")
	cmd.Flags().Float64Var(&loadPer, "load", 1.0, "1-minute loadavg per CPU that flags a server")
	cmd.Flags().DurationVar(&interval, "interval", 3*time.Second, "refresh interval for --watch")
	return cmd
}

// selectHealthServers resolves the server set, applying an optional --group tag
// filter via TagStore.ServersMatching. An empty group means all servers.
func selectHealthServers(app *core.App, configDir, group string) ([]string, error) {
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
	store := core.NewTagStore(configDir)
	matched, err := store.ServersMatching(group, names)
	if err != nil {
		return nil, err
	}
	return matched, nil
}

// collectHealthReport probes all servers concurrently and evaluates the checks.
// It honours ctx: once ctx is cancelled (e.g. Ctrl-C under --watch) it stops
// waiting on outstanding probes so the command exits promptly rather than
// blocking for the exec timeout on slow or unreachable servers. Probes still in
// flight are recorded as offline/cancelled in the returned report.
func collectHealthReport(ctx context.Context, app *core.App, servers []string, th core.HealthThresholds) core.HealthReport {
	now := time.Now().UTC()
	results := make([]core.HealthResult, len(servers))
	finished := make([]atomic.Bool, len(servers))
	var wg sync.WaitGroup
	for i, name := range servers {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			// Capture the skew reference per server, immediately before this
			// server's own probe, rather than reusing the single pre-launch
			// 'now' for every probe. With many concurrent (or slow) probes that
			// shared timestamp goes stale, inflating measured clock skew; the
			// core then adds half the round trip to land on the probe midpoint.
			probeStart := time.Now().UTC()
			one := core.EvaluateHealth(app.ExecCommand, []string{name}, th, probeStart)
			results[i] = one.Results[0]
			finished[i].Store(true)
		}(i, name)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		// Cancelled before every probe returned: substitute a complete result
		// set so the report is well-formed, and return without blocking on the
		// outstanding (slow / unreachable) probes' exec timeout. We do not touch
		// results[i] for in-flight probes — their goroutines still own those
		// slots — so we build a fresh slice instead.
		out := make([]core.HealthResult, len(servers))
		for i := range servers {
			if finished[i].Load() {
				out[i] = results[i]
			} else {
				out[i] = core.HealthResult{Server: servers[i], AgentOffline: true, Error: ctx.Err().Error()}
			}
		}
		results = out
	}
	return core.HealthReport{
		GeneratedAt: now,
		Thresholds:  normalizedThresholds(th),
		Results:     results,
	}
}

// normalizedThresholds mirrors core's defaulting so the JSON/table header shows
// the effective values even when the flags were left at zero.
func normalizedThresholds(th core.HealthThresholds) core.HealthThresholds {
	// Mirror core.normalizeThresholds: only a negative value is "unset"; an
	// explicit 0 disables that check and is shown as-is in the header.
	d := core.DefaultHealthThresholds()
	if th.DiskPercent < 0 {
		th.DiskPercent = d.DiskPercent
	}
	if th.LoadPerCPU < 0 {
		th.LoadPerCPU = d.LoadPerCPU
	}
	if th.ClockSkew <= 0 {
		th.ClockSkew = d.ClockSkew
	}
	return th
}

func runHealthWatch(ctx context.Context, cmd *cobra.Command, app *core.App, servers []string, th core.HealthThresholds, group string, interval time.Duration) error {
	live := term.IsTerminal(int(os.Stdout.Fd()))
	for {
		report := collectHealthReport(ctx, app, servers, th)
		if err := renderHealthTable(cmd, report, group, live); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			if live {
				fmt.Fprintln(cmd.OutOrStdout())
			}
			return nil
		case <-time.After(interval):
		}
	}
}

func renderHealthTable(cmd *cobra.Command, report core.HealthReport, group string, live bool) error {
	out := cmd.OutOrStdout()
	if live {
		// Clear screen and home the cursor for an in-place refresh.
		fmt.Fprint(out, "\x1b[H\x1b[2J")
	}
	header := fmt.Sprintf("fleet health — %s", report.GeneratedAt.Local().Format("15:04:05"))
	if group != "" {
		header += fmt.Sprintf("  group=%q", group)
	}
	header += fmt.Sprintf("  (disk>%.0f%%, load>%.2f/CPU)", report.Thresholds.DiskPercent, report.Thresholds.LoadPerCPU)
	fmt.Fprintln(out, header)
	if live {
		fmt.Fprintln(out, "(Ctrl-C to exit)")
	}
	fmt.Fprintln(out)

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVER\tSTATUS\tDISK%\tLOAD/CPU\tSWAP\tREBOOT\tCLOCK\tPROBLEMS")
	for _, r := range report.Results {
		fmt.Fprintln(w, formatHealthRow(r))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	healthy, problem := 0, 0
	for _, r := range report.Results {
		if r.Healthy {
			healthy++
		} else {
			problem++
		}
	}
	fmt.Fprintf(out, "\n%d healthy, %d with problems\n", healthy, problem)
	return nil
}

// formatHealthRow renders one tab-separated table line for a server result.
func formatHealthRow(r core.HealthResult) string {
	if r.AgentOffline {
		reason := classifyAgentErrorStr(r.Error)
		return fmt.Sprintf("%s\t%s\t-\t-\t-\t-\t-\t%s", r.Server, reason, "agent offline")
	}

	status := "ok"
	if !r.Healthy {
		status = "WARN"
	}

	disk := "-"
	if r.DiskPercent >= 0 {
		disk = formatPercent(r.DiskPercent)
		if r.DiskFull {
			disk += "!"
		}
	}

	loadPerCPU := formatLoad(r.LoadPerCPU)
	if r.HighLoad {
		loadPerCPU += "!"
	}

	swap := "ok"
	switch {
	case r.SwapTotalKB < 0:
		swap = "?"
	case r.NoSwap:
		swap = "none!"
	}

	reboot := "no"
	if r.RebootRequired {
		reboot = "YES!"
	}

	clock := "ok"
	if r.ClockSkew {
		clock = fmt.Sprintf("%+.0fs!", r.ClockSkewSecs)
	}

	problems := "-"
	if probs := r.Problems(); len(probs) > 0 {
		labels := make([]string, 0, len(probs))
		for _, p := range probs {
			labels = append(labels, core.CheckLabel(p))
		}
		problems = strings.Join(labels, ",")
	}

	return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
		r.Server, status, disk, loadPerCPU, swap, reboot, clock, problems)
}
