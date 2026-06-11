// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// automationDir holds named shell scripts that `fleet shell-init` loads into new
// shells. It lives under the config dir so it travels with the controller config.
func automationDir(configDir string) string { return filepath.Join(configDir, "automations") }

// validAutomationName keeps a name confined to the store directory.
func validAutomationName(name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("invalid automation name %q (no path separators or '..')", name)
	}
	return nil
}

func automationPath(configDir, name string) (string, error) {
	if err := validAutomationName(name); err != nil {
		return "", err
	}
	return filepath.Join(automationDir(configDir), name+".sh"), nil
}

func newAutomationCommand(configDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "automation",
		Short: "Store named shell scripts; load the latest into every shell via 'fleet shell-init'",
		Long: "Manage named automation scripts. Store one with 'set', print it with 'get'\n" +
			"(raw, safe to eval), and have every new shell load the latest version with\n" +
			"'fleet shell-init'. Updating a script with 'set' is picked up by the next shell.",
	}
	cmd.AddCommand(newAutomationSetCommand(configDir))
	cmd.AddCommand(newAutomationGetCommand(configDir))
	cmd.AddCommand(newAutomationListCommand(configDir))
	cmd.AddCommand(newAutomationRemoveCommand(configDir))
	return cmd
}

func newAutomationSetCommand(configDir *string) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Store (replace) a named script from --file, or stdin",
		Long: "Store a script under <name>, replacing any existing one. Reads from --file,\n" +
			"or from stdin (the default, or with --file -).\n\n" +
			"  fleet automation set deploy --file ./deploy.sh\n" +
			"  echo 'alias k=kubectl' | fleet automation set default",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := automationPath(*configDir, args[0])
			if err != nil {
				return err
			}
			var body []byte
			if file != "" && file != "-" {
				body, err = os.ReadFile(file) // #nosec G304 -- operator-chosen file
			} else {
				body, err = io.ReadAll(cmd.InOrStdin())
			}
			if err != nil {
				return err
			}
			if err := os.MkdirAll(automationDir(*configDir), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(path, body, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stored automation %q (%d bytes)\n", args[0], len(body))
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "read the script from this file ('-' or empty = stdin)")
	return cmd
}

func newAutomationGetCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Print a stored script (raw; safe to eval). Missing name = empty output, exit 0.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := automationPath(*configDir, args[0])
			if err != nil {
				return err
			}
			body, err := os.ReadFile(path) // #nosec G304 -- name validated, confined to the store dir
			if err != nil {
				if os.IsNotExist(err) {
					return nil // empty output, exit 0 — safe for `eval "$(fleet automation get ...)"`
				}
				return err
			}
			_, err = cmd.OutOrStdout().Write(body)
			return err
		},
	}
}

func newAutomationListCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List stored automation scripts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := os.ReadDir(automationDir(*configDir))
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Fprintln(cmd.OutOrStdout(), "no automations stored")
					return nil
				}
				return err
			}
			type row struct {
				name string
				size int64
				mod  time.Time
			}
			var rows []row
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".sh") {
					continue
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				rows = append(rows, row{strings.TrimSuffix(e.Name(), ".sh"), info.Size(), info.ModTime()})
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no automations stored")
				return nil
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSIZE\tMODIFIED")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%dB\t%s\n", r.name, r.size, r.mod.Format("2006-01-02 15:04"))
			}
			return tw.Flush()
		},
	}
}

func newAutomationRemoveCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove", "delete"},
		Short:   "Delete a stored automation script",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := automationPath(*configDir, args[0])
			if err != nil {
				return err
			}
			if err := os.Remove(path); err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("no automation named %q", args[0])
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed automation %q\n", args[0])
			return nil
		},
	}
}
