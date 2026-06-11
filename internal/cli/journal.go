// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// FL-022 — journal/service log tailing.
//
// The existing `fleet logs` command (in root.go) reads tracked-service logs and
// the controller audit log. This command pages or streams the systemd journal
// for an arbitrary unit over the live agent transport:
//
//	fleet journal <server> --unit <name> [--since 1h] [--follow] [--grep PATTERN] [-n N]
//
// It runs `journalctl -u <unit>` via App.ExecCommand. There is no streaming exec
// primitive on App, so --follow polls the journal on a short interval and prints
// only newly-appended lines.
//
// newJournalCommand is exported so root.go can register it with
// root.AddCommand(newJournalCommand(&configDir)).

func newJournalCommand(configDir *string) *cobra.Command {
	var unit, since, grep string
	var lines int
	var follow bool

	cmd := &cobra.Command{
		Use:   "journal <server> --unit <name>",
		Short: "Page or follow the systemd journal for a unit",
		Long: `Read journalctl -u <unit> from a server over the live agent transport.

  fleet journal web-01 --unit nginx
  fleet journal web-01 --unit nginx --since 1h -n 200
  fleet journal web-01 --unit nginx --follow --grep error`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			server := args[0]
			if strings.TrimSpace(unit) == "" {
				return fmt.Errorf("--unit is required")
			}
			if err := validUnitName(unit); err != nil {
				return err
			}
			if err := validJournalSince(since); err != nil {
				return err
			}
			if lines <= 0 {
				lines = 200
			}
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			if follow {
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
				defer stop()
				return followJournal(ctx, cmd, app, server, unit, since, grep, lines)
			}
			return pageJournal(cmd, app, server, unit, since, grep, lines)
		},
	}
	cmd.Flags().StringVar(&unit, "unit", "", "systemd unit name (required)")
	cmd.Flags().StringVar(&since, "since", "", "journalctl --since window, e.g. '1h' or '2024-01-01'")
	cmd.Flags().StringVar(&grep, "grep", "", "only show lines matching this substring (case-insensitive)")
	cmd.Flags().IntVarP(&lines, "lines", "n", 200, "maximum number of lines to return")
	cmd.Flags().BoolVar(&follow, "follow", false, "stream new journal lines until Ctrl-C")
	return cmd
}

// journalCommand builds the remote journalctl invocation. tail caps the number of
// returned lines; sinceFloor, when set, anchors --since for follow polling.
func journalCommand(unit, since string, tail int) string {
	parts := []string{"journalctl", "-u", shellQuote(unit), "--no-pager"}
	if since != "" {
		parts = append(parts, "--since", shellQuote(since))
	}
	if tail > 0 {
		parts = append(parts, "-n", fmt.Sprintf("%d", tail))
	}
	return strings.Join(parts, " ")
}

// runJournal executes the remote journalctl and returns its non-empty output
// lines. journalctl exits 0 with a "-- No entries --" banner when empty; the
// caller treats that as no lines via grep/printing logic.
func runJournal(app *core.App, server, unit, since string, tail int) ([]string, error) {
	res, err := app.ExecCommand(server, journalCommand(unit, since, tail))
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("journalctl exited %d", res.ExitCode)
		}
		return nil, fmt.Errorf("journal read on %s failed: %s", server, msg)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

func matchGrep(line, grep string) bool {
	if grep == "" {
		return true
	}
	return strings.Contains(strings.ToLower(line), strings.ToLower(grep))
}

func pageJournal(cmd *cobra.Command, app *core.App, server, unit, since, grep string, lines int) error {
	out, err := runJournal(app, server, unit, since, lines)
	if err != nil {
		return err
	}
	for _, line := range out {
		if matchGrep(line, grep) {
			fmt.Fprintln(cmd.OutOrStdout(), line)
		}
	}
	return nil
}

// followJournal polls journalctl and prints only lines it has not seen before.
// journal lines carry no stable index over ExecCommand, so we de-duplicate by
// remembering the last line printed and skipping everything up to and including
// it on the next poll.
func followJournal(ctx context.Context, cmd *cobra.Command, app *core.App, server, unit, since, grep string, lines int) error {
	// Prime with the initial window so --follow shows recent context first.
	prev, err := runJournal(app, server, unit, since, lines)
	if err != nil {
		return err
	}
	for _, line := range prev {
		if matchGrep(line, grep) {
			fmt.Fprintln(cmd.OutOrStdout(), line)
		}
	}
	lastSeen := ""
	if len(prev) > 0 {
		lastSeen = prev[len(prev)-1]
	}

	interval := core.DefaultLogFollowInterval
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}

		// Re-read a small tail; new lines are whatever follows lastSeen.
		batch, err := runJournal(app, server, unit, since, lines)
		if err != nil {
			return err
		}
		fresh := newJournalLines(batch, lastSeen)
		for _, line := range fresh {
			if matchGrep(line, grep) {
				fmt.Fprintln(cmd.OutOrStdout(), line)
			}
		}
		if len(batch) > 0 {
			lastSeen = batch[len(batch)-1]
		}
	}
}

// newJournalLines returns the lines in batch that appear after the last
// occurrence of lastSeen. When lastSeen is absent from the batch (log rotated or
// the tail window moved past it) the whole batch is treated as new.
func newJournalLines(batch []string, lastSeen string) []string {
	if lastSeen == "" {
		return batch
	}
	for i := len(batch) - 1; i >= 0; i-- {
		if batch[i] == lastSeen {
			return batch[i+1:]
		}
	}
	return batch
}

// validJournalSince guards the --since value so it cannot smuggle shell words
// into the remote command. journalctl accepts free-form times, but we restrict
// to a safe character set (digits, letters, space, dash, colon, dot).
func validJournalSince(since string) error {
	since = strings.TrimSpace(since)
	if since == "" {
		return nil
	}
	if len(since) > 64 {
		return fmt.Errorf("--since value too long")
	}
	for _, r := range since {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == ' ', r == '-', r == ':', r == '.', r == '+':
		default:
			return fmt.Errorf("invalid --since value %q: contains %q", since, string(r))
		}
	}
	return nil
}
