// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"strings"
	"testing"
	"time"
)

// TestJobStorePrune verifies the pure pruning logic: finished jobs older than
// the cutoff are removed (and their remote logfiles rm-ed), while recent jobs
// and still-running jobs — even old ones — are kept.
func TestJobStorePrune(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewJobStore(dir)
	now := time.Now().UTC()
	doc := jobsDocument{
		Counter: 4,
		Jobs: []JobRecord{
			{ID: 1, Server: "a", Logfile: "/var/tmp/fleet-job-1-x.log", Status: JobDone, Finished: now.Add(-48 * time.Hour)},   // old done -> prune
			{ID: 2, Server: "b", Logfile: "/var/tmp/fleet-job-2-y.log", Status: JobDone, Finished: now.Add(-1 * time.Hour)},    // recent done -> keep
			{ID: 3, Server: "c", Logfile: "/var/tmp/fleet-job-3-z.log", Status: JobRunning, Started: now.Add(-72 * time.Hour)}, // old running -> keep
			{ID: 4, Server: "a", Logfile: "/var/tmp/fleet-job-4-w.log", Status: JobDone, Finished: now.Add(-24 * time.Hour)},   // old done -> prune
		},
	}
	if err := s.write(doc); err != nil {
		t.Fatal(err)
	}

	var rmCmds []string
	exec := func(server, command string) (string, int, error) {
		rmCmds = append(rmCmds, command)
		return "", 0, nil
	}
	n, err := s.Prune(now.Add(-12*time.Hour), exec)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("pruned %d, want 2 (jobs 1 and 4)", n)
	}
	if len(rmCmds) != 2 {
		t.Fatalf("remote rm calls = %d, want 2", len(rmCmds))
	}
	for _, c := range rmCmds {
		if !strings.HasPrefix(c, "rm -f ") {
			t.Fatalf("expected rm -f command, got %q", c)
		}
	}

	got, err := s.read()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Jobs) != 2 {
		t.Fatalf("remaining jobs = %d, want 2", len(got.Jobs))
	}
	kept := map[int]bool{}
	for _, j := range got.Jobs {
		kept[j.ID] = true
	}
	if !kept[2] || !kept[3] {
		t.Fatalf("expected jobs 2 (recent) and 3 (running) kept, got %v", kept)
	}
	if kept[1] || kept[4] {
		t.Fatalf("old done jobs should be gone, got %v", kept)
	}

	// Counter is preserved (ids stay monotonic).
	if got.Counter != 4 {
		t.Fatalf("counter = %d, want 4 preserved", got.Counter)
	}

	// Pruning again with the same cutoff is a no-op.
	n2, err := s.Prune(now.Add(-12*time.Hour), exec)
	if err != nil || n2 != 0 {
		t.Fatalf("second prune = %d (err %v), want 0", n2, err)
	}
}
