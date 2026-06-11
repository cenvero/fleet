// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/cenvero/fleet/internal/version"
	"github.com/spf13/cobra"
)

// newAgentVersionCommand builds `fleet agent version [--all|<server>]`.
//
// It reports the observed agent version per server, NORMALIZED (a leading 'v'
// stripped and surrounding spaces trimmed) so "v2.1.0" and "2.1.0" compare
// equal. Versions are flagged when they differ from the reference version: the
// controller's own version, or — when the controller is a dev build — the most
// common normalized agent version across the fleet.
//
// NOTE: no top-level `fleet agent` command exists yet. This returns a standalone
// `version` command; the main loop should attach it under a `fleet agent`
// parent (e.g. agent.AddCommand(newAgentVersionCommand(&configDir))). If wired
// directly onto root it would register as `fleet version`, which already exists,
// so it must be attached as a subcommand of `agent`.
func newAgentVersionCommand(configDir *string) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "version [--all|<server>]",
		Short: "Report agent versions per server and flag mismatches",
		Long: "Report the observed agent version for each server, normalized so a leading 'v'\n" +
			"and surrounding whitespace are ignored (so 'v2.1.0' and '2.1.0' are equal).\n\n" +
			"Versions are compared against a reference (the controller version, or the most\n" +
			"common agent version when the controller is a dev build) and mismatches are\n" +
			"flagged.\n\n" +
			"  fleet agent version            # all servers (default)\n" +
			"  fleet agent version --all      # all servers (explicit)\n" +
			"  fleet agent version web-01     # a single server",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			only := ""
			if len(args) == 1 {
				if all {
					return fmt.Errorf("pass either --all or a server name, not both")
				}
				only = args[0]
			}
			return runAgentVersion(cmd, *configDir, only)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "report every server (the default)")
	return cmd
}

// agentVersionRow is one server's reported agent version.
type agentVersionRow struct {
	Server     string
	Raw        string
	Normalized string
	Reachable  bool
}

// runAgentVersion gathers, compares, and prints agent versions.
func runAgentVersion(cmd *cobra.Command, configDir, only string) error {
	app, err := openApp(configDir)
	if err != nil {
		return err
	}
	defer app.Close()

	servers, err := app.ListServers()
	if err != nil {
		return err
	}

	var rows []agentVersionRow
	for _, s := range servers {
		if only != "" && s.Name != only {
			continue
		}
		rows = append(rows, agentVersionRow{
			Server:     s.Name,
			Raw:        s.Observed.AgentVersion,
			Normalized: normalizeAgentVersion(s.Observed.AgentVersion),
			Reachable:  s.Observed.Reachable,
		})
	}
	if only != "" && len(rows) == 0 {
		return fmt.Errorf("server %q not found — run 'fleet server list' to see all servers", only)
	}

	reference := referenceVersion(rows)

	out := cmd.OutOrStdout()
	if reference != "" {
		fmt.Fprintf(out, "reference version: %s\n\n", reference)
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "SERVER\tAGENT VERSION\tSTATUS"); err != nil {
		return err
	}
	mismatches := 0
	for _, r := range rows {
		display := r.Normalized
		if display == "" {
			display = "-"
		}
		status := "ok"
		switch {
		case r.Normalized == "":
			status = "unknown"
		case reference != "" && r.Normalized != reference:
			status = "MISMATCH"
			mismatches++
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\n", r.Server, display, status); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if mismatches > 0 {
		fmt.Fprintf(out, "\n%d server(s) do not match the reference version %s\n", mismatches, reference)
	}
	return nil
}

// normalizeAgentVersion trims surrounding whitespace and a single leading 'v'
// (or 'V') so "v2.1.0", " 2.1.0 ", and "2.1.0" all normalize to "2.1.0".
func normalizeAgentVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if v[0] == 'v' || v[0] == 'V' {
		v = strings.TrimSpace(v[1:])
	}
	return v
}

// referenceVersion picks the version to compare against: the controller's own
// version when it is a real release, otherwise the most common (mode) normalized
// agent version observed across the fleet. Returns "" if nothing is known.
func referenceVersion(rows []agentVersionRow) string {
	if ctrl := normalizeAgentVersion(version.Version); ctrl != "" && ctrl != "dev" {
		return ctrl
	}
	counts := map[string]int{}
	for _, r := range rows {
		if r.Normalized != "" {
			counts[r.Normalized]++
		}
	}
	best := ""
	bestCount := 0
	// Iterate in sorted key order so ties resolve deterministically.
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if counts[k] > bestCount {
			bestCount = counts[k]
			best = k
		}
	}
	return best
}
