// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"os"
	"strings"
	"testing"
)

func TestTokenStoreCreateGetListRevoke(t *testing.T) {
	dir := t.TempDir()
	store := NewTokenStore(dir)

	created, err := store.Create(Token{Name: "ci", AllowCommands: []string{"exec"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("Create did not fill ID")
	}
	if len(created.ID) != 32 {
		t.Fatalf("ID = %q, want 32 hex chars", created.ID)
	}
	if created.Created.IsZero() {
		t.Fatal("Create did not fill Created")
	}

	// File must be 0600.
	info, err := os.Stat(TokensPath(dir))
	if err != nil {
		t.Fatalf("stat tokens.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("tokens.json perm = %o, want 600", perm)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "ci" {
		t.Fatalf("Get name = %q, want ci", got.Name)
	}

	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List len = %d, want 1", len(list))
	}

	if err := store.Revoke(created.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := store.Get(created.ID); err == nil {
		t.Fatal("Get after Revoke should fail")
	}
	if err := store.Revoke(created.ID); err == nil {
		t.Fatal("Revoke of unknown token should fail")
	}
	if err := store.Revoke("does-not-exist"); err == nil {
		t.Fatal("Revoke of unknown token should fail")
	}
}

func TestCreateRequiresName(t *testing.T) {
	store := NewTokenStore(t.TempDir())
	if _, err := store.Create(Token{Name: "   "}); err == nil {
		t.Fatal("Create with blank name should fail")
	}
}

func TestIsDestructiveCommand(t *testing.T) {
	// RUNTIME args shape: cobra has already consumed the subcommand by the time
	// enforcement runs, so the subcommand is passed explicitly as `sub` and the
	// positional args (here) do NOT contain it — args[0] is the first leaf
	// positional (typically a server name), which must NOT drive classification.
	cases := []struct {
		top  string
		sub  string
		args []string // leaf positionals at runtime (no subcommand)
		want bool
	}{
		// server
		{"server", "remove", []string{"web-01"}, true},
		{"server", "list", nil, false},
		// file
		{"file", "rm", []string{"web-01", "/tmp/x"}, true},
		{"file", "list", []string{"web-01"}, false},
		// key: any mutating sub destructive, read subs exempt
		{"key", "rotate", nil, true},
		{"key", "regenerate", nil, true},
		{"key", "fingerprint", nil, false},
		{"key", "list", nil, false},
		{"key", "", nil, false}, // bare key = help/list
		// firewall / fw
		{"firewall", "enable", []string{"web-01"}, true},
		{"firewall", "status", []string{"web-01"}, false},
		{"fw", "enable", []string{"web-01"}, true},
		// guard / revert: any sub destructive
		{"guard", "web-01", []string{"rm -rf /"}, true},
		{"revert", "", []string{"abc123"}, true},
		// newly-added destructive set
		{"tag", "set", []string{"web-01", "role=web"}, true},
		{"tag", "list", nil, false},
		{"port", "open", []string{"web-01", "80"}, true},
		{"port", "close", []string{"web-01", "80"}, true},
		{"port", "list", []string{"web-01"}, false},
		{"cron", "add", []string{"web-01"}, true},
		{"cron", "rm", []string{"web-01"}, true},
		{"cron", "list", []string{"web-01"}, false},
		{"cmd-policy", "set", nil, true},
		{"cmd-policy", "list", nil, false},
		{"secret", "set", []string{"name"}, true},
		{"secret", "rotate", []string{"name"}, true},
		{"secret", "rm", []string{"name"}, true},
		{"secret", "list", nil, false},
		{"policy", "set", nil, true},
		{"policy", "show", nil, false},
		// config: mutating subs destructive, read subs exempt
		{"config", "set", nil, true},
		{"config", "edit", nil, true},
		{"config", "show", nil, false},
		{"config", "", nil, false},
		// non-destructive
		{"drift", "", []string{"web-01"}, false},
		{"exec", "", []string{"web-01", "uptime"}, false},
		{"status", "", nil, false},
		// CRITICAL regression: a server name in args[0] must NOT be mistaken for a
		// destructive subcommand. `firewall <server>` with sub="" (bare) is not
		// destructive even though args[0] could be any string.
		{"firewall", "", []string{"enable"}, false},
		{"server", "", []string{"remove"}, false},
	}
	for _, c := range cases {
		if got := IsDestructiveCommand(c.top, c.sub, c.args); got != c.want {
			t.Errorf("IsDestructiveCommand(%q, %q, %v) = %v, want %v", c.top, c.sub, c.args, got, c.want)
		}
	}
}

func TestAuthorizeAllowDenyByCommand(t *testing.T) {
	// Deny-list: command present in DenyCommands is rejected.
	tok := Token{Name: "t", DenyCommands: []string{"server"}, DestructiveAllowed: true}
	if err := Authorize(tok, "server", "", false, nil, nil); err == nil {
		t.Fatal("expected deny for command in DenyCommands")
	} else if !strings.Contains(err.Error(), "denied") {
		t.Fatalf("error = %q, want 'denied: ...'", err)
	}
	// A command not on the deny-list is allowed.
	if err := Authorize(tok, "status", "", false, nil, nil); err != nil {
		t.Fatalf("status should be allowed: %v", err)
	}

	// Allow-list: only listed commands pass.
	allowTok := Token{Name: "t", AllowCommands: []string{"exec"}}
	if err := Authorize(allowTok, "exec", "", false, nil, nil); err != nil {
		t.Fatalf("exec should be allowed: %v", err)
	}
	if err := Authorize(allowTok, "file", "", false, nil, nil); err == nil {
		t.Fatal("file should be denied (not in allow-list)")
	}

	// Deny wins over allow when both list the same command.
	bothTok := Token{Name: "t", AllowCommands: []string{"exec"}, DenyCommands: []string{"exec"}}
	if err := Authorize(bothTok, "exec", "", false, nil, nil); err == nil {
		t.Fatal("deny should win over allow")
	}
}

func TestAuthorizeServerScope(t *testing.T) {
	tok := Token{Name: "t", Servers: []string{"web-01", "web-02"}}

	// In-scope server is allowed.
	if err := Authorize(tok, "exec", "web-01", false, nil, nil); err != nil {
		t.Fatalf("web-01 should be in scope: %v", err)
	}
	// Out-of-scope server is denied.
	if err := Authorize(tok, "exec", "db-01", false, nil, nil); err == nil {
		t.Fatal("db-01 should be out of scope")
	}
	// No target server -> server scope not enforced.
	if err := Authorize(tok, "status", "", false, nil, nil); err != nil {
		t.Fatalf("no-target command should pass: %v", err)
	}
	// Unscoped token (no Servers/Groups) targets any server.
	open := Token{Name: "open"}
	if err := Authorize(open, "exec", "anything", false, nil, nil); err != nil {
		t.Fatalf("unscoped token should allow any server: %v", err)
	}
}

func TestAuthorizeGroupScope(t *testing.T) {
	dir := t.TempDir()
	tags := NewTagStore(dir)
	if err := tags.SetTags("web-01", map[string]string{"role": "web"}); err != nil {
		t.Fatalf("SetTags: %v", err)
	}
	if err := tags.SetTags("web-02", map[string]string{"role": "web"}); err != nil {
		t.Fatalf("SetTags: %v", err)
	}
	if err := tags.SetTags("db-01", map[string]string{"role": "db"}); err != nil {
		t.Fatalf("SetTags: %v", err)
	}
	all := []string{"web-01", "web-02", "db-01"}

	tok := Token{Name: "t", Groups: []string{"role=web"}}

	if err := Authorize(tok, "exec", "web-01", false, all, tags); err != nil {
		t.Fatalf("web-01 matches role=web: %v", err)
	}
	if err := Authorize(tok, "exec", "db-01", false, all, tags); err == nil {
		t.Fatal("db-01 does not match role=web; should be denied")
	}

	// Group + explicit server union.
	tok2 := Token{Name: "t", Groups: []string{"role=web"}, Servers: []string{"db-01"}}
	if err := Authorize(tok2, "exec", "db-01", false, all, tags); err != nil {
		t.Fatalf("db-01 is explicitly listed: %v", err)
	}
}

func TestAuthorizeDestructive(t *testing.T) {
	// Destructive op denied when DestructiveAllowed is false.
	tok := Token{Name: "t"}
	if err := Authorize(tok, "server", "web-01", true, nil, nil); err == nil {
		t.Fatal("destructive op should be denied without DestructiveAllowed")
	}
	// Allowed when DestructiveAllowed is true (and server in scope / unscoped).
	allow := Token{Name: "t", DestructiveAllowed: true}
	if err := Authorize(allow, "server", "web-01", true, nil, nil); err != nil {
		t.Fatalf("destructive op should be allowed: %v", err)
	}
	// Non-destructive op is unaffected by DestructiveAllowed=false.
	if err := Authorize(tok, "exec", "web-01", false, nil, nil); err != nil {
		t.Fatalf("non-destructive op should pass: %v", err)
	}
}

func TestIsScoped(t *testing.T) {
	cases := []struct {
		name string
		tok  Token
		want bool
	}{
		{"no constraints (admin-equivalent)", Token{Name: "admin"}, false},
		{"servers", Token{Servers: []string{"web-01"}}, true},
		{"groups", Token{Groups: []string{"role=web"}}, true},
		{"allow-list", Token{AllowCommands: []string{"exec"}}, true},
		{"deny-list", Token{DenyCommands: []string{"server"}}, true},
		{"read-only", Token{ReadOnlyDefault: true}, true},
		{"destructive-allowed", Token{DestructiveAllowed: true}, true},
	}
	for _, c := range cases {
		if got := c.tok.IsScoped(); got != c.want {
			t.Errorf("%s: IsScoped() = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestTagSetNotReadOnly guards the regression where `tag set` (a WRITE) slipped
// past a ReadOnlyDefault token because `tag` was in readCommands. `tag` must NOT
// be a read command, and `tag set` must additionally be destructive.
func TestTagSetNotReadOnly(t *testing.T) {
	if IsReadCommand("tag") {
		t.Fatal("tag must not be a read-only command (tag set is a WRITE)")
	}
	tok := Token{Name: "ro", ReadOnlyDefault: true}
	if err := Authorize(tok, "tag", "", false, nil, nil); err == nil {
		t.Fatal("read-only token must be denied 'tag' (no longer a read command)")
	}
	// And classified destructive so even a non-read-only token without
	// DestructiveAllowed is blocked.
	if !IsDestructiveCommand("tag", "set", []string{"web-01", "role=web"}) {
		t.Fatal("tag set must be destructive")
	}
}

func TestAuthorizeReadOnlyDefault(t *testing.T) {
	tok := Token{Name: "t", ReadOnlyDefault: true}

	// A read command passes.
	if err := Authorize(tok, "status", "", false, nil, nil); err != nil {
		t.Fatalf("read command should pass under read-only default: %v", err)
	}
	// A non-read command is denied.
	if err := Authorize(tok, "exec", "web-01", false, nil, nil); err == nil {
		t.Fatal("exec should be denied under read-only default")
	}
	// ... unless explicitly allowed.
	tokAllow := Token{Name: "t", ReadOnlyDefault: true, AllowCommands: []string{"exec"}}
	if err := Authorize(tokAllow, "exec", "web-01", false, nil, nil); err != nil {
		t.Fatalf("explicitly-allowed exec should pass under read-only default: %v", err)
	}
}
