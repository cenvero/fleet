// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// TestUIAuthorizerRefusesServerScopedTokens: the web UI's overview reads are
// fleet-wide, and core.Authorize only checks a server scope when a target is
// named, so the UI authorizer must refuse a server-scoped token itself instead
// of listing out-of-scope servers. Unscoped and command-scoped tokens keep the
// CLI's own verdicts.
func TestUIAuthorizerRefusesServerScopedTokens(t *testing.T) {
	dir := newServerCommandTestConfig(t)
	tokens := core.NewTokenStore(dir)
	scoped, err := tokens.Create(core.Token{Name: "webonly", Servers: []string{"web-01"}})
	if err != nil {
		t.Fatal(err)
	}
	grouped, err := tokens.Create(core.Token{Name: "webs", Groups: []string{"role=web"}})
	if err != nil {
		t.Fatal(err)
	}
	fileOnly, err := tokens.Create(core.Token{Name: "files", AllowCommands: []string{"file"}})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := tokens.Create(core.Token{Name: "admin"})
	if err != nil {
		t.Fatal(err)
	}

	authFor := func(id string) func(string) error {
		t.Setenv("FLEET_TOKEN", id)
		return uiCommandAuthorizer(&cobra.Command{}, dir)
	}
	for _, id := range []string{scoped.ID, grouped.ID} {
		for _, command := range []string{"server", "alerts", "tag"} {
			if err := authFor(id)(command); err == nil || !strings.Contains(err.Error(), "server-scoped") {
				t.Fatalf("server-scoped token %s: %q read = %v, want a server-scoped denial", id, command, err)
			}
		}
	}
	if err := authFor(fileOnly.ID)("server"); err == nil {
		t.Fatal("a token allowed only 'file' must still be denied the server list")
	}
	if err := authFor(admin.ID)("server"); err != nil {
		t.Fatalf("an unscoped token must read the overview: %v", err)
	}
	if auth := authFor(""); auth != nil {
		t.Fatal("no token: the UI must run unrestricted (nil authorizer)")
	}
}
