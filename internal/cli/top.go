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
	"text/tabwriter"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/pkg/proto"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// FL-023 — live metrics table.
//
//	fleet top [--group EXPR] [--interval 2s]
//
// Renders an in-place table of CPU/mem/swap/disk/load across the selected
// servers, refreshing on an interval until Ctrl-C. Metrics come from
// App.SampleMetrics (the same live snapshot as `fleet server metrics`, without
// an audit entry per server per frame); load is filled in from /proc/loadavg via
// ExecCommand when the snapshot omits it, and swap comes from the snapshot's
// swap fields, falling back to probing `free` for older agents that do not
// report swap.
//
// --group filters the servers with a tag expression (like exec --group).
//
// newTopCommand is exported so root.go can register it with
// root.AddCommand(newTopCommand(&configDir)).

// topRow is one server's line in the live table.
type topRow struct {
	Server    string
	Reachable bool
	Err       string
	CPU       float64
	Mem       float64
	Swap      float64 // percent; -1 when unknown
	Disk      float64
	Load1     float64
	Load5     float64
	Load15    float64
}

func newTopCommand(configDir *string) *cobra.Command {
	var group string
	var interval time.Duration
	var once bool

	cmd := &cobra.Command{
		Use:   "top",
		Short: "Live CPU/mem/swap/disk/load table across servers",
		Long: `Render a live, in-place metrics table across the fleet.

  fleet top
  fleet top --interval 5s
  fleet top --group role=web   # only servers whose tags match the expression
  fleet top --once             # render a single frame and exit (no live refresh)`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if interval <= 0 {
				interval = 2 * time.Second
			}
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			servers, err := selectTopServers(app, *configDir, group)
			if err != nil {
				return err
			}
			if len(servers) == 0 {
				if strings.TrimSpace(group) != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "no servers match --group %q\n", group)
					return nil
				}
				fmt.Fprintln(cmd.OutOrStdout(), "no servers to display")
				return nil
			}

			if once {
				rows := collectTopRows(app, servers)
				return renderTopTable(cmd, rows, group, false)
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			return runTopLoop(ctx, cmd, app, servers, group, interval)
		},
	}
	cmd.Flags().StringVar(&group, "group", "", "only show servers whose tags match EXPR (e.g. role=web)")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "refresh interval")
	cmd.Flags().BoolVar(&once, "once", false, "render a single frame and exit")
	return cmd
}

// selectTopServers resolves the server set for the table: every server, or only
// those whose tags match the --group expression (the same matcher exec, health
// and agent update use). Names are sorted so the table order is stable.
func selectTopServers(app *core.App, configDir, group string) ([]string, error) {
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

func runTopLoop(ctx context.Context, cmd *cobra.Command, app *core.App, servers []string, group string, interval time.Duration) error {
	live := term.IsTerminal(int(os.Stdout.Fd()))
	for {
		rows := collectTopRows(app, servers)
		if err := renderTopTable(cmd, rows, group, live); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			if live {
				// Leave the cursor on a fresh line after the last frame.
				fmt.Fprintln(cmd.OutOrStdout())
			}
			return nil
		case <-time.After(interval):
		}
	}
}

// collectTopRows gathers a metrics snapshot for every server concurrently,
// bounded so a large reverse-mode fleet does not overrun the daemon's control
// socket (which drops connections beyond its limit and showed servers offline).
func collectTopRows(app *core.App, servers []string) []topRow {
	rows := make([]topRow, len(servers))
	core.ForEachLimit(len(servers), core.DefaultFanoutLimit, func(i int) {
		rows[i] = collectTopRow(app, servers[i])
	})
	return rows
}

func collectTopRow(app *core.App, name string) topRow {
	row := topRow{Server: name, Swap: -1}
	// No audit entry per server per frame: top refreshes every couple of
	// seconds and used to flood the audit log.
	snapshot, err := app.SampleMetrics(name)
	if err != nil {
		row.Err = classifyAgentError(err)
		return row
	}
	row.Reachable = true
	row.CPU = snapshot.CPUPercent
	row.Mem = snapshot.MemoryPercent
	row.Disk = snapshot.DiskPercent
	row.Load1 = snapshot.Load1
	row.Load5 = snapshot.Load5
	row.Load15 = snapshot.Load15

	// Some agents omit load; probe /proc/loadavg to fill the gap.
	if row.Load1 == 0 && row.Load5 == 0 && row.Load15 == 0 {
		if l1, l5, l15, ok := probeLoadAvg(app, name); ok {
			row.Load1, row.Load5, row.Load15 = l1, l5, l15
		}
	}
	if pct, ok := snapshotSwapPercent(snapshot); ok {
		row.Swap = pct
	} else if !snapshot.SwapReported {
		// Older agents do not report swap: probe `free` as before.
		if pct, ok := probeSwapPercent(app, name); ok {
			row.Swap = pct
		}
	}
	return row
}

// snapshotSwapPercent returns swap used/total as a percentage when the agent
// reported swap and the host has some configured (no swap renders as "-", just
// as the `free` probe does).
func snapshotSwapPercent(s proto.MetricsSnapshot) (float64, bool) {
	if !s.SwapReported || s.SwapTotalBytes == 0 {
		return 0, false
	}
	return float64(s.SwapUsedBytes) / float64(s.SwapTotalBytes) * 100, true
}

// probeLoadAvg reads /proc/loadavg via ExecCommand and parses the three averages.
func probeLoadAvg(app *core.App, name string) (l1, l5, l15 float64, ok bool) {
	res, err := app.ExecCommand(name, "cat /proc/loadavg")
	if err != nil || res.ExitCode != 0 {
		return 0, 0, 0, false
	}
	fields := strings.Fields(res.Stdout)
	if len(fields) < 3 {
		return 0, 0, 0, false
	}
	a, e1 := parseFloat(fields[0])
	b, e2 := parseFloat(fields[1])
	c, e3 := parseFloat(fields[2])
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, 0, 0, false
	}
	return a, b, c, true
}

// probeSwapPercent reads `free` and computes used/total swap as a percentage.
func probeSwapPercent(app *core.App, name string) (float64, bool) {
	res, err := app.ExecCommand(name, "free -b")
	if err != nil || res.ExitCode != 0 {
		return 0, false
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(fields[0]), "swap") {
			continue
		}
		total, e1 := parseFloat(fields[1])
		used, e2 := parseFloat(fields[2])
		if e1 != nil || e2 != nil || total <= 0 {
			return 0, false
		}
		return used / total * 100, true
	}
	return 0, false
}

func renderTopTable(cmd *cobra.Command, rows []topRow, group string, live bool) error {
	out := cmd.OutOrStdout()
	if live {
		// Clear screen and home the cursor for an in-place refresh.
		fmt.Fprint(out, "\x1b[H\x1b[2J")
	}
	header := fmt.Sprintf("fleet top — %s", time.Now().Format("15:04:05"))
	if group != "" {
		header += fmt.Sprintf("  group=%q", group)
	}
	fmt.Fprintln(out, header)
	if live {
		fmt.Fprintln(out, "(Ctrl-C to exit)")
	}
	fmt.Fprintln(out)

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVER\tCPU%\tMEM%\tSWAP%\tDISK%\tLOAD1\tLOAD5\tLOAD15\tSTATUS")
	for _, r := range rows {
		if !r.Reachable {
			status := r.Err
			if status == "" {
				status = "unreachable"
			}
			fmt.Fprintf(w, "%s\t-\t-\t-\t-\t-\t-\t-\t%s\n", r.Server, status)
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Server,
			formatPercent(r.CPU),
			formatPercent(r.Mem),
			formatSwap(r.Swap),
			formatPercent(r.Disk),
			formatLoad(r.Load1),
			formatLoad(r.Load5),
			formatLoad(r.Load15),
			"online",
		)
	}
	return w.Flush()
}

func formatPercent(v float64) string {
	return fmt.Sprintf("%.1f", v)
}

func formatSwap(v float64) string {
	if v < 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", v)
}

func formatLoad(v float64) string {
	return fmt.Sprintf("%.2f", v)
}

// parseFloat is a thin wrapper so the probe helpers read cleanly.
func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%g", &f)
	return f, err
}
