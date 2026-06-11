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
	cases := []struct {
		top  string
		args []string
		want bool
	}{
		{"server", []string{"remove", "web-01"}, true},
		{"server", []string{"list"}, false},
		{"file", []string{"rm", "web-01", "/tmp/x"}, true},
		{"file", []string{"list", "web-01"}, false},
		{"key", []string{"rotate"}, true},
		{"key", []string{"fingerprint"}, false},
		{"firewall", []string{"enable", "web-01"}, true},
		{"firewall", []string{"status", "web-01"}, false},
		{"fw", []string{"enable", "web-01"}, true},
		{"guard", []string{"web-01", "rm -rf /"}, true}, // guard is destructive (any sub)
		{"revert", []string{"abc123"}, true},            // revert is destructive (any sub)
		{"drift", []string{"web-01"}, false},            // drift is read-only, NOT destructive
		{"exec", []string{"web-01", "uptime"}, false},   // exec is non-destructive by default
		{"status", nil, false},
	}
	for _, c := range cases {
		if got := IsDestructiveCommand(c.top, c.args); got != c.want {
			t.Errorf("IsDestructiveCommand(%q, %v) = %v, want %v", c.top, c.args, got, c.want)
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
