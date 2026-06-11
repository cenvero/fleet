// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// newTagCommand builds `fleet tag` for managing per-server key=value tags.
//
//	fleet tag <server> key=value [key=value...]  set (empty value deletes a key)
//	fleet tag <server>                           show one server's tags
//	fleet tag --list | fleet tag list            show all servers and their tags
func newTagCommand(configDir *string) *cobra.Command {
	var list bool
	cmd := &cobra.Command{
		Use:   "tag [<server> [key=value...]]",
		Short: "Tag servers with key=value labels and group them",
		Long: "Attach arbitrary key=value tags to servers and use them to group and filter.\n" +
			"Tags are stored locally in the controller config dir (tags.json); they don't\n" +
			"touch the managed servers.\n\n" +
			"Examples:\n" +
			"  fleet tag web-01 role=plesk env=prod   # set tags\n" +
			"  fleet tag web-01 env=                   # delete the env tag\n" +
			"  fleet tag web-01                        # show one server's tags\n" +
			"  fleet tag --list                        # show all servers and tags\n\n" +
			"Filter expressions like role=plesk or role=plesk,env=prod (comma = AND) are\n" +
			"consumed by other commands (e.g. fleet inventory).",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// `fleet tag list` is an alias for --list.
			if !list && len(args) == 1 && args[0] == "list" {
				list = true
				args = nil
			}
			if list {
				if len(args) > 0 {
					return fmt.Errorf("--list takes no arguments")
				}
				return runTagList(cmd, *configDir)
			}
			if len(args) == 0 {
				return cmd.Help()
			}

			server := args[0]
			pairs := args[1:]
			store := core.NewTagStore(*configDir)

			if len(pairs) == 0 {
				return runTagShow(cmd, store, server)
			}
			return runTagSet(cmd, store, server, pairs)
		},
	}
	cmd.Flags().BoolVar(&list, "list", false, "list all servers and their tags")
	return cmd
}

// runTagSet parses key=value pairs and stores them for the server.
func runTagSet(cmd *cobra.Command, store *core.TagStore, server string, pairs []string) error {
	kv := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return fmt.Errorf("invalid tag %q: expected key=value", pair)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" {
			return fmt.Errorf("invalid tag %q: empty key", pair)
		}
		kv[k] = v
	}
	if err := store.SetTags(server, kv); err != nil {
		return err
	}
	return runTagShow(cmd, store, server)
}

// runTagShow prints a single server's tags as a sorted table.
func runTagShow(cmd *cobra.Command, store *core.TagStore, server string) error {
	tags := store.GetTags(server)
	out := cmd.OutOrStdout()
	if len(tags) == 0 {
		fmt.Fprintf(out, "%s has no tags\n", server)
		return nil
	}
	keys := sortedTagKeys(tags)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "KEY\tVALUE"); err != nil {
		return err
	}
	for _, k := range keys {
		if _, err := fmt.Fprintf(w, "%s\t%s\n", k, tags[k]); err != nil {
			return err
		}
	}
	return w.Flush()
}

// runTagList prints every server's tags. Servers with no tags are shown too so
// the operator can see the full fleet at a glance.
func runTagList(cmd *cobra.Command, configDir string) error {
	app, err := openApp(configDir)
	if err != nil {
		return err
	}
	defer app.Close()

	servers, err := app.ListServers()
	if err != nil {
		return err
	}
	store := core.NewTagStore(configDir)
	all := store.AllTags()

	out := cmd.OutOrStdout()
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "SERVER\tTAGS"); err != nil {
		return err
	}
	for _, server := range servers {
		if _, err := fmt.Fprintf(w, "%s\t%s\n", server.Name, formatTags(all[server.Name])); err != nil {
			return err
		}
	}
	return w.Flush()
}

// formatTags renders a tag map as "k=v, k2=v2" with keys sorted, or "-" if empty.
func formatTags(tags map[string]string) string {
	if len(tags) == 0 {
		return "-"
	}
	keys := sortedTagKeys(tags)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+tags[k])
	}
	return strings.Join(parts, ", ")
}

func sortedTagKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
