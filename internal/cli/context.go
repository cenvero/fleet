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

// newAICommand prints the full, machine-readable help for any command — the
// AI-facing counterpart to --help. It shares the renderer with `context`, so it
// always reflects the live command tree (description, flags, subcommands).
func newAICommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ai [command...]",
		Short: "Print full machine-readable help for a command (for AI agents)",
		Long: "Print the complete help — usage, full description, flags, and subcommands — for\n" +
			"any command, in markdown (default) or JSON (--json). It is the AI-facing\n" +
			"counterpart to --help: humans keep using --help for the normal concise help;\n" +
			"an agent runs `fleet ai <command>` to get everything about a command in one\n" +
			"structured block. With no command it prints the full reference (like `context`\n" +
			"without the concept sections).\n\n" +
			"Examples:\n" +
			"  fleet ai                 # full reference for every command\n" +
			"  fleet ai file upload     # full help for one command\n" +
			"  fleet ai sync --json     # the same, as JSON",
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			target := root
			if len(args) > 0 {
				found, _, err := root.Find(args)
				if err != nil || found == nil || found == root {
					return fmt.Errorf("unknown command %q — run 'fleet ai' to list everything", strings.Join(args, " "))
				}
				target = found
			}
			if asJSON {
				if target == root {
					return writeJSON(cmd, buildContextJSON(root))
				}
				return writeJSON(cmd, commandNodeJSON(target))
			}
			var b strings.Builder
			if target == root {
				for _, c := range visibleSubcommands(root) {
					renderCommandMarkdown(&b, c, 2)
				}
			} else {
				renderCommandMarkdown(&b, target, 1)
			}
			fmt.Fprint(cmd.OutOrStdout(), b.String())
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON instead of markdown")
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
	b.WriteString("This reference is generated live from the installed binary by walking the command tree, so it always matches your version — there is nothing to keep in sync by hand, and any new command appears automatically. Each entry is the command's own help. For one command on demand, run `fleet ai <command>` (e.g. `fleet ai file upload`) or `fleet ai <command> --json`. Global flag (all commands): `--config-dir <path>` selects a non-default controller directory.\n\n")
	for _, c := range visibleSubcommands(root) {
		renderCommandMarkdown(&b, c, 3)
	}

	b.WriteString(contextWorkflows)
	return b.String()
}

// renderCommandMarkdown writes a command — its path, usage line, full help
// (Long, else Short), and flags — then recurses into its subcommands at the next
// heading level. This is the shared renderer used by both `context` and `ai`.
func renderCommandMarkdown(b *strings.Builder, c *cobra.Command, depth int) {
	level := min(depth, 6)
	fmt.Fprintf(b, "%s `%s`\n\n", strings.Repeat("#", level), c.CommandPath())
	if use := strings.TrimSpace(c.UseLine()); use != "" {
		fmt.Fprintf(b, "Usage: `%s`\n\n", use)
	}
	if help := fullHelp(c); help != "" {
		fmt.Fprintf(b, "%s\n\n", help)
	}
	if flags := commandFlagLines(c); len(flags) > 0 {
		b.WriteString("Flags:\n")
		for _, f := range flags {
			fmt.Fprintf(b, "- %s\n", f)
		}
		b.WriteString("\n")
	}
	for _, sub := range visibleSubcommands(c) {
		renderCommandMarkdown(b, sub, depth+1)
	}
}

// fullHelp returns a command's complete Long help, falling back to its Short.
func fullHelp(c *cobra.Command) string {
	if strings.TrimSpace(c.Long) != "" {
		return strings.TrimSpace(c.Long)
	}
	return strings.TrimSpace(c.Short)
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
	Usage       string               `json:"usage,omitempty"`
	Short       string               `json:"short,omitempty"`
	Long        string               `json:"long,omitempty"`
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
		out = append(out, commandNodeJSON(sub))
	}
	return out
}

// commandNodeJSON builds the full JSON node for a single command (its usage,
// short/long help, flags, and recursive subcommands). Shared by `context` and
// `ai`.
func commandNodeJSON(c *cobra.Command) contextCommandJSON {
	node := contextCommandJSON{
		Path:        c.CommandPath(),
		Usage:       strings.TrimSpace(c.UseLine()),
		Short:       commandDescription(c),
		Long:        strings.TrimSpace(c.Long),
		Subcommands: commandsJSON(c),
	}
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		node.Flags = append(node.Flags, contextFlagJSON{
			Name: f.Name, Type: f.Value.Type(), Usage: f.Usage, Default: f.DefValue,
		})
	})
	return node
}

// ---- static conceptual sections ----

const contextAbout = "## What this is\n\n" +
	"Cenvero Fleet is a self-hosted, operator-owned fleet manager for Linux, macOS, and Windows " +
	"servers. A single controller (`fleet`) manages remote agents (`fleet-agent`) over an " +
	"authenticated, host-key-pinned SSH transport. State lives in an operator-controlled directory; " +
	"there is no cloud dependency in the core runtime.\n\n" +
	"It is built to be driven programmatically and operated unattended: most commands emit JSON, " +
	"`exec --json` returns a structured result (stdout/stderr/exit_code/duration), and a set of safety " +
	"primitives (scoped RBAC tokens, named secrets, a dead-man's-switch, command policy, and an approval " +
	"queue) lets an agent make changes without holding a live human's hand on every keystroke.\n\n"

const contextForAgents = "## How to use this as an agent\n\n" +
	"- Run commands with `fleet <group> <subcommand> ...`. Most commands print JSON to stdout — parse it.\n" +
	"- Need the full details of one command? Run `fleet ai <command>` (e.g. `fleet ai file upload`), or add `--json`. It's the machine-readable counterpart to `--help` and always matches the installed binary.\n" +
	"- Add `--config-dir <path>` if the user runs a controller outside the default `~/.cenvero-fleet`.\n" +
	"- Start by checking state: `fleet status` (overall), `fleet server list` (the fleet), `fleet health` and " +
	"`fleet inventory --json` (plan against a machine-readable view of every server's OS, resources, ports, services, and tags).\n" +
	"- Prefer structured execution for anything you'll parse or gate on: `fleet exec <server> <cmd> --json` returns " +
	"`{stdout, stderr, exit_code, duration}`. Add `--timeout`, `--retry/--backoff` for flaky transports, " +
	"`--dry-run` to preview, `--propagate-exit` to surface the remote exit code, and `--group role=web` to fan out by tag.\n" +
	"- If you see \"not initialized\", the controller needs `fleet init` first — confirm with the user before initializing.\n" +
	"- DESTRUCTIVE or outward-facing actions require explicit user intent — confirm before running: " +
	"`server remove`, `file rm`, `key rotate`, `update apply`, `self-uninstall`, `config restore`.\n" +
	"- Read-only/safe to explore freely: `status`, `health`, `inventory`, `top`, `doctor`, `drift`, " +
	"`server list/show/metrics`, `service list`, `svc status`, `journal`, `logs`, `file list`, `config show`, `context`.\n\n" +
	"### Operating unattended (safety primitives)\n\n" +
	"- **Scope yourself with an RBAC token.** Pass `--token <id>` (or set `FLEET_TOKEN`) to run inside a scope the operator " +
	"minted with `fleet token create` (`--servers`, `--group`, `--allow`/`--deny` commands, `--destructive`). The controller " +
	"fails closed: out-of-scope or destructive calls are denied, and a scoped token can never mint or modify tokens.\n" +
	"- **Never inline credentials.** Store them with `fleet secret set <name>`, then inject per-command with " +
	"`fleet exec ... --secret VAR=@name` (resolves the stored secret into an env var). Secret values are redacted from " +
	"stdout/stderr and the audit log; add reusable redaction patterns with `fleet policy`.\n" +
	"- **Guard risky changes with a dead-man's-switch.** `fleet guard <server> <cmd> --revert-after 2m --revert-cmd '<undo>'` " +
	"arms a detached server-side timer that auto-reverts unless you `fleet confirm <id>` in time; `fleet revert <id>` undoes it now. " +
	"`fleet exec --guard` refuses commands that could lock the controller out of a server.\n" +
	"- **Stage instead of running** when a human must sign off: `fleet exec ... --require-approval` queues the command " +
	"(`fleet approvals list`, then `fleet approve <id>` / `approvals reject <id>`). `fleet cmd-policy` defines deny/confirm " +
	"patterns; a confirm-flagged command needs `--confirm`. Use `--idempotency-key` so a retried `exec` returns the cached " +
	"result instead of running twice.\n" +
	"- **Multi-step changes** belong in a playbook: `fleet run <playbook.yaml>` runs idempotent check/apply steps and can " +
	"`--on-fail rollback`; preview with `--dry-run` and target with `--group`.\n\n"

const contextConcepts = "## Concepts\n\n" +
	"- Transport modes: `direct` (controller dials the agent's port) and `reverse` (agent dials out to the " +
	"controller). Host keys are pinned on first contact (TOFU) and rejected if they change.\n" +
	"- Security: all RPCs ride one authenticated `fleet-rpc` SSH channel — public-key auth only, strong " +
	"ciphers, no separate unauthenticated port.\n" +
	"- Files: secure file transfers are chunked, parallel (direct mode), checksummed, and resumable. " +
	"Surfaces are the `fleet file` CLI (incl. `fleet file copy`/`move` directly between two servers), the `fleet files` dual-pane TUI (alias `fleet filemanager` / `fm`, supports local↔server and server↔server), and the localhost web app `fleet file ui` (alias `fleet filemanager ui`). Both UIs have full operations (new folder, rename, delete, copy, move), a hidden-files toggle, and List/Icons views.\n" +
	"- Storage: config + per-server records live as TOML under the config dir; workload/metrics state in a " +
	"SQLite/Postgres/MySQL/MariaDB backend. Tokens, secrets, tags, guards, jobs, and policy each live in a small " +
	"local JSON store under the config dir. Everything is operator-controlled.\n" +
	"- Tags & groups: `fleet tag <server> key=value` labels a server; commands that take `--group EXPR` " +
	"(e.g. `role=web,env=prod`, comma = AND) target every server matching the tag expression.\n" +
	"- RBAC: scoped tokens (`fleet token`) are enforced controller-side. A token can be limited to named servers " +
	"or a tag group, to an allow/deny list of top-level commands, and to whether destructive operations are permitted.\n" +
	"- Safety state: secrets (`fleet secret`), command policy (`fleet cmd-policy`), output redaction (`fleet policy`), " +
	"the approval queue (`fleet approvals`/`approve`), and dead-man's-switch guards (`fleet guard`/`confirm`/`revert`) " +
	"are all local to the controller and apply before anything touches a server.\n" +
	"- Observability: `fleet health`/`top` summarize the fleet; `doctor` and `drift` check one server; `inventory` " +
	"is the machine-readable fleet snapshot; `notify` fires Slack/webhook alerts on events (offline, job-failed, drift).\n\n"

const contextWorkflows = "## Common workflows\n\n" +
	"Add a Linux server and auto-install the agent:\n" +
	"```\nfleet server add web-01 192.0.2.10 --mode direct --login-user ubuntu --sudo\n```\n\n" +
	"Inspect a server:\n" +
	"```\nfleet server show web-01\nfleet server metrics web-01\nfleet service list web-01\n```\n\n" +
	"Move files (chunked, parallel, resumable):\n" +
	"```\nfleet file upload web-01 ./app.tar.gz /srv/app.tar.gz --parallel 4\nfleet file download web-01 /var/log/syslog ./syslog\n```\n\n" +
	"Open the interactive UIs:\n" +
	"```\nfleet dashboard            # fleet-wide TUI\nfleet files web-01 db-01   # dual-pane file manager (a.k.a. fleet filemanager)\nfleet file ui              # localhost web file manager (a.k.a. fleet filemanager ui)\nfleet file copy web-01:/a db-01:/a   # server-to-server copy (move: fleet file move)\n```\n\n" +
	"Run a command for a machine-readable result, with a timeout and retries:\n" +
	"```\nfleet exec web-01 \"systemctl is-active nginx\" --json --timeout 30s --retry 2\nfleet exec --group role=web \"uname -r\" --json   # fan out by tag\n```\n\n" +
	"Operate unattended with a scoped token + a stored secret:\n" +
	"```\nfleet token create --name deploy --group role=web --allow exec,service --destructive\nfleet secret set deploy_key --generate 40\nFLEET_TOKEN=<id> fleet exec web-01 \"./deploy.sh\" --secret DEPLOY_KEY=@deploy_key --json\n```\n\n" +
	"Make a risky change safely (auto-reverts unless confirmed):\n" +
	"```\nfleet guard web-01 \"ufw default deny incoming && ufw reload\" --revert-after 2m --revert-cmd \"ufw default allow incoming && ufw reload\"\nfleet confirm <id>     # keep the change; or: fleet revert <id> to undo now\n```\n\n" +
	"Apply a multi-step change as a transactional playbook:\n" +
	"```\nfleet run deploy.yaml --group role=web --on-fail rollback --dry-run   # preview\nfleet run deploy.yaml --group role=web --on-fail rollback              # apply\n```\n\n" +
	"Inspect health and plan against the inventory:\n" +
	"```\nfleet health --json\nfleet inventory --json\nfleet doctor web-01 --json\n```\n\n" +
	"Re-print this reference at any time with `fleet context` (add `--json` for a structured command tree).\n"
