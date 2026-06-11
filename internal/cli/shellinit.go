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

func newShellInitCommand() *cobra.Command {
	var install bool
	cmd := &cobra.Command{
		Use:   "shell-init [name]",
		Short: "Print/install a shell snippet that loads the latest automation in every new shell",
		Long: "Emit a snippet for your shell rc that runs 'fleet automation get <name>' and evals\n" +
			"it, so every new terminal loads the latest stored automation (update it with\n" +
			"'fleet automation set'). With --install, append it to your rc idempotently.\n" +
			"Default name: 'default'.\n\n" +
			"  fleet shell-init             # print the snippet\n" +
			"  fleet shell-init --install   # add it to your shell rc\n" +
			"  eval \"$(fleet shell-init deploy)\"   # load it into the current shell now",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := "default"
			if len(args) == 1 {
				name = args[0]
			}
			if err := validAutomationName(name); err != nil {
				return err
			}
			snippet := shellInitSnippet(name)
			if !install {
				fmt.Fprint(cmd.OutOrStdout(), snippet)
				return nil
			}
			rc, err := shellRCPath()
			if err != nil {
				return err
			}
			added, err := appendOnce(rc, shellInitMarker(name), snippet)
			if err != nil {
				return err
			}
			if added {
				fmt.Fprintf(cmd.OutOrStdout(), "added Fleet automation loader %q to %s\nOpen a new shell or run: source %s\n", name, rc, rc)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Fleet automation loader %q already present in %s\n", name, rc)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&install, "install", false, "append the snippet to your shell rc file")
	return cmd
}

func shellInitMarker(name string) string { return "cenvero-fleet:automation:" + name }

func shellInitSnippet(name string) string {
	m := shellInitMarker(name)
	return fmt.Sprintf(`# >>> %s >>>
if command -v fleet >/dev/null 2>&1; then
  eval "$(fleet automation get %s 2>/dev/null)"
fi
# <<< %s <<<
`, m, name, m)
}

// ---- shared shell helpers (also used by autocomplete) ----

func currentShellName() string { return filepath.Base(os.Getenv("SHELL")) }

// shellRCPathFor returns the rc file path for a shell name.
func shellRCPathFor(sh string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch sh {
	case "zsh":
		return filepath.Join(home, ".zshrc"), nil
	case "bash":
		return filepath.Join(home, ".bashrc"), nil
	case "fish":
		return filepath.Join(home, ".config", "fish", "config.fish"), nil
	default:
		return filepath.Join(home, ".profile"), nil
	}
}

func shellRCPath() (string, error) { return shellRCPathFor(currentShellName()) }

// appendOnce appends content to path if marker isn't already present. Returns
// whether it added anything.
func appendOnce(path, marker, content string) (bool, error) {
	existing, err := os.ReadFile(path) // #nosec G304 -- the operator's own shell rc file
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if strings.Contains(string(existing), marker) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- the operator's own shell rc file
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.WriteString("\n" + content)
	return err == nil, err
}
