// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package logs

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeRawAudit(t testing.TB, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func legacyLine(t testing.TB, action string) string {
	t.Helper()
	b, err := json.Marshal(AuditEntry{Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Action: action, Operator: "op"})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAuditTailMatchesReadAll checks Tail(n) returns exactly the last n entries
// ReadAll returns, oldest first, including when lines straddle the backwards
// reader's chunk boundaries and when n exceeds the number of entries.
func TestAuditTailMatchesReadAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "_audit.log")
	log := NewAuditLog(path)
	for i := 0; i < 40; i++ {
		// Vary line lengths so some lines cross the 64 KiB chunk boundary.
		details := strings.Repeat("x", (i*7919)%(3*auditTailChunk/2))
		if err := log.Append(AuditEntry{Action: fmt.Sprintf("a%d", i), Target: "t", Operator: "o", Details: details}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	all, err := log.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{1, 2, 5, 39, 40, 41, 1000} {
		got, err := log.Tail(n)
		if err != nil {
			t.Fatalf("Tail(%d): %v", n, err)
		}
		want := all
		if n < len(all) {
			want = all[len(all)-n:]
		}
		if len(got) != len(want) {
			t.Fatalf("Tail(%d) returned %d entries, want %d", n, len(got), len(want))
		}
		for i := range want {
			if got[i].Action != want[i].Action || got[i].Hash != want[i].Hash {
				t.Fatalf("Tail(%d)[%d] = %s/%s, want %s/%s", n, i, got[i].Action, got[i].Hash, want[i].Action, want[i].Hash)
			}
		}
	}
	if got, err := log.Tail(0); err != nil || got != nil {
		t.Fatalf("Tail(0) = %v, %v; want nil, nil", got, err)
	}
	if got, err := NewAuditLog(filepath.Join(t.TempDir(), "missing.log")).Tail(3); err != nil || got != nil {
		t.Fatalf("Tail on missing log = %v, %v; want nil, nil", got, err)
	}
}

// TestAuditTailScannerSemantics pins that the backwards reader agrees with the
// bufio.Scanner used by ReadAll on CRLF endings and a final unterminated line.
func TestAuditTailScannerSemantics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "_audit.log")
	a, b, c := legacyLine(t, "one"), legacyLine(t, "two"), legacyLine(t, "three")
	writeRawAudit(t, path, a+"\r\n"+b+"\n"+c) // CRLF line + unterminated last line
	log := NewAuditLog(path)
	all, err := log.ReadAll()
	if err != nil || len(all) != 3 {
		t.Fatalf("ReadAll = %d entries, %v", len(all), err)
	}
	got, err := log.Tail(3)
	if err != nil {
		t.Fatal(err)
	}
	for i := range all {
		if got[i].Action != all[i].Action {
			t.Fatalf("Tail[%d] = %q, want %q", i, got[i].Action, all[i].Action)
		}
	}

	// An empty trailing line is an undecodable entry for both readers.
	writeRawAudit(t, path, a+"\n\n")
	if _, err := log.ReadAll(); err == nil {
		t.Fatal("ReadAll accepted an empty line")
	}
	if _, err := log.Tail(1); err == nil {
		t.Fatal("Tail accepted an empty line")
	}
}

// TestAuditAppendAfterLegacyLastEntry: when the newest entry predates the chain
// (legacy, no hash), the next entry starts the chain with PrevHash "".
func TestAuditAppendAfterLegacyLastEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "_audit.log")
	log := NewAuditLog(path)
	if err := log.Append(AuditEntry{Action: "chained", Operator: "op"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(legacyLine(t, "legacy") + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := log.Append(AuditEntry{Action: "after", Operator: "op"}); err != nil {
		t.Fatal(err)
	}
	entries, err := log.ReadAll()
	if err != nil || len(entries) != 3 {
		t.Fatalf("ReadAll = %d, %v", len(entries), err)
	}
	if entries[2].PrevHash != "" {
		t.Fatalf("entry after a legacy last entry must link to \"\", got %q", entries[2].PrevHash)
	}
}

// TestAuditAppendRejectsTornLastLine: a torn (undecodable) last line still makes
// Append fail without touching the file, as the full-file reader did.
func TestAuditAppendRejectsTornLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "_audit.log")
	content := legacyLine(t, "ok") + "\n" + `{"timestamp":"2026-01-02T03:0`
	writeRawAudit(t, path, content)
	log := NewAuditLog(path)
	if err := log.Append(AuditEntry{Action: "x", Operator: "op"}); err == nil || !strings.Contains(err.Error(), "decode audit entry") {
		t.Fatalf("Append over a torn last line: err = %v, want decode error", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content {
		t.Fatalf("Append modified the log after failing:\n%q", data)
	}
}

// TestAuditAppendTerminatesUnterminatedLastLine: a complete last entry without a
// trailing newline gets one, so the new entry is not glued onto it.
func TestAuditAppendTerminatesUnterminatedLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "_audit.log")
	log := NewAuditLog(path)
	if err := log.Append(AuditEntry{Action: "first", Operator: "op"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeRawAudit(t, path, strings.TrimSuffix(string(data), "\n"))
	log = NewAuditLog(path)
	if err := log.Append(AuditEntry{Action: "second", Operator: "op"}); err != nil {
		t.Fatal(err)
	}
	entries, err := log.ReadAll()
	if err != nil || len(entries) != 2 {
		t.Fatalf("ReadAll = %d entries, %v", len(entries), err)
	}
	if ok, idx, err := log.Verify(); err != nil || !ok {
		t.Fatalf("Verify = %v, %d, %v", ok, idx, err)
	}
}

// TestAuditAppendLineTooLong: a last line beyond the scanner limit fails the
// same way ReadAll does.
func TestAuditAppendLineTooLong(t *testing.T) {
	path := filepath.Join(t.TempDir(), "_audit.log")
	writeRawAudit(t, path, `{"action":"`+strings.Repeat("y", maxAuditLineBytes+10)+`"}`+"\n")
	log := NewAuditLog(path)
	if err := log.Append(AuditEntry{Action: "x"}); !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("Append err = %v, want ErrTooLong", err)
	}
	if _, err := log.ReadAll(); !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("ReadAll err = %v, want ErrTooLong", err)
	}
}

// TestAuditAppendSeesOtherWriters: two AuditLog instances on one file (as two
// processes would have) interleave appends; the in-memory tail cache must never
// hide the other writer's entry, so the chain stays intact.
func TestAuditAppendSeesOtherWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "_audit.log")
	a, b := NewAuditLog(path), NewAuditLog(path)
	for i := 0; i < 10; i++ {
		w := a
		if i%3 == 1 {
			w = b
		}
		if err := w.Append(AuditEntry{Action: fmt.Sprintf("e%d", i), Operator: "op"}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, idx, err := a.Verify(); err != nil || !ok {
		t.Fatalf("Verify = %v, %d, %v", ok, idx, err)
	}
}

// TestAuditAppendConcurrentInstances hammers one file from several instances at
// once; without the cross-process lock two writers could read the same last hash
// and fork the chain.
func TestAuditAppendConcurrentInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "_audit.log")
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			log := NewAuditLog(path)
			for i := 0; i < 40; i++ {
				if err := log.Append(AuditEntry{Action: fmt.Sprintf("w%d-%d", w, i), Operator: "op"}); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	log := NewAuditLog(path)
	entries, err := log.ReadAll()
	if err != nil || len(entries) != 240 {
		t.Fatalf("ReadAll = %d entries, %v; want 240", len(entries), err)
	}
	if ok, idx, err := log.Verify(); err != nil || !ok {
		t.Fatalf("Verify = %v, %d, %v", ok, idx, err)
	}
}

const auditChildEnv = "FLEET_AUDIT_TEST_CHILD"

// TestAuditAppendChildProcess is the body run by re-executed test binaries in
// TestAuditAppendConcurrentProcesses; it is a no-op in a normal test run.
func TestAuditAppendChildProcess(t *testing.T) {
	spec := os.Getenv(auditChildEnv)
	if spec == "" {
		t.Skip("helper for TestAuditAppendConcurrentProcesses")
	}
	parts := strings.SplitN(spec, "|", 3)
	count, _ := strconv.Atoi(parts[1])
	log := NewAuditLog(parts[0])
	for i := 0; i < count; i++ {
		if err := log.Append(AuditEntry{Action: parts[2], Target: strconv.Itoa(i), Operator: "op"}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAuditAppendConcurrentProcesses appends from several real OS processes at
// once and checks the chain is a single unbroken line.
func TestAuditAppendConcurrentProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	path := filepath.Join(t.TempDir(), "_audit.log")
	const procs, each = 4, 60
	cmds := make([]*exec.Cmd, procs)
	for p := range cmds {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAuditAppendChildProcess$", "-test.count=1") // #nosec G204 -- re-executes this test binary
		cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%s|%d|p%d", auditChildEnv, path, each, p))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[p] = cmd
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child append process failed: %v", err)
		}
	}
	log := NewAuditLog(path)
	entries, err := log.ReadAll()
	if err != nil || len(entries) != procs*each {
		t.Fatalf("ReadAll = %d entries, %v; want %d", len(entries), err, procs*each)
	}
	if ok, idx, err := log.Verify(); err != nil || !ok {
		t.Fatalf("Verify after concurrent processes = %v, %d, %v", ok, idx, err)
	}
}

// BenchmarkAuditAppend measures one Append against an existing log; its cost
// must not grow with the log size.
func BenchmarkAuditAppend(b *testing.B) {
	for _, existing := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("existing=%d", existing), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "_audit.log")
			var sb strings.Builder
			for i := 0; i < existing; i++ {
				fmt.Fprintf(&sb, `{"timestamp":"2026-09-01T00:00:00Z","action":"metrics.collect","target":"node-%03d","operator":"root","details":"cpu=12.0 memory=40.0 disk=50.0","prev_hash":"%s","hash":"%s"}`+"\n", i%500, strings.Repeat("a", 64), strings.Repeat("b", 64))
			}
			writeRawAudit(b, path, sb.String())
			log := NewAuditLog(path)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := log.Append(AuditEntry{Action: "x", Target: "y", Operator: "z"}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkAuditTail reads the last 50 entries of a large log.
func BenchmarkAuditTail(b *testing.B) {
	path := filepath.Join(b.TempDir(), "_audit.log")
	var sb strings.Builder
	for i := 0; i < 100000; i++ {
		fmt.Fprintf(&sb, `{"timestamp":"2026-09-01T00:00:00Z","action":"metrics.collect","target":"node-%03d","operator":"root","hash":"%s"}`+"\n", i%500, strings.Repeat("b", 64))
	}
	writeRawAudit(b, path, sb.String())
	log := NewAuditLog(path)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := log.Tail(50); err != nil {
			b.Fatal(err)
		}
	}
}
