// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"testing"
	"time"
)

// newTestStore returns an ApprovalStore rooted in a temp dir with a controllable
// clock so expiry behavior is deterministic.
func newTestStore(t *testing.T, now *time.Time) *ApprovalStore {
	t.Helper()
	s := NewApprovalStore(t.TempDir())
	s.now = func() time.Time { return *now }
	return s
}

func TestStageAndGet(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	s := newTestStore(t, &now)

	id, err := s.Stage("web-01", "systemctl restart nginx", time.Hour)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if id == "" {
		t.Fatal("Stage returned empty id")
	}

	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Server != "web-01" || got.Command != "systemctl restart nginx" {
		t.Fatalf("unexpected approval: %+v", got)
	}
	if got.Status != ApprovalPending {
		t.Fatalf("status = %q, want pending", got.Status)
	}
	if !got.Expires.Equal(now.Add(time.Hour)) {
		t.Fatalf("expires = %v, want %v", got.Expires, now.Add(time.Hour))
	}
}

func TestStageValidation(t *testing.T) {
	now := time.Now()
	s := newTestStore(t, &now)
	if _, err := s.Stage("", "ls", time.Hour); err == nil {
		t.Fatal("expected error for empty server")
	}
	if _, err := s.Stage("web-01", "", time.Hour); err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestStageDefaultTTL(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	s := newTestStore(t, &now)
	id, err := s.Stage("web-01", "ls", 0)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Expires.Equal(now.Add(DefaultApprovalTTL)) {
		t.Fatalf("expires = %v, want default-ttl %v", got.Expires, now.Add(DefaultApprovalTTL))
	}
}

func TestApprove(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	s := newTestStore(t, &now)
	id, _ := s.Stage("web-01", "ls", time.Hour)

	approved, err := s.Approve(id)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.Status != ApprovalApproved {
		t.Fatalf("status = %q, want approved", approved.Status)
	}

	// Re-approving a non-pending approval must fail.
	if _, err := s.Approve(id); err == nil {
		t.Fatal("expected error approving an already-approved request")
	}
	// Rejecting a decided approval must also fail.
	if _, err := s.Reject(id); err == nil {
		t.Fatal("expected error rejecting an already-approved request")
	}
}

func TestReject(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	s := newTestStore(t, &now)
	id, _ := s.Stage("web-01", "ls", time.Hour)

	rejected, err := s.Reject(id)
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if rejected.Status != ApprovalRejected {
		t.Fatalf("status = %q, want rejected", rejected.Status)
	}
}

func TestDecideUnknownID(t *testing.T) {
	now := time.Now()
	s := newTestStore(t, &now)
	if _, err := s.Approve("does-not-exist"); err == nil {
		t.Fatal("expected error for unknown id")
	}
	if _, err := s.Get("does-not-exist"); err == nil {
		t.Fatal("expected error for unknown id in Get")
	}
}

func TestExpiry(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	s := newTestStore(t, &now)
	id, _ := s.Stage("web-01", "ls", time.Minute)

	// Advance past expiry.
	now = now.Add(2 * time.Minute)

	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != ApprovalExpired {
		t.Fatalf("status = %q, want expired", got.Status)
	}

	// An expired approval can no longer be approved.
	if _, err := s.Approve(id); err == nil {
		t.Fatal("expected error approving an expired request")
	}
}

func TestPruneExpired(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	s := newTestStore(t, &now)
	expiringID, _ := s.Stage("a", "ls", time.Minute)
	liveID, _ := s.Stage("b", "ls", time.Hour)

	now = now.Add(2 * time.Minute)
	count, err := s.PruneExpired()
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if count != 1 {
		t.Fatalf("pruned %d, want 1", count)
	}

	expired, _ := s.Get(expiringID)
	if expired.Status != ApprovalExpired {
		t.Fatalf("expiring approval status = %q, want expired", expired.Status)
	}
	live, _ := s.Get(liveID)
	if live.Status != ApprovalPending {
		t.Fatalf("live approval status = %q, want pending", live.Status)
	}

	// A second prune with nothing newly expired returns 0.
	if count, err := s.PruneExpired(); err != nil || count != 0 {
		t.Fatalf("second PruneExpired = (%d, %v), want (0, nil)", count, err)
	}
}

func TestListNewestFirst(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	s := newTestStore(t, &now)

	firstID, _ := s.Stage("a", "first", time.Hour)
	now = now.Add(time.Second)
	secondID, _ := s.Stage("b", "second", time.Hour)

	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
	if list[0].ID != secondID || list[1].ID != firstID {
		t.Fatalf("List not newest-first: got %s then %s", list[0].ID, list[1].ID)
	}
}

func TestPersistenceAcrossStores(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	s1 := NewApprovalStore(dir)
	s1.now = func() time.Time { return now }
	id, err := s1.Stage("web-01", "ls", time.Hour)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	// A fresh store over the same dir must see the persisted approval.
	s2 := NewApprovalStore(dir)
	s2.now = func() time.Time { return now }
	got, err := s2.Get(id)
	if err != nil {
		t.Fatalf("Get from second store: %v", err)
	}
	if got.ID != id {
		t.Fatalf("id = %q, want %q", got.ID, id)
	}
}

func TestListEmpty(t *testing.T) {
	now := time.Now()
	s := newTestStore(t, &now)
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("len = %d, want 0", len(list))
	}
}
