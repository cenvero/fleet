// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCmdPolicySetAndPersist(t *testing.T) {
	dir := t.TempDir()
	store, err := NewCmdPolicyStore(dir)
	if err != nil {
		t.Fatalf("NewCmdPolicyStore: %v", err)
	}

	if err := store.SetDenyPatterns([]string{"rm -rf /", "mkfs", ""}); err != nil {
		t.Fatalf("SetDenyPatterns: %v", err)
	}
	if err := store.SetConfirmPatterns([]string{"reboot", "shutdown"}); err != nil {
		t.Fatalf("SetConfirmPatterns: %v", err)
	}

	// Empty entries are dropped.
	if got, want := store.DenyPatterns(), []string{"rm -rf /", "mkfs"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DenyPatterns = %v, want %v", got, want)
	}
	if got, want := store.ConfirmPatterns(), []string{"reboot", "shutdown"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ConfirmPatterns = %v, want %v", got, want)
	}

	// File must exist, be 0600, and live in cmd-policy.json (NOT policy.json).
	info, err := os.Stat(CmdPolicyPath(dir))
	if err != nil {
		t.Fatalf("stat cmd-policy.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("cmd-policy.json perm = %o, want 600", perm)
	}
	if filepath.Base(CmdPolicyPath(dir)) != "cmd-policy.json" {
		t.Fatalf("unexpected cmd policy path %s", CmdPolicyPath(dir))
	}
	// Crucially it must be a different file from the redaction policy.
	if CmdPolicyPath(dir) == PolicyPath(dir) {
		t.Fatalf("cmd policy path must differ from redaction policy.json path")
	}

	// A fresh store reloads the same patterns from disk.
	reloaded, err := NewCmdPolicyStore(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, want := reloaded.DenyPatterns(), []string{"rm -rf /", "mkfs"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reloaded DenyPatterns = %v, want %v", got, want)
	}
	if got, want := reloaded.ConfirmPatterns(), []string{"reboot", "shutdown"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reloaded ConfirmPatterns = %v, want %v", got, want)
	}
}

func TestCmdPolicySettingOneListKeepsOther(t *testing.T) {
	dir := t.TempDir()
	store, err := NewCmdPolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDenyPatterns([]string{"mkfs"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetConfirmPatterns([]string{"reboot"}); err != nil {
		t.Fatal(err)
	}
	// Setting deny again must not wipe the confirm list, and vice versa.
	if err := store.SetDenyPatterns([]string{"dd of=/dev/sd*"}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewCmdPolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reloaded.ConfirmPatterns(), []string{"reboot"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("confirm list clobbered: got %v, want %v", got, want)
	}
	if got, want := reloaded.DenyPatterns(), []string{"dd of=/dev/sd*"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("deny list = %v, want %v", got, want)
	}
}

func TestCmdPolicyMatchDeny(t *testing.T) {
	dir := t.TempDir()
	store, err := NewCmdPolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDenyPatterns([]string{"rm -rf /", "mkfs", "dd of=/dev/sd*"}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		command     string
		wantMatch   bool
		wantPattern string
	}{
		// substring matches
		{"sudo rm -rf / --no-preserve-root", true, "rm -rf /"},
		{"mkfs.ext4 /dev/sdb1", true, "mkfs"},
		// glob matches whole command
		{"dd of=/dev/sda", true, "dd of=/dev/sd*"},
		{"dd of=/dev/sdb1 bs=1M", true, "dd of=/dev/sd*"},
		// non-matches
		{"ls -la", false, ""},
		{"rm -rf /tmp/cache", true, "rm -rf /"}, // contains "rm -rf /"
		{"dd of=/dev/null", false, ""},          // glob requires /dev/sd...
	}
	for _, tc := range cases {
		gotMatch, gotPattern := store.MatchDeny(tc.command)
		if gotMatch != tc.wantMatch || gotPattern != tc.wantPattern {
			t.Fatalf("MatchDeny(%q) = (%v, %q), want (%v, %q)",
				tc.command, gotMatch, gotPattern, tc.wantMatch, tc.wantPattern)
		}
	}
}

func TestCmdPolicyMatchConfirm(t *testing.T) {
	dir := t.TempDir()
	store, err := NewCmdPolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetConfirmPatterns([]string{"reboot", "systemctl restart *"}); err != nil {
		t.Fatal(err)
	}
	if match, p := store.MatchConfirm("reboot now"); !match || p != "reboot" {
		t.Fatalf("MatchConfirm(reboot now) = (%v, %q)", match, p)
	}
	if match, p := store.MatchConfirm("systemctl restart nginx"); !match || p != "systemctl restart *" {
		t.Fatalf("MatchConfirm glob = (%v, %q)", match, p)
	}
	if match, _ := store.MatchConfirm("uptime"); match {
		t.Fatalf("MatchConfirm(uptime) should not match")
	}
}

func TestCmdPolicyEmptyStoreMatchesNothing(t *testing.T) {
	store, err := NewCmdPolicyStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if match, _ := store.MatchDeny("rm -rf /"); match {
		t.Fatal("empty deny list should match nothing")
	}
	if match, _ := store.MatchConfirm("reboot"); match {
		t.Fatal("empty confirm list should match nothing")
	}
}

func TestCmdPolicyClearPatterns(t *testing.T) {
	dir := t.TempDir()
	store, err := NewCmdPolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDenyPatterns([]string{"mkfs"}); err != nil {
		t.Fatal(err)
	}
	// Passing an all-empty slice clears the list.
	if err := store.SetDenyPatterns([]string{"", "  "}); err != nil {
		t.Fatal(err)
	}
	if got := store.DenyPatterns(); len(got) != 0 {
		t.Fatalf("expected empty deny list after clear, got %v", got)
	}
}

func TestCmdPolicyCorruptFileIsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(CmdPolicyPath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCmdPolicyStore(dir); err == nil {
		t.Fatal("expected error loading corrupt cmd-policy.json")
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"dd of=/dev/sd*", "dd of=/dev/sda", true},
		{"dd of=/dev/sd*", "dd of=/dev/sdb1", true},
		{"dd of=/dev/sd*", "dd of=/dev/nvme0", false},
		{"*", "anything", true},
		{"*", "", true},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"a*c", "abbbc", true},
		{"a*c", "abbb", false},
		{"reboot", "reboot", true},
		{"reboot", "reboots", false}, // whole-string glob (no '*')... but no meta -> not via globMatch
	}
	for _, tc := range cases {
		// Skip the last case here: it has no meta, exercised by matchPattern instead.
		if !hasGlobMeta(tc.pattern) {
			continue
		}
		if got := globMatch(tc.pattern, tc.name); got != tc.want {
			t.Fatalf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestMatchPatternSubstringVsGlob(t *testing.T) {
	// No meta -> substring (matches anywhere).
	if !matchPattern("please reboot the box", "reboot") {
		t.Fatal("expected substring match")
	}
	// Glob -> UNANCHORED substring-glob: it fires wherever the fragment occurs,
	// not only when it spans the whole command. This is what keeps a deny list
	// from failing open.
	if !matchPattern("please reboot the box", "reboot*the") {
		t.Fatal("glob should match a fragment anywhere in the command")
	}
	if !matchPattern("reboot the box", "reboot*box") {
		t.Fatal("glob should match the command")
	}
	if !matchPattern("cd /tmp && reboot --force now", "reboot*--force") {
		t.Fatal("glob should match a fragment in the middle of a chained command")
	}
	// A glob whose fragment is absent must still NOT match.
	if matchPattern("ls -la /tmp", "reboot*now") {
		t.Fatal("glob should not match when its fragment is absent")
	}
}

// TestCmdPolicyDenyGlobUnanchored is the regression guard for the audit finding:
// a deny glob like "rm -rf /*" must block any command that contains "rm -rf /...",
// including when it is buried in a chained command. Anchoring the glob to the
// whole command line previously let "cd /tmp && rm -rf /etc" slip through.
func TestCmdPolicyDenyGlobUnanchored(t *testing.T) {
	dir := t.TempDir()
	store, err := NewCmdPolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDenyPatterns([]string{"rm -rf /*"}); err != nil {
		t.Fatal(err)
	}
	blocked := []string{
		"rm -rf /",
		"rm -rf /etc",
		"sudo rm -rf /var/lib/foo",
		"cd /tmp && rm -rf /",
		"cd /tmp && rm -rf /etc/passwd",
		"true; rm -rf /; echo done",
	}
	for _, cmd := range blocked {
		if match, p := store.MatchDeny(cmd); !match || p != "rm -rf /*" {
			t.Fatalf("MatchDeny(%q) = (%v, %q), want blocked by %q", cmd, match, p, "rm -rf /*")
		}
	}
	allowed := []string{
		"rm -rf ./build", // relative path, not "rm -rf /"
		"ls -la /",
		"rm -i file",
	}
	for _, cmd := range allowed {
		if match, _ := store.MatchDeny(cmd); match {
			t.Fatalf("MatchDeny(%q) should not be blocked by %q", cmd, "rm -rf /*")
		}
	}
}
