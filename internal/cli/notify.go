// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// newNotifyCommand builds `fleet notify` for managing notification targets
// (Slack incoming webhooks and generic webhooks) persisted in notify.json.
//
//	fleet notify add <slack|webhook> <url> --on offline,job-failed,destructive
//	fleet notify list
//	fleet notify rm <index|url>
//	fleet notify test [--event offline]
func newNotifyCommand(configDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "notify",
		Short: "Manage notification targets (Slack / webhooks) for fleet events",
		Long: "Configure where Cenvero Fleet sends event notifications. Targets are stored\n" +
			"locally in the controller config dir (notify.json); they don't touch the\n" +
			"managed servers.\n\n" +
			"A target has a kind (slack or webhook), a URL, and the events it subscribes to.\n" +
			"Known events: " + strings.Join(core.NotifyEvents, ", ") + ".\n\n" +
			"Examples:\n" +
			"  fleet notify add slack https://hooks.slack.com/services/XXX --on offline,job-failed\n" +
			"  fleet notify add webhook https://example.com/hook --on destructive,drift\n" +
			"  fleet notify list\n" +
			"  fleet notify rm 0\n" +
			"  fleet notify test --event offline",
	}
	cmd.AddCommand(newNotifyAddCommand(configDir))
	cmd.AddCommand(newNotifyListCommand(configDir))
	cmd.AddCommand(newNotifyRemoveCommand(configDir))
	cmd.AddCommand(newNotifyTestCommand(configDir))
	return cmd
}

func newNotifyAddCommand(configDir *string) *cobra.Command {
	var on string
	cmd := &cobra.Command{
		Use:   "add <slack|webhook> <url>",
		Short: "Add a notification target",
		Long: "Add a Slack incoming-webhook or generic-webhook target subscribed to one or\n" +
			"more events (comma-separated via --on). Adding a target whose kind+url already\n" +
			"exists replaces its event set.\n\n" +
			"Known events: " + strings.Join(core.NotifyEvents, ", ") + ".",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			events := splitEvents(on)
			if len(events) == 0 {
				return fmt.Errorf("--on is required: a comma-separated list of events (%s)", strings.Join(core.NotifyEvents, ", "))
			}
			store := core.NewNotifyStore(*configDir)
			target := core.NotifyTarget{
				Kind:   core.NotifyKind(args[0]),
				URL:    args[1],
				Events: events,
			}
			if err := store.Add(target); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added %s target %s (events: %s)\n",
				strings.ToLower(args[0]), args[1], strings.Join(events, ", "))
			return nil
		},
	}
	cmd.Flags().StringVar(&on, "on", "", "comma-separated events to subscribe to ("+strings.Join(core.NotifyEvents, ", ")+")")
	return cmd
}

func newNotifyListCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured notification targets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store := core.NewNotifyStore(*configDir)
			targets, err := store.List()
			if err != nil {
				return err
			}
			if len(targets) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no notification targets configured")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			if _, err := fmt.Fprintln(w, "#\tKIND\tURL\tEVENTS"); err != nil {
				return err
			}
			for i, t := range targets {
				if _, err := fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", i, t.Kind, t.URL, strings.Join(t.Events, ",")); err != nil {
					return err
				}
			}
			return w.Flush()
		},
	}
}

func newNotifyRemoveCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <index|url>",
		Aliases: []string{"remove", "delete"},
		Short:   "Remove a notification target by index or exact url",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store := core.NewNotifyStore(*configDir)
			removed, err := store.Remove(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s target %s\n", removed.Kind, removed.URL)
			return nil
		},
	}
}

func newNotifyTestCommand(configDir *string) *cobra.Command {
	var event string
	cmd := &cobra.Command{
		Use:   "test",
		Short: "Send a test notification to matching targets",
		Long: "Send a test message to every target subscribed to --event (default 'offline').\n" +
			"Use this to confirm a webhook or Slack URL is reachable and accepted.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			event = strings.ToLower(strings.TrimSpace(event))
			if !core.ValidNotifyEvent(event) {
				return fmt.Errorf("invalid --event %q (want one of %s)", event, strings.Join(core.NotifyEvents, ", "))
			}
			store := core.NewNotifyStore(*configDir)
			targets, err := store.List()
			if err != nil {
				return err
			}
			message := fmt.Sprintf("Cenvero Fleet test notification (event=%s)", event)
			sent, failed := 0, 0
			for _, t := range targets {
				if !t.Subscribed(event) {
					continue
				}
				if sendErr := store.Send(t, event, message); sendErr != nil {
					failed++
					fmt.Fprintf(cmd.ErrOrStderr(), "  FAIL %s %s: %v\n", t.Kind, t.URL, sendErr)
					continue
				}
				sent++
				fmt.Fprintf(cmd.OutOrStdout(), "  OK   %s %s\n", t.Kind, t.URL)
			}
			if sent == 0 && failed == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "no targets subscribed to %q\n", event)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "test complete: %d sent, %d failed\n", sent, failed)
			if failed > 0 {
				return fmt.Errorf("%d of %d test notifications failed", failed, sent+failed)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&event, "event", core.NotifyEventOffline, "event to test ("+strings.Join(core.NotifyEvents, ", ")+")")
	return cmd
}

// splitEvents splits a comma-separated event list, trimming and dropping blanks.
func splitEvents(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
