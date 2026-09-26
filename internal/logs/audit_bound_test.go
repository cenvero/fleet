// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package logs

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestAuditAppendBoundsOversizedEntry: an entry whose text is larger than the
// log's line limit — an agent-supplied error message embedded in Details, say —
// must not produce a line the log can no longer read. Before the bound, one
// such entry made every later Append (and ReadAll/Verify/Tail) fail with
// bufio.ErrTooLong, silently ending the audit trail.
func TestAuditAppendBoundsOversizedEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "_audit.log")
	log := NewAuditLog(path)

	// "<" is JSON-escaped to six bytes (<), so even a modest field can
	// expand past the limit once marshalled.
	huge := strings.Repeat("<", maxAuditLineBytes)
	if err := log.Append(AuditEntry{Action: "metrics.replay.failed", Target: "web-01", Operator: "system", Details: "x: " + huge}); err != nil {
		t.Fatalf("Append oversized: %v", err)
	}
	if err := log.Append(AuditEntry{Action: "after", Target: "web-01", Operator: "op", Details: "still recorded"}); err != nil {
		t.Fatalf("Append after oversized entry: %v", err)
	}
	entries, err := log.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(entries) != 2 || entries[1].Action != "after" {
		t.Fatalf("entries = %d, want the oversized entry and the one after it", len(entries))
	}
	if !strings.HasPrefix(entries[0].Details, "x: <<<") || !strings.Contains(entries[0].Details, "truncated") {
		t.Fatalf("oversized Details not kept as a truncated prefix: %q...", entries[0].Details[:min(len(entries[0].Details), 40)])
	}
	if ok, idx, err := log.Verify(); err != nil || !ok {
		t.Fatalf("Verify = (%v, %d, %v), want an intact chain", ok, idx, err)
	}
	if tail, err := log.Tail(1); err != nil || len(tail) != 1 || tail[0].Action != "after" {
		t.Fatalf("Tail(1) = %v, %v", tail, err)
	}
}

// TestBoundAuditFieldKeepsUTF8: truncation never splits a multi-byte rune.
func TestBoundAuditFieldKeepsUTF8(t *testing.T) {
	in := strings.Repeat("é", 10) // 2 bytes each
	out := boundAuditField(in, 5)
	if !utf8.ValidString(out) {
		t.Fatalf("boundAuditField produced invalid UTF-8: %q", out)
	}
	if !strings.HasPrefix(out, "éé") || strings.HasPrefix(out, "ééé") {
		t.Fatalf("boundAuditField(%q, 5) = %q, want a two-rune prefix", in, out)
	}
	if got := boundAuditField("short", 5); got != "short" {
		t.Fatalf("boundAuditField left alone = %q", got)
	}
}
