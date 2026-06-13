// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// ServerCompletion is a minimal server summary for shell completion: the name
// plus a short human-readable description (address · mode).
type ServerCompletion struct {
	Name        string
	Description string
}

// ServerCompletions reads <configDir>/servers/*.toml and returns name+description
// pairs for shell tab-completion, sorted by name.
//
// It is deliberately lightweight: shell completion runs in a child `fleet`
// process on every <tab>, so this must be fast and side-effect-free. Unlike
// App.ListServers it does NOT open the App or its state/metrics databases — it
// just decodes the per-server TOML files. It is best-effort: a missing directory
// or an unreadable/garbled entry yields no error, only fewer candidates, so a
// half-written server file never breaks the operator's tab key.
func ServerCompletions(configDir string) []ServerCompletion {
	if configDir == "" {
		configDir = DefaultConfigDir("")
	}
	dir := filepath.Join(configDir, "servers")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]ServerCompletion, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".toml" {
			continue
		}
		var s ServerRecord
		if _, err := toml.DecodeFile(filepath.Join(dir, entry.Name()), &s); err != nil {
			continue // skip unreadable entries; completion must not fail
		}
		if s.Name == "" {
			continue
		}
		desc := s.Address
		if mode := string(s.Mode); mode != "" {
			if desc != "" {
				desc += " · " + mode
			} else {
				desc = mode
			}
		}
		out = append(out, ServerCompletion{Name: s.Name, Description: desc})
	}
	slices.SortFunc(out, func(a, b ServerCompletion) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out
}
