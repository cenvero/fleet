// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// completionLine returns the rc line that enables tab-completion for a shell.
func completionLine(sh string) (string, bool) {
	switch sh {
	case "bash":
		return "source <(fleet completion bash)", true
	case "zsh":
		return "source <(fleet completion zsh)", true
	case "fish":
		return "fleet completion fish | source", true
	default:
		return "", false
	}
}

func newAutocompleteCommand() *cobra.Command {
	var shellOverride string
	cmd := &cobra.Command{
		Use:   "autocomplete",
		Short: "Enable shell tab-completion for fleet (wraps 'fleet completion')",
		Long: "Print the one-liner that turns on tab-completion for your shell, or run\n" +
			"'fleet autocomplete install' to append it to your shell rc.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sh := shellOverride
			if sh == "" {
				sh = currentShellName()
			}
			line, ok := completionLine(sh)
			if !ok {
				return fmt.Errorf("unsupported shell %q (try --shell bash|zsh|fish)", sh)
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Enable fleet tab-completion for %s by adding this to your shell rc:\n\n  %s\n\nor run: fleet autocomplete install\n",
				sh, line)
			return nil
		},
	}
	cmd.Flags().StringVar(&shellOverride, "shell", "", "shell to target (bash|zsh|fish); default: $SHELL")
	cmd.AddCommand(newAutocompleteInstallCommand(&shellOverride))
	return cmd
}

func newAutocompleteInstallCommand(shellOverride *string) *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Append the completion line to your shell rc (idempotent)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sh := *shellOverride
			if sh == "" {
				sh = currentShellName()
			}
			line, ok := completionLine(sh)
			if !ok {
				return fmt.Errorf("unsupported shell %q (try --shell bash|zsh|fish)", sh)
			}
			rc, err := shellRCPathFor(sh)
			if err != nil {
				return err
			}
			added, err := appendOnce(rc, "cenvero-fleet:completion", "# cenvero-fleet:completion\n"+line+"\n")
			if err != nil {
				return err
			}
			if added {
				fmt.Fprintf(cmd.OutOrStdout(), "enabled %s completion in %s\nOpen a new shell or run: source %s\n", sh, rc, rc)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "fleet completion already enabled in %s\n", rc)
			}
			return nil
		},
	}
}
