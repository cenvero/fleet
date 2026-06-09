// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/cenvero/fleet/internal/version"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// newContextCommand prints a complete, self-describing reference for the fleet
// CLI, aimed at AI coding agents. It is generated from the live command tree so
// it never drifts from the binary, and needs no initialized controller.
func newContextCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Print the full agent-facing reference (commands, concepts, workflows)",
		Long: "Print a complete, machine-readable reference for operating Cenvero Fleet.\n" +
			"Designed for AI agents: run this first to learn every command and concept, then act.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asJSON {
				return writeJSON(cmd, buildContextJSON(cmd.Root()))
			}
			fmt.Fprint(cmd.OutOrStdout(), renderContextMarkdown(cmd.Root()))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the command tree as JSON instead of markdown")
	return cmd
}

func renderContextMarkdown(root *cobra.Command) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — Agent Context\n\n", version.ProductName)
	fmt.Fprintf(&b, "Binary: `%s`  ·  Version: `%s`\n\n", version.BinaryName, version.Version)
	b.WriteString(contextAbout)
	b.WriteString(contextForAgents)
	b.WriteString(contextConcepts)

	b.WriteString("## Command reference\n\n")
	b.WriteString("Global flag (all commands): `--config-dir <path>` selects a non-default controller directory.\n\n")
	for _, c := range visibleSubcommands(root) {
		renderCommandMarkdown(&b, c)
	}

	b.WriteString(contextWorkflows)
	return b.String()
}

func renderCommandMarkdown(b *strings.Builder, c *cobra.Command) {
	fmt.Fprintf(b, "### `%s`\n\n", c.CommandPath())
	if desc := commandDescription(c); desc != "" {
		fmt.Fprintf(b, "%s\n\n", desc)
	}
	if flags := commandFlagLines(c); len(flags) > 0 {
		b.WriteString("Flags:\n")
		for _, f := range flags {
			fmt.Fprintf(b, "- %s\n", f)
		}
		b.WriteString("\n")
	}
	subs := visibleSubcommands(c)
	if len(subs) > 0 {
		b.WriteString("Subcommands:\n")
		for _, sub := range subs {
			line := fmt.Sprintf("- `%s` — %s", sub.CommandPath(), commandDescription(sub))
			if flags := commandFlagLines(sub); len(flags) > 0 {
				line += "  (flags: " + strings.Join(flagNames(sub), ", ") + ")"
			}
			b.WriteString(strings.TrimRight(line, " ") + "\n")
			// Recurse one extra level for grouped subcommands (e.g. config, file defaults).
			for _, leaf := range visibleSubcommands(sub) {
				fmt.Fprintf(b, "  - `%s` — %s\n", leaf.CommandPath(), commandDescription(leaf))
			}
		}
		b.WriteString("\n")
	}
}

func commandDescription(c *cobra.Command) string {
	if c.Short != "" {
		return c.Short
	}
	if c.Long != "" {
		return strings.SplitN(c.Long, "\n", 2)[0]
	}
	return ""
}

func commandFlagLines(c *cobra.Command) []string {
	var lines []string
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		entry := fmt.Sprintf("`--%s` (%s)", f.Name, f.Value.Type())
		if f.Usage != "" {
			entry += ": " + f.Usage
		}
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" && f.DefValue != "[]" {
			entry += fmt.Sprintf(" [default %s]", f.DefValue)
		}
		lines = append(lines, entry)
	})
	return lines
}

func flagNames(c *cobra.Command) []string {
	var names []string
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		names = append(names, "--"+f.Name)
	})
	return names
}

func visibleSubcommands(c *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, sub := range c.Commands() {
		if sub.Hidden || !sub.IsAvailableCommand() {
			continue
		}
		switch sub.Name() {
		case "help", "completion":
			continue
		}
		out = append(out, sub)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// ---- JSON form ----

type contextCommandJSON struct {
	Path        string               `json:"path"`
	Use         string               `json:"use"`
	Short       string               `json:"short,omitempty"`
	Flags       []contextFlagJSON    `json:"flags,omitempty"`
	Subcommands []contextCommandJSON `json:"subcommands,omitempty"`
}

type contextFlagJSON struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Usage   string `json:"usage,omitempty"`
	Default string `json:"default,omitempty"`
}

func buildContextJSON(root *cobra.Command) map[string]any {
	return map[string]any{
		"product":  version.ProductName,
		"binary":   version.BinaryName,
		"version":  version.Version,
		"commands": commandsJSON(root),
	}
}

func commandsJSON(c *cobra.Command) []contextCommandJSON {
	subs := visibleSubcommands(c)
	out := make([]contextCommandJSON, 0, len(subs))
	for _, sub := range subs {
		node := contextCommandJSON{
			Path:        sub.CommandPath(),
			Use:         sub.Use,
			Short:       commandDescription(sub),
			Subcommands: commandsJSON(sub),
		}
		sub.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Hidden || f.Name == "help" {
				return
			}
			node.Flags = append(node.Flags, contextFlagJSON{
				Name: f.Name, Type: f.Value.Type(), Usage: f.Usage, Default: f.DefValue,
			})
		})
		out = append(out, node)
	}
	return out
}

// ---- static conceptual sections ----

const contextAbout = "## What this is\n\n" +
	"Cenvero Fleet is a self-hosted, operator-owned fleet manager for Linux, macOS, and Windows " +
	"servers. A single controller (`fleet`) manages remote agents (`fleet-agent`) over an " +
	"authenticated, host-key-pinned SSH transport. State lives in an operator-controlled directory; " +
	"there is no cloud dependency in the core runtime.\n\n"

const contextForAgents = "## How to use this as an agent\n\n" +
	"- Run commands with `fleet <group> <subcommand> ...`. Most commands print JSON to stdout — parse it.\n" +
	"- Add `--config-dir <path>` if the user runs a controller outside the default `~/.cenvero-fleet`.\n" +
	"- Start by checking state: `fleet status` (overall) and `fleet server list` (the fleet).\n" +
	"- If you see \"not initialized\", the controller needs `fleet init` first — confirm with the user before initializing.\n" +
	"- DESTRUCTIVE or outward-facing actions require explicit user intent — confirm before running: " +
	"`server remove`, `file rm`, `key rotate`, `update apply`, `self-uninstall`, `config restore`.\n" +
	"- Read-only/safe to explore freely: `status`, `server list/show/metrics`, `service list`, `logs`, " +
	"`file list`, `config show`, `context`.\n\n"

const contextConcepts = "## Concepts\n\n" +
	"- Transport modes: `direct` (controller dials the agent's port) and `reverse` (agent dials out to the " +
	"controller). Host keys are pinned on first contact (TOFU) and rejected if they change.\n" +
	"- Security: all RPCs ride one authenticated `fleet-rpc` SSH channel — public-key auth only, strong " +
	"ciphers, no separate unauthenticated port.\n" +
	"- Files: secure file transfers are chunked, parallel (direct mode), checksummed, and resumable. " +
	"Surfaces are the `fleet file` CLI, the `fleet files` dual-pane TUI, and the `fleet ui` localhost web app.\n" +
	"- Storage: config + per-server records live as TOML under the config dir; workload/metrics state in a " +
	"SQLite/Postgres/MySQL/MariaDB backend. Everything is operator-controlled.\n\n"

const contextWorkflows = "## Common workflows\n\n" +
	"Add a Linux server and auto-install the agent:\n" +
	"```\nfleet server add web-01 192.0.2.10 --mode direct --login-user ubuntu --sudo\n```\n\n" +
	"Inspect a server:\n" +
	"```\nfleet server show web-01\nfleet server metrics web-01\nfleet service list web-01\n```\n\n" +
	"Move files (chunked, parallel, resumable):\n" +
	"```\nfleet file upload web-01 ./app.tar.gz /srv/app.tar.gz --parallel 4\nfleet file download web-01 /var/log/syslog ./syslog\n```\n\n" +
	"Open the interactive UIs:\n" +
	"```\nfleet dashboard        # fleet-wide TUI\nfleet files web-01     # dual-pane file manager\nfleet ui               # localhost web file manager\n```\n\n" +
	"Re-print this reference at any time with `fleet context` (add `--json` for a structured command tree).\n"
