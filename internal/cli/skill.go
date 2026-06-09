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

func newSkillTargetCommand(name string, aliases []string, short string, install func(opts skillInstallOptions) ([]string, error)) *cobra.Command {
	var dir string
	var force bool
	var printOnly bool
	cmd := &cobra.Command{
		Use:     name,
		Aliases: aliases,
		Short:   short,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			written, err := install(skillInstallOptions{dir: dir, force: force, printOnly: printOnly, out: cmd.OutOrStdout()})
			if err != nil {
				return err
			}
			if printOnly {
				return nil
			}
			for _, p := range written {
				fmt.Fprintf(cmd.OutOrStdout(), "installed %s\n", p)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "\nThe agent can now run `fleet context` to load the full reference.")
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "override the install base directory")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing files")
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the content instead of writing files")
	return cmd
}

type skillInstallOptions struct {
	dir       string
	force     bool
	printOnly bool
	out       interface{ Write([]byte) (int, error) }
}

func installClaudeSkill(opts skillInstallOptions) ([]string, error) {
	base := opts.dir
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		base = filepath.Join(home, ".claude")
	}
	skillPath := filepath.Join(base, "skills", "cenvero-fleet", "SKILL.md")
	cmdPath := filepath.Join(base, "commands", "fleet.md")

	if opts.printOnly {
		fmt.Fprintf(opts.out, "# %s\n\n%s\n# %s\n\n%s", skillPath, claudeSkillMarkdown(), cmdPath, claudeSlashCommandMarkdown())
		return nil, nil
	}
	if err := writeSkillFile(skillPath, claudeSkillMarkdown(), opts.force); err != nil {
		return nil, err
	}
	if err := writeSkillFile(cmdPath, claudeSlashCommandMarkdown(), opts.force); err != nil {
		return nil, err
	}
	return []string{skillPath, cmdPath}, nil
}

func installCodexSkill(opts skillInstallOptions) ([]string, error) {
	base := opts.dir
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		base = filepath.Join(home, ".codex")
	}
	promptPath := filepath.Join(base, "prompts", "fleet.md")
	if opts.printOnly {
		fmt.Fprintf(opts.out, "# %s\n\n%s", promptPath, codexPromptMarkdown())
		return nil, nil
	}
	if err := writeSkillFile(promptPath, codexPromptMarkdown(), opts.force); err != nil {
		return nil, err
	}
	return []string{promptPath}, nil
}

func installAgentsFile(opts skillInstallOptions) ([]string, error) {
	dir := opts.dir
	if dir == "" {
		dir = "."
	}
	target := filepath.Join(dir, "AGENTS.md")
	content := agentsMarkdown()
	if opts.printOnly {
		fmt.Fprintf(opts.out, "# %s\n\n%s", target, content)
		return nil, nil
	}
	// AGENTS.md is commonly shared — append our section idempotently rather than
	// clobbering an existing file.
	if existing, err := os.ReadFile(target); err == nil {
		if strings.Contains(string(existing), agentsMarker) {
			fmt.Fprintf(opts.out, "%s already contains the Cenvero Fleet section; nothing to do\n", target)
			return nil, nil
		}
		merged := strings.TrimRight(string(existing), "\n") + "\n\n" + content
		if err := os.WriteFile(target, []byte(merged), 0o644); err != nil {
			return nil, err
		}
		return []string{target}, nil
	}
	if err := writeSkillFile(target, content, opts.force); err != nil {
		return nil, err
	}
	return []string{target}, nil
}

func writeSkillFile(path, content string, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; re-run with --force to overwrite", path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
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
		"command tree. Then operate with `fleet <group> <subcommand> ...`.\n\n" +
		"Guidance:\n" +
		"- Most commands print JSON to stdout — parse it.\n" +
		"- Check state first: `fleet status` and `fleet server list`.\n" +
		"- Add `--config-dir <path>` for a non-default controller location.\n" +
		"- Confirm before destructive actions: `server remove`, `file rm`, `key rotate`,\n" +
		"  `update apply`, `self-uninstall`, `config restore`.\n" +
		"- Move files with `fleet file upload|download|list`; interactive UIs are\n" +
		"  `fleet files <server>` (terminal) and `fleet ui` (browser).\n"
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
		"description: Load Cenvero Fleet context, then help manage the fleet\n" +
		"---\n\n" +
		"Run `fleet context` to load the full Cenvero Fleet command reference and concepts.\n" +
		"Then help me with the following (ask for clarification if needed):\n\n" +
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
