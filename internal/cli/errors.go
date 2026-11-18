// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// newErrorsCommand is `fleet errors`: everything about the controller that
// needs attention, with the command that fixes each item. Ordinary commands
// do not print these any more (they used to appear before `fleet ssh`,
// `fleet exec` and every other command).
func newErrorsCommand(configDir *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "errors",
		Aliases: []string{"issues", "notices"},
		Short:   "Show what needs attention (updates, migrations, failed background work) and how to fix it",
		Long: "List everything about this controller that needs attention but is not the\n" +
			"result of a command you ran: an available update, a pending config migration,\n" +
			"a background agent sync that failed, reverse-mode servers waiting for the\n" +
			"daemon, and so on. Each item comes with the command that fixes it, and goes\n" +
			"away by itself once its cause is fixed.\n\n" +
			"Other commands no longer print these as they run; bare `fleet` and\n" +
			"`fleet status` mention how many there are. A command's own errors are still\n" +
			"reported by that command.",
		Example: "  fleet errors\n  fleet errors --json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			notices := core.CollectNotices(*configDir)
			if asJSON {
				if notices == nil {
					notices = []core.Notice{}
				}
				return writeJSON(cmd, notices)
			}
			writeNotices(cmd.OutOrStdout(), notices)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the items as JSON")
	return cmd
}

func writeNotices(w io.Writer, notices []core.Notice) {
	if len(notices) == 0 {
		fmt.Fprintln(w, "Nothing needs attention.")
		return
	}
	fmt.Fprintf(w, "%s need attention:\n", plural(len(notices), "thing"))
	for _, n := range notices {
		mark := "•"
		switch n.Severity {
		case core.NoticeError:
			mark = "✗"
		case core.NoticeWarning:
			mark = "⚠"
		}
		fmt.Fprintf(w, "\n  %s %s\n", mark, n.Title)
		if n.Detail != "" {
			fmt.Fprintf(w, "    %s\n", n.Detail)
		}
		if n.Fix != "" {
			fmt.Fprintf(w, "    Fix: %s\n", n.Fix)
		}
	}
}

// printNoticeSummary tells an operator at a terminal how many things need
// attention, on stderr so stdout (JSON for `fleet status`) stays clean. It is
// shown only by bare `fleet` and `fleet status`, never by commands such as
// `fleet ssh` or `fleet exec`.
func printNoticeSummary(cmd *cobra.Command, configDir string) {
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	if n := len(core.CollectNotices(configDir)); n > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "\n⚠  %s need attention — run `fleet errors` to see them and how to fix them.\n", plural(n, "thing"))
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
