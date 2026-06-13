// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"github.com/spf13/cobra"

	"github.com/cenvero/fleet/internal/core"
)

// completeServerNames returns server-name candidates (with descriptions) for the
// given prefix, formatted as Cobra expects ("name\tdescription"). It reads the
// server list straight from disk via core.ServerCompletions — no App/DB open —
// so it stays fast on every <tab>. Cobra itself filters by prefix, but we filter
// too so the description column doesn't leak non-matching rows.
func completeServerNames(configDir *string, toComplete string) ([]string, cobra.ShellCompDirective) {
	dir := ""
	if configDir != nil {
		dir = *configDir
	}
	servers := core.ServerCompletions(dir)
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		if toComplete != "" && !hasPrefixFold(s.Name, toComplete) {
			continue
		}
		if s.Description != "" {
			out = append(out, s.Name+"\t"+s.Description)
		} else {
			out = append(out, s.Name)
		}
	}
	// NoFileComp: a server slot should never fall back to filesystem paths.
	return out, cobra.ShellCompDirectiveNoFileComp
}

// serverNameComp is a Cobra ValidArgsFunction that completes ONE server name in
// the first positional slot. Once that slot is filled it stays silent (and
// suppresses file completion) so later positionals — a command, a path — are not
// polluted with server names.
func serverNameComp(configDir *string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) != 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeServerNames(configDir, toComplete)
	}
}

// serverNameCompAt is like serverNameComp but completes the server name only at a
// specific positional index (e.g. `server mode <name> <mode>` → index 0). Other
// positions return no completion.
func serverNameCompAt(configDir *string, index int) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) != index {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeServerNames(configDir, toComplete)
	}
}

// hasPrefixFold reports whether s starts with prefix, case-insensitively, without
// allocating. Server names are case-sensitive on disk but operators tab-complete
// without worrying about case.
func hasPrefixFold(s, prefix string) bool {
	if len(prefix) > len(s) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		c, p := s[i], prefix[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if 'A' <= p && p <= 'Z' {
			p += 'a' - 'A'
		}
		if c != p {
			return false
		}
	}
	return true
}
