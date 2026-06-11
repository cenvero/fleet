// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package alerts

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestDeleteRejectsUnsafeID confirms Delete validates the alert id the same way
// the other methods do, so a traversal id can't escape the alerts directory.
func TestDeleteRejectsUnsafeID(t *testing.T) {
	t.Parallel()
	s := NewStore(t.TempDir())

	// Plant a file outside the store dir that a naive join would target.
	outside := filepath.Join(t.TempDir(), "victim.json")
	if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed victim: %v", err)
	}

	bad := []string{"../victim", "../../etc/passwd", "a/b", "", strings.Repeat("x", 200)}
	for _, id := range bad {
		if err := s.Delete(id); err == nil {
			t.Fatalf("Delete(%q) = nil, want validation error", id)
		}
	}

	// The traversal target must still exist (Delete refused to act on it).
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("victim file was touched: %v", err)
	}
}

// TestDeleteRemovesValidAlert confirms a normal delete still works and is
// idempotent for a missing id.
func TestDeleteRemovesValidAlert(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Save(Alert{ID: "disk-full", Severity: SeverityWarning, Message: "x"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Delete("disk-full"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "disk-full.json")); !os.IsNotExist(err) {
		t.Fatalf("alert file still present: %v", err)
	}
	// Deleting a now-missing alert is a no-op, not an error.
	if err := s.Delete("disk-full"); err != nil {
		t.Fatalf("Delete(missing): %v", err)
	}
}

// TestSaveIsAtomic confirms Save writes via a temp file + rename and leaves no
// stray temp files behind, so a reader never sees a partial alert.
func TestSaveIsAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Save(Alert{ID: "cpu-hot", Severity: SeverityCritical, Message: "burning"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Get("cpu-hot")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Message != "burning" {
		t.Fatalf("unexpected message %q", got.Message)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("stray temp file left behind: %s", e.Name())
		}
	}
	if len(entries) != 1 || entries[0].Name() != "cpu-hot.json" {
		t.Fatalf("unexpected dir contents: %v", entries)
	}
}

// TestSaveConcurrent hammers Save on the same alert from many goroutines; the
// mutex + atomic write must leave a single, well-formed file with no lost-update
// crash or partial JSON.
func TestSaveConcurrent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewStore(dir)

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := Alert{ID: "flap", Severity: SeverityInfo, Message: "m"}
			a.Occurrences = i + 1
			errs <- s.Save(a)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Save: %v", err)
		}
	}

	// The file must be present and parse cleanly (no torn write).
	if _, err := s.Get("flap"); err != nil {
		t.Fatalf("Get after concurrent saves: %v", err)
	}
}
