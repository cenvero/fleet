// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"strings"
	"testing"
)

func TestNextGuardIDIncrementsAndIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	store := NewGuardStore(dir)

	id1, err := store.NextGuardID("web-01")
	if err != nil {
		t.Fatalf("NextGuardID: %v", err)
	}
	id2, err := store.NextGuardID("web-01")
	if err != nil {
		t.Fatalf("NextGuardID: %v", err)
	}
	if id1 != "web-01-1" || id2 != "web-01-2" {
		t.Fatalf("expected incrementing ids web-01-1, web-01-2; got %q, %q", id1, id2)
	}
	// A different server has its own counter starting at 1.
	other, err := store.NextGuardID("db-02")
	if err != nil {
		t.Fatalf("NextGuardID: %v", err)
	}
	if other != "db-02-1" {
		t.Fatalf("expected db-02-1, got %q", other)
	}
	// Ids must be charset-safe.
	for _, id := range []string{id1, id2, other} {
		if err := ValidateGuardID(id); err != nil {
			t.Errorf("generated id %q failed validation: %v", id, err)
		}
	}
}

func TestNextGuardIDPersistsCounter(t *testing.T) {
	dir := t.TempDir()
	if id, err := NewGuardStore(dir).NextGuardID("web-01"); err != nil || id != "web-01-1" {
		t.Fatalf("first id: %q err=%v", id, err)
	}
	// A fresh store over the same dir must continue the counter, not reset it.
	if id, err := NewGuardStore(dir).NextGuardID("web-01"); err != nil || id != "web-01-2" {
		t.Fatalf("second id from reopened store: %q err=%v", id, err)
	}
}

func TestGuardStorePutGetSetStatus(t *testing.T) {
	dir := t.TempDir()
	store := NewGuardStore(dir)
	rec := GuardRecord{
		ID:        "web-01-1",
		Server:    "web-01",
		Status:    GuardPending,
		RevertCmd: "ufw disable",
	}
	if err := store.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok := store.Get("web-01-1")
	if !ok {
		t.Fatal("expected to find stored guard")
	}
	if got.Server != "web-01" || got.RevertCmd != "ufw disable" || got.Status != GuardPending {
		t.Fatalf("unexpected record: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatal("expected Put to stamp timestamps")
	}

	if err := store.SetStatus("web-01-1", GuardConfirmed); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	got, _ = store.Get("web-01-1")
	if got.Status != GuardConfirmed {
		t.Fatalf("expected confirmed, got %q", got.Status)
	}

	if err := store.SetStatus("missing", GuardReverted); err == nil {
		t.Fatal("expected error setting status on unknown guard")
	}
}

func TestGuardStoreList(t *testing.T) {
	dir := t.TempDir()
	store := NewGuardStore(dir)
	_ = store.Put(GuardRecord{ID: "b-1", Server: "b", Status: GuardPending})
	_ = store.Put(GuardRecord{ID: "a-1", Server: "a", Status: GuardPending})
	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].ID != "a-1" || list[1].ID != "b-1" {
		t.Fatalf("expected sorted [a-1 b-1], got %+v", list)
	}
}

func TestValidateGuardID(t *testing.T) {
	for _, ok := range []string{"web-01-1", "a.b_c-2", "Server99-12"} {
		if err := ValidateGuardID(ok); err != nil {
			t.Errorf("expected %q valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "has space", "semi;colon", "slash/here", "dot..dot", strings.Repeat("x", 81)} {
		if err := ValidateGuardID(bad); err == nil {
			t.Errorf("expected %q invalid", bad)
		}
	}
}

func TestBuildGuardArmCommandQuotesEverything(t *testing.T) {
	cmd, err := BuildGuardArmCommand("web-01-1", "ufw enable", "ufw disable", 60)
	if err != nil {
		t.Fatalf("BuildGuardArmCommand: %v", err)
	}
	for _, want := range []string{
		"cat > '/run/fleet-guard-web-01-1.sh'",
		"chmod 700 '/run/fleet-guard-web-01-1.sh'",
		"rm -f '/run/fleet-guard-web-01-1.ok'",
		"sh -c 'ufw enable'",
		"systemd-run --on-active='60'",
		"--unit='fleet-guard-web-01-1'",
		"exit $__fleet_guard_rc",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("arm command missing %q\n---\n%s", want, cmd)
		}
	}
	// The heredoc that writes the revert script must be quoted to suppress
	// expansion of $, `, \ inside the body.
	if !strings.Contains(cmd, "<<'FLEET_GUARD_EOF'") {
		t.Errorf("expected quoted heredoc delimiter:\n%s", cmd)
	}
}

func TestBuildGuardArmCommandValidation(t *testing.T) {
	if _, err := BuildGuardArmCommand("web-01-1", "", "undo", 60); err == nil {
		t.Error("expected error for empty risky command")
	}
	if _, err := BuildGuardArmCommand("web-01-1", "x", "", 60); err == nil {
		t.Error("expected error for empty revert script")
	}
	if _, err := BuildGuardArmCommand("web-01-1", "x", "y", -1); err == nil {
		t.Error("expected error for negative delay")
	}
	if _, err := BuildGuardArmCommand("bad id", "x", "y", 60); err == nil {
		t.Error("expected error for invalid id")
	}
}

func TestDefaultRevertCommand(t *testing.T) {
	cmd := DefaultRevertCommand("web-01-1")
	if !strings.Contains(cmd, "web-01-1") {
		t.Errorf("expected default revert to mention the id:\n%s", cmd)
	}
	if !strings.Contains(cmd, "logger") {
		t.Errorf("expected default revert to log a warning:\n%s", cmd)
	}
}

func TestGuardIDSlugSanitizes(t *testing.T) {
	if got := guardIDSlug("web/../01"); strings.Contains(got, "/") || strings.Contains(got, "..") {
		t.Errorf("slug must not contain path separators or '..': %q", got)
	}
	if got := guardIDSlug(""); got != "server" {
		t.Errorf("expected fallback slug 'server', got %q", got)
	}
}
