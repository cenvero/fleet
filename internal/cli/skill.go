// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// newSkillCommand installs agent integrations that teach an AI coding agent how
// to drive Cenvero Fleet. Each integration is thin: it tells the agent to run
// `fleet context` to load the full, always-current reference, then operate.
func newSkillCommand() *cobra.Command {
	skillCmd := &cobra.Command{
		Use:   "skill",
		Short: "Install agent integrations (Claude, Codex, AGENTS.md) for driving fleet",
		Long: "Install a global skill / slash-command that teaches an AI coding agent to operate\n" +
			"Cenvero Fleet. The installed skill instructs the agent to run `fleet context` first,\n" +
			"which prints the complete, up-to-date command reference and concepts.",
	}

	skillCmd.AddCommand(newSkillTargetCommand(
		"claude", []string{"cloud"},
		"Install a global Claude Code skill + /fleet slash command",
		installClaudeSkill,
	))
	skillCmd.AddCommand(newSkillTargetCommand(
		"codex", nil,
		"Install a global Codex /fleet prompt",
		installCodexSkill,
	))
	skillCmd.AddCommand(newSkillTargetCommand(
		"agents", nil,
		"Write a portable AGENTS.md (defaults to the current directory)",
		installAgentsFile,
	))

	skillCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List available skill targets and where they install",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "Available skill targets:")
			fmt.Fprintln(out, "  claude  -> ~/.claude/skills/cenvero-fleet/SKILL.md + ~/.claude/commands/fleet.md")
			fmt.Fprintln(out, "  codex   -> ~/.codex/prompts/fleet.md")
			fmt.Fprintln(out, "  agents  -> ./AGENTS.md  (override with --dir)")
			fmt.Fprintln(out, "\nEach skill instructs the agent to run `fleet context` first.")
			return nil
		},
	})

	return skillCmd
}

func newSkillTargetCommand(name string, aliases []string, short string, install func(opts skillInstallOptions) (skillResult, error)) *cobra.Command {
	var dir string
	var printOnly bool
	cmd := &cobra.Command{
		Use:     name,
		Aliases: aliases,
		Short:   short,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			res, err := install(skillInstallOptions{dir: dir, printOnly: printOnly, out: cmd.OutOrStdout()})
			if err != nil {
				return err
			}
			if printOnly {
				return nil
			}
			out := cmd.OutOrStdout()
			if res.message != "" {
				fmt.Fprintln(out, res.message)
				return nil
			}
			for _, p := range res.paths {
				fmt.Fprintf(out, "installed %s\n", p)
			}
			fmt.Fprintln(out, "\nThe agent can now run `fleet context` to load the full reference.")
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "override the install base directory")
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the content instead of writing files")
	return cmd
}

type skillInstallOptions struct {
	dir       string
	printOnly bool
	out       interface{ Write([]byte) (int, error) }
}

// skillResult is what an installer returns: the files written and an optional
// tailored completion message (used instead of the default per-path output).
type skillResult struct {
	paths   []string
	message string
}

func installClaudeSkill(opts skillInstallOptions) (skillResult, error) {
	base := opts.dir
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return skillResult{}, err
		}
		base = filepath.Join(home, ".claude")
	}
	skillPath := filepath.Join(base, "skills", "cenvero-fleet", "SKILL.md")
	cmdPath := filepath.Join(base, "commands", "fleet.md")

	if opts.printOnly {
		fmt.Fprintf(opts.out, "# %s\n\n%s\n# %s\n\n%s", skillPath, claudeSkillMarkdown(), cmdPath, claudeSlashCommandMarkdown())
		return skillResult{}, nil
	}
	// Re-running just overwrites the existing files (no --force, no error).
	if err := writeSkillFile(cmdPath, claudeSlashCommandMarkdown()); err != nil {
		return skillResult{}, err
	}
	if err := writeSkillFile(skillPath, claudeSkillMarkdown()); err != nil {
		return skillResult{}, err
	}
	msg := "✓ /fleet slash command added:\n    " + cmdPath +
		"\n  Cenvero Fleet skill added:\n    " + skillPath +
		"\n\nRestart Claude — or launch `claude` — then type /fleet to load the full" +
		"\nCenvero Fleet CLI and manage your servers for the rest of the session."
	return skillResult{paths: []string{cmdPath, skillPath}, message: msg}, nil
}

func installCodexSkill(opts skillInstallOptions) (skillResult, error) {
	base := opts.dir
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return skillResult{}, err
		}
		base = filepath.Join(home, ".codex")
	}
	promptPath := filepath.Join(base, "prompts", "fleet.md")
	if opts.printOnly {
		fmt.Fprintf(opts.out, "# %s\n\n%s", promptPath, codexPromptMarkdown())
		return skillResult{}, nil
	}
	if err := writeSkillFile(promptPath, codexPromptMarkdown()); err != nil {
		return skillResult{}, err
	}
	msg := "✓ /fleet prompt added:\n    " + promptPath +
		"\n\nRestart Codex, then run /fleet to load the Cenvero Fleet CLI for the session."
	return skillResult{paths: []string{promptPath}, message: msg}, nil
}

func installAgentsFile(opts skillInstallOptions) (skillResult, error) {
	dir := opts.dir
	if dir == "" {
		dir = "."
	}
	target := filepath.Join(dir, "AGENTS.md")
	content := agentsMarkdown()
	if opts.printOnly {
		fmt.Fprintf(opts.out, "# %s\n\n%s", target, content)
		return skillResult{}, nil
	}
	// AGENTS.md is commonly shared — append our section idempotently rather than
	// clobbering an existing file.
	if existing, err := os.ReadFile(target); err == nil {
		if strings.Contains(string(existing), agentsMarker) {
			return skillResult{message: fmt.Sprintf("%s already contains the Cenvero Fleet section; nothing to do", target)}, nil
		}
		merged := strings.TrimRight(string(existing), "\n") + "\n\n" + content
		if err := os.WriteFile(target, []byte(merged), 0o600); err != nil {
			return skillResult{}, err
		}
		return skillResult{paths: []string{target}}, nil
	}
	if err := writeSkillFile(target, content); err != nil {
		return skillResult{}, err
	}
	return skillResult{paths: []string{target}}, nil
}

// writeSkillFile writes content to path, creating parents and overwriting any
// existing file (skill installs are idempotent — re-running just refreshes).
func writeSkillFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

// ---- content ----

const agentsMarker = "<!-- cenvero-fleet -->"

// skillCoreBody is the shared guidance every integration embeds.
func skillCoreBody() string {
	return "Cenvero Fleet (`fleet`) is a self-hosted fleet manager for Linux, macOS, and Windows\n" +
		"servers, operated over an authenticated SSH transport.\n\n" +
		"**Before doing anything, run:**\n\n" +
		"```bash\nfleet context\n```\n\n" +
		"That prints the complete, always-current command reference, concepts, and workflows\n" +
		"(generated from the installed binary). Use `fleet context --json` for a structured\n" +
		"command tree, and `fleet ai <command>` (e.g. `fleet ai file upload`, add `--json`)\n" +
		"to get the full help for any single command. Then operate with\n" +
		"`fleet <group> <subcommand> ...`.\n\n" +
		"Guidance:\n" +
		"- Most commands print JSON to stdout — parse it.\n" +
		"- Check state first: `fleet status` and `fleet server list`.\n" +
		"- Add `--config-dir <path>` for a non-default controller location.\n" +
		"- Confirm before destructive actions: `server remove`, `file rm`, `key rotate`,\n" +
		"  `update apply`, `self-uninstall`, `config restore`.\n" +
		"- Move files with `fleet file upload|download|list`; interactive UIs are\n" +
		"  `fleet files <server>` (terminal) and `fleet file ui` (browser).\n"
}

func claudeSkillMarkdown() string {
	return "---\n" +
		"name: cenvero-fleet\n" +
		"description: Operate Cenvero Fleet — a self-hosted SSH-based fleet manager (servers, services, logs, metrics, firewall, files and transfers) via the `fleet` CLI. Use whenever the user mentions Cenvero Fleet, the `fleet` command, or managing their fleet of servers.\n" +
		"---\n\n" +
		"# Cenvero Fleet\n\n" +
		skillCoreBody()
}

func claudeSlashCommandMarkdown() string {
	return "---\n" +
		"description: Load the Cenvero Fleet CLI and operate the user's fleet for this session\n" +
		"---\n\n" +
		"Run `fleet context` now (with the Bash tool) and read its full output. It is the\n" +
		"complete, self-describing reference for the Cenvero Fleet CLI — every command,\n" +
		"concept, and safety rule — generated live from the installed binary. Keep it in\n" +
		"mind for the rest of this session; you do not need to reload it unless the context\n" +
		"is compacted.\n\n" +
		"You are now the user's fleet operator:\n\n" +
		"- Inspect and control their servers with `fleet <group> <subcommand> ...`; most\n" +
		"  commands print JSON to stdout — parse it.\n" +
		"- For the full help of any one command, run `fleet ai <command>` (add `--json`).\n" +
		"- Check state first (`fleet status`, `fleet server list`). Confirm before\n" +
		"  destructive or outward-facing actions (`server remove`, `file rm`, `key rotate`,\n" +
		"  `update apply`, `self-uninstall`).\n\n" +
		"Then help with this request — if it is empty, summarize the fleet's current state\n" +
		"and suggest next steps:\n\n" +
		"$ARGUMENTS\n"
}

func codexPromptMarkdown() string {
	return "Run `fleet context` to load the full Cenvero Fleet command reference and concepts,\n" +
		"then help with the request below.\n\n" +
		"$ARGUMENTS\n\n" +
		"---\n\n" +
		skillCoreBody()
}

func agentsMarkdown() string {
	return agentsMarker + "\n# Cenvero Fleet\n\n" + skillCoreBody()
}
