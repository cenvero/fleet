// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
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

type failingApprovalEntropy struct {
	err error
}

func (r failingApprovalEntropy) Read([]byte) (int, error) {
	return 0, r.err
}

func TestStagePropagatesEntropyFailure(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	s := newTestStore(t, &now)
	injected := errors.New("injected entropy failure")
	s.entropy = failingApprovalEntropy{err: injected}

	id, err := s.Stage("web-01", "ls", time.Hour)
	if !errors.Is(err, injected) {
		t.Fatalf("Stage error = %v, want injected entropy failure", err)
	}
	if id != "" {
		t.Fatalf("Stage id = %q after entropy failure, want empty", id)
	}
	approvals, readErr := s.read()
	if readErr != nil {
		t.Fatalf("read after failed Stage: %v", readErr)
	}
	if len(approvals) != 0 {
		t.Fatalf("failed Stage persisted approvals: %#v", approvals)
	}
}

func TestConcurrentStoresDoNotLoseStagedApprovals(t *testing.T) {
	dir := t.TempDir()
	stores := []*ApprovalStore{NewApprovalStore(dir), NewApprovalStore(dir)}
	const stages = 40
	start := make(chan struct{})
	errs := make(chan error, stages)
	var wg sync.WaitGroup
	for i := 0; i < stages; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := stores[i%len(stores)].Stage("web-01", "echo staged", time.Hour)
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Stage: %v", err)
		}
	}

	approvals, err := stores[0].List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(approvals) != stages {
		t.Fatalf("staged approvals = %d, want %d", len(approvals), stages)
	}
}

func TestApprovalStoreProcessHelper(t *testing.T) {
	if os.Getenv("FLEET_APPROVAL_STORE_HELPER") != "1" {
		return
	}
	args := os.Args
	sep := -1
	for i, arg := range args {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || len(args) != sep+5 {
		t.Fatalf("bad approval helper args: %v", args)
	}
	dir, server, command, gate := args[sep+1], args[sep+2], args[sep+3], args[sep+4]
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(gate); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat process gate: %v", err)
		}
		if !time.Now().Before(deadline) {
			t.Fatal("timed out waiting for process gate")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := NewApprovalStore(dir).Stage(server, command, time.Hour); err != nil {
		t.Fatalf("Stage(%q): %v", server, err)
	}
}

func TestApprovalStoreCrossProcessReadModifyWrite(t *testing.T) {
	dir := t.TempDir()
	gate := filepath.Join(dir, "approval-start-gate")
	const workers = 16
	commands := make([]*exec.Cmd, 0, workers)
	for i := 0; i < workers; i++ {
		name := "worker-" + strconv.Itoa(i)
		cmd := exec.Command(os.Args[0], "-test.run=^TestApprovalStoreProcessHelper$", "--", dir, name, "echo "+name, gate)
		cmd.Env = append(os.Environ(), "FLEET_APPROVAL_STORE_HELPER=1")
		commands = append(commands, cmd)
	}
	for _, cmd := range commands {
		if err := cmd.Start(); err != nil {
			t.Fatalf("start approval helper: %v", err)
		}
	}
	if err := os.WriteFile(gate, []byte("go\n"), 0o600); err != nil {
		t.Fatalf("write process gate: %v", err)
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("approval helper failed: %v", err)
		}
	}

	approvals, err := NewApprovalStore(dir).List()
	if err != nil {
		t.Fatalf("List after cross-process mutations: %v", err)
	}
	if len(approvals) != workers {
		t.Fatalf("cross-process stages lost updates: got %d approvals, want %d", len(approvals), workers)
	}
	seen := make(map[string]bool, workers)
	for _, approval := range approvals {
		if seen[approval.Server] {
			t.Fatalf("duplicate approval for %q", approval.Server)
		}
		seen[approval.Server] = true
	}
	for i := 0; i < workers; i++ {
		name := "worker-" + strconv.Itoa(i)
		if !seen[name] {
			t.Errorf("missing cross-process approval for %q", name)
		}
	}
}

func TestApprovalStoreLockFailureFailsClosed(t *testing.T) {
	dir := t.TempDir()
	lockPath := ApprovalsPath(dir) + ".lock"
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatalf("plant unusable lock path: %v", err)
	}
	if _, err := NewApprovalStore(dir).Stage("web-01", "echo blocked", time.Hour); err == nil {
		t.Fatal("Stage succeeded without acquiring its advisory lock")
	}
	if _, err := os.Stat(ApprovalsPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("approval document exists after failed lock acquisition: %v", err)
	}
}
