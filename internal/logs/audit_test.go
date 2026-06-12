// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package logs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAuditAppendChainsEntries verifies that consecutive appended entries form a
// hash chain: each entry's PrevHash equals the previous entry's Hash, every Hash
// is non-empty and self-consistent, and Verify reports the log intact.
func TestAuditAppendChainsEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "_audit.log")
	log := NewAuditLog(path)

	for i := 0; i < 4; i++ {
		if err := log.Append(AuditEntry{Action: "act", Target: "tgt", Operator: "op", Details: "d"}); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}

	entries, err := log.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4", len(entries))
	}

	prev := ""
	for i, e := range entries {
		if e.Hash == "" {
			t.Fatalf("entry %d has empty Hash", i)
		}
		if e.PrevHash != prev {
			t.Fatalf("entry %d PrevHash = %q, want %q", i, e.PrevHash, prev)
		}
		if got := entryDigest(e); got != e.Hash {
			t.Fatalf("entry %d Hash mismatch: digest=%q stored=%q", i, got, e.Hash)
		}
		prev = e.Hash
	}

	ok, idx, err := log.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok || idx != -1 {
		t.Fatalf("Verify on intact log = (ok=%v, idx=%d), want (true, -1)", ok, idx)
	}
}

// TestAuditVerifyDetectsTampering confirms editing, reordering, and deleting an
// entry all break the chain and are reported by Verify.
func TestAuditVerifyDetectsTampering(t *testing.T) {
	write := func(t *testing.T, entries []AuditEntry) *AuditLog {
		t.Helper()
		path := filepath.Join(t.TempDir(), "_audit.log")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close()
		for _, e := range entries {
			b, err := json.Marshal(e)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if _, err := f.Write(append(b, '\n')); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		return NewAuditLog(path)
	}

	// Build a valid 3-entry chain first via Append.
	base := NewAuditLog(filepath.Join(t.TempDir(), "_audit.log"))
	for _, a := range []string{"a", "b", "c"} {
		if err := base.Append(AuditEntry{Action: a, Operator: "op"}); err != nil {
			t.Fatalf("seed Append: %v", err)
		}
	}
	good, err := base.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	t.Run("edited-content", func(t *testing.T) {
		tampered := append([]AuditEntry(nil), good...)
		tampered[1].Details = "secretly-changed" // Hash no longer matches content
		log := write(t, tampered)
		ok, idx, err := log.Verify()
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if ok || idx != 1 {
			t.Fatalf("edited entry: Verify = (ok=%v, idx=%d), want (false, 1)", ok, idx)
		}
	})

	t.Run("reordered", func(t *testing.T) {
		tampered := []AuditEntry{good[0], good[2], good[1]}
		log := write(t, tampered)
		ok, idx, err := log.Verify()
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		// good[2].PrevHash links to good[1].Hash, but at position 1 the previous
		// entry is good[0], so the chain breaks at index 1.
		if ok || idx != 1 {
			t.Fatalf("reordered: Verify = (ok=%v, idx=%d), want (false, 1)", ok, idx)
		}
	})

	t.Run("deleted-middle", func(t *testing.T) {
		tampered := []AuditEntry{good[0], good[2]} // drop the middle entry
		log := write(t, tampered)
		ok, idx, err := log.Verify()
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if ok || idx != 1 {
			t.Fatalf("deleted middle: Verify = (ok=%v, idx=%d), want (false, 1)", ok, idx)
		}
	})
}

// TestAuditBackwardCompatLegacyLog verifies that a legacy log written before the
// hash chain (entries with no prev_hash/hash fields) still parses, Verify treats
// the wholly-legacy log as intact, and the chain begins cleanly at the next
// Append — linking to the (empty) last legacy hash and verifying from there.
func TestAuditBackwardCompatLegacyLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "_audit.log")

	// Hand-write two legacy entries with NO hash fields (the pre-chain format).
	legacy := []AuditEntry{
		{Timestamp: time.Now().UTC().Truncate(time.Second), Action: "old1", Operator: "op"},
		{Timestamp: time.Now().UTC().Truncate(time.Second), Action: "old2", Operator: "op"},
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, e := range legacy {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		// Sanity: a legacy line must NOT contain the chain fields.
		if got := string(b); contains(got, "hash") {
			t.Fatalf("legacy entry unexpectedly serialized a hash field: %s", got)
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	f.Close()

	log := NewAuditLog(path)

	// Legacy entries still parse.
	entries, err := log.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll legacy: %v", err)
	}
	if len(entries) != 2 || entries[0].Hash != "" || entries[1].Hash != "" {
		t.Fatalf("legacy entries should parse with empty Hash; got %+v", entries)
	}

	// A wholly-legacy log verifies as intact (tolerated prefix).
	if ok, idx, err := log.Verify(); err != nil || !ok || idx != -1 {
		t.Fatalf("legacy-only Verify = (ok=%v, idx=%d, err=%v), want (true, -1, nil)", ok, idx, err)
	}

	// The chain begins at the next Append.
	if err := log.Append(AuditEntry{Action: "new1", Operator: "op"}); err != nil {
		t.Fatalf("Append new1: %v", err)
	}
	if err := log.Append(AuditEntry{Action: "new2", Operator: "op"}); err != nil {
		t.Fatalf("Append new2: %v", err)
	}

	entries, err = log.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll after append: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("want 4 entries, got %d", len(entries))
	}
	// First new entry links to the last legacy entry's hash, which is "".
	if entries[2].Hash == "" {
		t.Fatal("first appended entry after legacy prefix must be hashed")
	}
	if entries[2].PrevHash != "" {
		t.Fatalf("first chained entry PrevHash = %q, want \"\" (legacy prefix)", entries[2].PrevHash)
	}
	if entries[3].PrevHash != entries[2].Hash {
		t.Fatalf("second chained entry PrevHash = %q, want %q", entries[3].PrevHash, entries[2].Hash)
	}

	// The mixed legacy+chained log verifies intact.
	if ok, idx, err := log.Verify(); err != nil || !ok || idx != -1 {
		t.Fatalf("mixed log Verify = (ok=%v, idx=%d, err=%v), want (true, -1, nil)", ok, idx, err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
