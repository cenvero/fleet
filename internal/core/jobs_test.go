// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBuildJobLaunch_QuotesAndDetaches(t *testing.T) {
	const nonce = "deadbeefcafef00d"
	got := buildJobLaunch("echo hi; rm -rf /tmp", "/var/tmp/fleet-job-7.log", nonce)
	// Detaching primitives must be present, and the marker must carry the nonce.
	for _, want := range []string{"setsid sh -c ", "</dev/null", ">/dev/null 2>&1 &", jobExitMarker + nonce + ":$?"} {
		if !strings.Contains(got, want) {
			t.Fatalf("launch missing %q in: %s", want, got)
		}
	}
	// The whole inner script (including the user command) must be wrapped in a
	// single-quoted argument to setsid sh -c, so injection cannot escape.
	if strings.Contains(got, "setsid sh -c echo") {
		t.Fatalf("user command was not quoted: %s", got)
	}
	// A logfile containing a quote must be safely escaped (package core's
	// shellQuote uses the '"'"' escaping idiom).
	tricky := buildJobLaunch("true", "/tmp/a'b.log", nonce)
	if !strings.Contains(tricky, `'"'"'`) {
		t.Fatalf("logfile single-quote not escaped: %s", tricky)
	}
}

func TestNewJobNonce_UniqueAndCharsetSafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		n, err := newJobNonce()
		if err != nil {
			t.Fatalf("newJobNonce: %v", err)
		}
		if len(n) != jobNonceBytes*2 {
			t.Fatalf("nonce length = %d, want %d", len(n), jobNonceBytes*2)
		}
		// Charset-safe: lowercase hex only, so it carries no shell metacharacters
		// and embeds literally into the launcher script.
		for _, c := range n {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Fatalf("nonce has non-hex char %q in %q", c, n)
			}
		}
		if seen[n] {
			t.Fatalf("duplicate nonce %q", n)
		}
		seen[n] = true
	}
}

func TestParseExit(t *testing.T) {
	const nonce = "abcd1234"
	m := jobExitMarker + nonce + ":"
	cases := []struct {
		in       string
		wantCode int
		wantDone bool
	}{
		{"still running\n", 0, false},
		{"output\n" + m + "0\n", 0, true},
		{"output\n" + m + "137\n", 137, true},
		{m + "2", 2, true},
		{m, 0, false},
		{m + "abc", 0, false},
		// A spoofed bare "FLEETEXIT:0" (no nonce) must NOT be accepted.
		{"output\nFLEETEXIT:0\n", 0, false},
		// A marker with the wrong nonce must NOT be accepted.
		{"output\n" + jobExitMarker + "wrongnonce:0\n", 0, false},
		// A marker not anchored at line start (mid-line forgery) must NOT match.
		{"junk" + m + "0\n", 0, false},
	}
	for _, c := range cases {
		code, done := parseExit(c.in, nonce)
		if code != c.wantCode || done != c.wantDone {
			t.Errorf("parseExit(%q) = (%d,%v), want (%d,%v)", c.in, code, done, c.wantCode, c.wantDone)
		}
	}
}

func TestStripExitMarker(t *testing.T) {
	const nonce = "abcd1234"
	m := jobExitMarker + nonce + ":"
	if got := stripExitMarker("line1\nline2\n"+m+"0\n", nonce); got != "line1\nline2" {
		t.Fatalf("stripExitMarker = %q", got)
	}
	if got := stripExitMarker("no marker here", nonce); got != "no marker here" {
		t.Fatalf("stripExitMarker passthrough = %q", got)
	}
	// A spoofed bare "FLEETEXIT:0" line is genuine user output and must remain
	// verbatim (no authentic marker found -> content passes through untouched).
	if got := stripExitMarker("user said FLEETEXIT:0 here\n", nonce); got != "user said FLEETEXIT:0 here\n" {
		t.Fatalf("stripExitMarker stripped non-authentic output: %q", got)
	}
}

// TestJobStore_TailIgnoresSpoofedMarker is the regression test for the
// completion-spoofing bug: a job whose own output prints "FLEETEXIT:0" (and even
// a marker with a guessed-but-wrong nonce) must stay running until the launcher
// writes the authentic FLEETEXIT:<nonce>:<code> line.
func TestJobStore_TailIgnoresSpoofedMarker(t *testing.T) {
	dir := t.TempDir()
	store := NewJobStore(dir)

	// Initial content: the user command spoofs a completion marker (and tries a
	// wrong-nonce variant) while it is still running.
	logContent := "starting work\nFLEETEXIT:0\nFLEETEXIT:guess123:0\nstill working\n"
	exec := func(server, command string) (string, int, error) {
		if strings.HasPrefix(command, "tail ") {
			return logContent, 0, nil
		}
		return "", 0, nil // launch
	}

	rec, err := store.Start(exec, "web-01", "echo FLEETEXIT:0", "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if rec.Nonce == "" {
		t.Fatal("expected a non-empty job nonce")
	}

	// Despite the spoofed marker(s), the job must still be reported running and
	// the fake "FLEETEXIT:0" output must be preserved (it is genuine job output).
	r, out, err := store.Tail(exec, rec.ID)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if r.Status != JobRunning {
		t.Fatalf("spoofed marker reported job done: %+v", r)
	}
	if !strings.Contains(out, "FLEETEXIT:0") {
		t.Fatalf("spoofed user output was stripped: %q", out)
	}

	// Now the launcher writes the authentic, nonce'd marker as the final line.
	logContent = "starting work\nFLEETEXIT:0\nstill working\ndone\n" +
		jobExitMarker + rec.Nonce + ":7\n"
	r, out, err = store.Tail(exec, rec.ID)
	if err != nil {
		t.Fatalf("Tail 2: %v", err)
	}
	if r.Status != JobDone || r.ExitCode != 7 {
		t.Fatalf("authentic marker not honored: status=%s exit=%d", r.Status, r.ExitCode)
	}
	// The authentic trailer is stripped, but the user's own spoof line remains.
	if strings.Contains(out, jobExitMarker+rec.Nonce) {
		t.Fatalf("authentic marker leaked into output: %q", out)
	}
	if !strings.Contains(out, "FLEETEXIT:0") {
		t.Fatalf("user output FLEETEXIT:0 should be preserved: %q", out)
	}
}

func TestJobStore_StartGetList(t *testing.T) {
	dir := t.TempDir()
	store := NewJobStore(dir)

	var launched []string
	exec := func(server, command string) (string, int, error) {
		launched = append(launched, command)
		return "", 0, nil
	}

	rec, err := store.Start(exec, "web-01", "sleep 1", "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if rec.ID != 1 || rec.Status != JobRunning || rec.Server != "web-01" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if rec.Logfile != JobLogfile(1, rec.Nonce) {
		t.Fatalf("unexpected logfile: %s", rec.Logfile)
	}
	// The logfile path must include the per-job nonce (unpredictable) and the
	// launcher must create it 0600 (umask 077), not world-readable.
	if rec.Nonce == "" || !strings.Contains(rec.Logfile, rec.Nonce) {
		t.Fatalf("logfile should include the nonce: %s (nonce %q)", rec.Logfile, rec.Nonce)
	}
	if !strings.Contains(launched[0], "umask 077") {
		t.Fatalf("launch should set umask 077 for 0600 logfile: %v", launched[0])
	}
	if len(launched) != 1 || !strings.Contains(launched[0], "setsid") {
		t.Fatalf("launch command not issued: %v", launched)
	}

	// Second job gets the next id (monotonic counter persisted on disk).
	rec2, err := store.Start(exec, "web-01", "true", "")
	if err != nil {
		t.Fatalf("Start 2: %v", err)
	}
	if rec2.ID != 2 {
		t.Fatalf("expected id 2, got %d", rec2.ID)
	}

	got, err := store.Get(1)
	if err != nil || got.ID != 1 {
		t.Fatalf("Get(1) = %+v, %v", got, err)
	}

	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].ID != 2 {
		t.Fatalf("List should be newest-first: %+v", list)
	}
}

func TestJobStore_StartLaunchFailureRollsBackCounter(t *testing.T) {
	dir := t.TempDir()
	store := NewJobStore(dir)

	failOnce := true
	exec := func(server, command string) (string, int, error) {
		if failOnce {
			failOnce = false
			return "", 0, fmt.Errorf("transport down")
		}
		return "", 0, nil
	}

	if _, err := store.Start(exec, "web-01", "true", ""); err == nil {
		t.Fatal("expected launch failure")
	}
	// The next successful job must still get id 1 (counter rolled back).
	rec, err := store.Start(exec, "web-01", "true", "")
	if err != nil {
		t.Fatalf("Start after failure: %v", err)
	}
	if rec.ID != 1 {
		t.Fatalf("expected id 1 after rollback, got %d", rec.ID)
	}
}

func TestJobStore_TailDetectsCompletion(t *testing.T) {
	dir := t.TempDir()
	store := NewJobStore(dir)

	logContent := "running...\n"
	exec := func(server, command string) (string, int, error) {
		if strings.HasPrefix(command, "tail ") {
			return logContent, 0, nil
		}
		return "", 0, nil // launch
	}

	rec, err := store.Start(exec, "web-01", "do-work", "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// While running, Tail returns the stripped output and keeps status running.
	r, out, err := store.Tail(exec, rec.ID)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if r.Status != JobRunning {
		t.Fatalf("expected running, got %s", r.Status)
	}
	// No marker yet, so content (including its trailing newline) passes through.
	if out != "running...\n" {
		t.Fatalf("unexpected output %q", out)
	}

	// Now the job finishes: the authentic nonce'd marker appears, status flips to
	// done with the exit code.
	logContent = "running...\ndone\n" + jobExitMarker + rec.Nonce + ":3\n"
	r, out, err = store.Tail(exec, rec.ID)
	if err != nil {
		t.Fatalf("Tail 2: %v", err)
	}
	if r.Status != JobDone || r.ExitCode != 3 {
		t.Fatalf("expected done exit 3, got %s exit %d", r.Status, r.ExitCode)
	}
	if strings.Contains(out, jobExitMarker) {
		t.Fatalf("marker leaked into output: %q", out)
	}

	// Completion must be persisted across a fresh store.
	reopened := NewJobStore(dir)
	persisted, err := reopened.Get(rec.ID)
	if err != nil {
		t.Fatalf("Get reopened: %v", err)
	}
	if persisted.Status != JobDone || persisted.ExitCode != 3 {
		t.Fatalf("completion not persisted: %+v", persisted)
	}
}

func TestJobStore_Wait(t *testing.T) {
	dir := t.TempDir()
	store := NewJobStore(dir)

	tails := 0
	var nonce string
	exec := func(server, command string) (string, int, error) {
		if strings.HasPrefix(command, "tail ") {
			tails++
			if tails >= 3 {
				return "out\n" + jobExitMarker + nonce + ":0\n", 0, nil
			}
			return "out\n", 0, nil
		}
		return "", 0, nil
	}

	rec, err := store.Start(exec, "web-01", "task", "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	nonce = rec.Nonce

	slept := 0
	done, err := store.Wait(exec, rec.ID, 0, 10*time.Millisecond, func(time.Duration) { slept++ })
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if done.Status != JobDone || done.ExitCode != 0 {
		t.Fatalf("unexpected final record: %+v", done)
	}
	if slept == 0 {
		t.Fatal("expected at least one poll sleep")
	}
}

func TestJobStore_WaitTimeout(t *testing.T) {
	dir := t.TempDir()
	store := NewJobStore(dir)

	exec := func(server, command string) (string, int, error) {
		if strings.HasPrefix(command, "tail ") {
			return "still going\n", 0, nil // never completes
		}
		return "", 0, nil
	}

	rec, err := store.Start(exec, "web-01", "task", "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// timeout in the past relative to the poll cadence -> must error out.
	_, err = store.Wait(exec, rec.ID, time.Nanosecond, time.Millisecond, func(time.Duration) {})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("unexpected error: %v", err)
	}
}
