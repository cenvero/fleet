// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidAutomationName(t *testing.T) {
	bad := []string{
		// path separators / traversal
		"", "a/b", "..", "../x", `a\b`, "x/../y",
		// leading '.' or '-' (hidden files, option-injection)
		".hidden", "-foo", "--version",
		// shell metacharacters — the name is interpolated unescaped into the
		// shell-init rc snippet, so any of these would inject into the rc file.
		"a;reboot", "$(id)", "a`id`b", "a b", "a&b", "a|b", "a>b",
		"a(b)", "a'b", `a"b`, "a$b", "a*b", "a\nb",
	}
	for _, n := range bad {
		if err := validAutomationName(n); err == nil {
			t.Errorf("expected %q to be rejected", n)
		}
	}
	for _, n := range []string{"default", "deploy", "my-script_1", "a.b.c", "A1_2-3"} {
		if err := validAutomationName(n); err != nil {
			t.Errorf("expected %q valid, got %v", n, err)
		}
	}
}

func TestAutomationPathConfined(t *testing.T) {
	store := filepath.Join("/cfg", "automations")
	got, err := automationPath("/cfg", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(store, "deploy.sh"); got != want {
		t.Fatalf("path=%q want %q", got, want)
	}
	if !strings.HasPrefix(got, store) {
		t.Fatalf("path escaped the store dir: %q", got)
	}
	if _, err := automationPath("/cfg", "../escape"); err == nil {
		t.Fatal("traversal name must be rejected")
	}
}

func TestShellInitSnippetEvalsAutomation(t *testing.T) {
	s := shellInitSnippet("deploy")
	if !strings.Contains(s, "fleet automation get deploy") {
		t.Fatalf("snippet should load the named automation: %s", s)
	}
	if !strings.Contains(s, shellInitMarker("deploy")) {
		t.Fatalf("snippet should carry an idempotency marker: %s", s)
	}
}

func TestCompletionLine(t *testing.T) {
	for _, sh := range []string{"bash", "zsh", "fish"} {
		if _, ok := completionLine(sh); !ok {
			t.Errorf("%s should be supported", sh)
		}
	}
	if _, ok := completionLine("nonsense"); ok {
		t.Error("unknown shell should be unsupported")
	}
}
