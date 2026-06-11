// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// newSecretCommand builds `fleet secret` for managing named secrets (FL-004,
// controller-side v1).
//
// Secrets are stored locally in the controller config dir (secrets.json, 0600)
// and never touch the managed servers from this command. Values are write-only
// from the operator's point of view: `set`/`generate`/`rotate` store a value,
// but no command ever prints or returns it. `exec --secret VAR=@name` is the
// only consumer, and it injects values as environment while adding them to the
// output redaction set so they never appear in stdout/stderr/audit/echo.
func newSecretCommand(configDir *string) *cobra.Command {
	secretCmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage named secrets (local, never printed)",
		Long: "Store named secrets in the controller config dir (secrets.json, 0600). Values\n" +
			"are write-only: 'set', 'generate', and 'rotate' store a value but no command\n" +
			"ever prints or returns it; 'list' shows names and creation times only.\n\n" +
			"Use a secret in a command with:\n" +
			"  fleet exec <server> --secret VAR=@name -- <cmd>\n\n" +
			"The value is injected as the VAR environment variable for that command and is\n" +
			"redacted from all output (stdout, stderr, audit, the echoed/dry-run command).",
	}

	secretCmd.AddCommand(newSecretSetCommand(configDir))
	secretCmd.AddCommand(newSecretListCommand(configDir))
	secretCmd.AddCommand(newSecretRotateCommand(configDir))
	secretCmd.AddCommand(newSecretRemoveCommand(configDir))
	return secretCmd
}

func newSecretSetCommand(configDir *string) *cobra.Command {
	var (
		value    string
		generate int
	)
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Store a secret from --value, --generate N, or stdin (never echoed)",
		Long: "Store a secret value under <name>. The value comes from exactly one source:\n" +
			"  --value <v>     take the value from the flag (visible in your shell history)\n" +
			"  --generate N    generate a random N-char alphanumeric value and store it\n" +
			"  (stdin)         with neither flag, read the value from stdin\n\n" +
			"The value is NEVER echoed. On --generate, only a confirmation with the length\n" +
			"is printed; the generated value is stored and never shown.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			store := core.NewSecretStore(*configDir)

			valueSet := cmd.Flags().Changed("value")
			if generate > 0 && valueSet {
				return fmt.Errorf("pass only one of --value or --generate")
			}

			if generate > 0 {
				if err := store.Generate(name, generate); err != nil {
					return err
				}
				// Print only the length — never the generated value.
				fmt.Fprintf(cmd.OutOrStdout(), "stored secret %s (%d chars)\n", name, generate)
				return nil
			}

			if !valueSet {
				// Read the value from stdin (e.g. piped or here-doc). Trim a single
				// trailing newline so `echo secret | fleet secret set x` is clean,
				// but preserve any other surrounding bytes the caller intended.
				data, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("read secret from stdin: %w", err)
				}
				value = strings.TrimRight(string(data), "\r\n")
				if value == "" {
					return fmt.Errorf("no value provided on stdin (use --value or --generate, or pipe a value)")
				}
			}

			if err := store.Set(name, value); err != nil {
				return err
			}
			// Confirm without echoing the value.
			fmt.Fprintf(cmd.OutOrStdout(), "stored secret %s\n", name)
			return nil
		},
	}
	cmd.Flags().StringVar(&value, "value", "", "secret value (visible in shell history; prefer stdin or --generate)")
	cmd.Flags().IntVar(&generate, "generate", 0, "generate and store a random N-character alphanumeric value")
	return cmd
}

func newSecretListCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List secret names and creation times (never values)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store := core.NewSecretStore(*configDir)
			metas, err := store.List()
			if err != nil {
				return err
			}
			if len(metas) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no secrets stored")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tCREATED")
			for _, m := range metas {
				created := ""
				if !m.Created.IsZero() {
					created = m.Created.UTC().Format(time.RFC3339)
				}
				fmt.Fprintf(w, "%s\t%s\n", m.Name, created)
			}
			return w.Flush()
		},
	}
}

func newSecretRotateCommand(configDir *string) *cobra.Command {
	var length int
	cmd := &cobra.Command{
		Use:   "rotate <name>",
		Short: "Replace a secret with a freshly generated value (never echoed)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			store := core.NewSecretStore(*configDir)
			if err := store.Rotate(name, length); err != nil {
				return err
			}
			// Print only the length — never the new value.
			fmt.Fprintf(cmd.OutOrStdout(), "rotated secret %s (%d chars)\n", name, length)
			return nil
		},
	}
	cmd.Flags().IntVar(&length, "length", 40, "length of the new random value")
	return cmd
}

func newSecretRemoveCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Remove a secret",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			store := core.NewSecretStore(*configDir)
			if err := store.Remove(name); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed secret %s\n", name)
			return nil
		},
	}
}
