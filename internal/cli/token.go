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

// newTokenCommand builds `fleet token` for managing scoped RBAC tokens.
//
// FL-030: tokens drive CONTROLLER-side enforcement. A scoped token, passed via
// `--token <id>` (or the FLEET_TOKEN env var), limits which commands and servers
// a controller invocation may touch. Tokens are stored locally in the config dir
// (tokens.json) and never touch the managed servers.
func newTokenCommand(configDir *string) *cobra.Command {
	tokenCmd := &cobra.Command{
		Use:   "token",
		Short: "Manage scoped RBAC tokens (controller-side enforcement)",
		Long: "Create, list, and revoke scoped RBAC tokens. A token limits which commands\n" +
			"and servers a controller invocation may touch when it is passed with\n" +
			"--token <id> (or the FLEET_TOKEN environment variable).\n\n" +
			"Tokens are stored locally in the controller config dir (tokens.json); they\n" +
			"never touch the managed servers. This is controller-side enforcement only;\n" +
			"agent-side per-RPC validation is a future hardening (FL-030 server-side).",
	}

	tokenCmd.AddCommand(newTokenCreateCommand(configDir))
	tokenCmd.AddCommand(newTokenListCommand(configDir))
	tokenCmd.AddCommand(newTokenRevokeCommand(configDir))
	return tokenCmd
}

func newTokenCreateCommand(configDir *string) *cobra.Command {
	var (
		name            string
		servers         []string
		group           string
		allow           []string
		deny            []string
		readOnlyDefault bool
		destructive     bool
	)
	cmd := &cobra.Command{
		Use:   "create --name <n> [scope flags]",
		Short: "Create a scoped token and print its ID once",
		Long: "Create a scoped token. The token ID is a bearer credential and is printed\n" +
			"exactly once — store it securely.\n\n" +
			"Scope flags:\n" +
			"  --servers a,b         restrict to these servers\n" +
			"  --group EXPR          restrict to servers matching a tag expr (e.g. role=web)\n" +
			"  --allow exec,file     only these top-level commands are permitted\n" +
			"  --deny server,key     these top-level commands are denied\n" +
			"  --read-only-default   deny non-read commands unless explicitly allowed\n" +
			"  --destructive         permit destructive ops (server remove, file rm, ...)\n\n" +
			"Examples:\n" +
			"  fleet token create --name ci --allow exec --group role=web --read-only-default\n" +
			"  fleet token create --name ops --servers web-01,web-02 --destructive",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store := core.NewTokenStore(*configDir)
			t := core.Token{
				Name:               strings.TrimSpace(name),
				Servers:            splitCommaFlags(servers),
				AllowCommands:      splitCommaFlags(allow),
				DenyCommands:       splitCommaFlags(deny),
				ReadOnlyDefault:    readOnlyDefault,
				DestructiveAllowed: destructive,
			}
			if g := strings.TrimSpace(group); g != "" {
				t.Groups = []string{g}
			}
			created, err := store.Create(t)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Token %q created.\n\n", created.Name)
			fmt.Fprintf(out, "  %s\n\n", created.ID)
			fmt.Fprintln(out, "Store this token securely — it is shown only once and grants the scope above.")
			fmt.Fprintln(out, "Use it with: fleet --token <id> <command>   (or set FLEET_TOKEN).")
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "human-readable token name (required)")
	cmd.Flags().StringSliceVar(&servers, "servers", nil, "comma-separated server names to scope to")
	cmd.Flags().StringVar(&group, "group", "", "tag filter expression to scope to (e.g. role=web)")
	cmd.Flags().StringSliceVar(&allow, "allow", nil, "comma-separated top-level commands to allow (allow-list)")
	cmd.Flags().StringSliceVar(&deny, "deny", nil, "comma-separated top-level commands to deny")
	cmd.Flags().BoolVar(&readOnlyDefault, "read-only-default", false, "deny non-read commands unless explicitly allowed")
	cmd.Flags().BoolVar(&destructive, "destructive", false, "permit destructive operations")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newTokenListCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List tokens (IDs are shown as a short prefix only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store := core.NewTokenStore(*configDir)
			tokens, err := store.List()
			if err != nil {
				return err
			}
			if len(tokens) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no tokens")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tSCOPE")
			for _, t := range tokens {
				fmt.Fprintf(w, "%s\t%s\t%s\n", tokenPrefix(t.ID), t.Name, tokenScopeSummary(t))
			}
			return w.Flush()
		},
	}
}

func newTokenRevokeCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke (delete) a token by ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store := core.NewTokenStore(*configDir)
			if err := store.Revoke(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked token %s\n", tokenPrefix(args[0]))
			return nil
		},
	}
}

// tokenPrefix returns a short, non-secret prefix for display (never the full ID).
func tokenPrefix(id string) string {
	if len(id) <= 8 {
		return id + "…"
	}
	return id[:8] + "…"
}

// tokenScopeSummary renders a compact one-line description of a token's scope.
func tokenScopeSummary(t core.Token) string {
	var parts []string
	if len(t.Servers) > 0 {
		parts = append(parts, "servers="+strings.Join(t.Servers, ","))
	}
	if len(t.Groups) > 0 {
		parts = append(parts, "groups="+strings.Join(t.Groups, ","))
	}
	if len(t.AllowCommands) > 0 {
		parts = append(parts, "allow="+strings.Join(t.AllowCommands, ","))
	}
	if len(t.DenyCommands) > 0 {
		parts = append(parts, "deny="+strings.Join(t.DenyCommands, ","))
	}
	if t.ReadOnlyDefault {
		parts = append(parts, "read-only")
	}
	if t.DestructiveAllowed {
		parts = append(parts, "destructive")
	}
	if len(parts) == 0 {
		return "(unrestricted)"
	}
	return strings.Join(parts, " ")
}

// splitCommaFlags normalizes a StringSlice flag: trims each element and drops
// empties, so "--allow exec, file" and repeated flags both produce clean lists.
func splitCommaFlags(in []string) []string {
	var out []string
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
