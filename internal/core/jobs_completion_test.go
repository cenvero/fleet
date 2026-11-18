// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shimSetsid puts a `setsid` on PATH that just runs its arguments, for systems
// without one (macOS). The launcher still backgrounds the job itself. Not
// parallel-safe: it changes PATH.
func shimSetsid(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "setsid"), []byte("#!/bin/sh\nexec \"$@\"\n"), 0o700); err != nil { // #nosec G306 -- test shim must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// waitForFileContaining polls path until it contains want, and returns it.
func waitForFileContaining(t *testing.T, path, want string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(path) // #nosec G304 -- test temp file
		if strings.Contains(string(data), want) {
			return string(data)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never contained %q; it has %q", path, want, data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A job whose output does not end in a newline (`printf done`) used to stay
// "running" forever: the launcher appended the marker straight after the
// output ("doneFLEETEXIT:..."), and the marker is only accepted at the start
// of a line.
func TestJobLaunchFinishesWhenOutputLacksTrailingNewline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the job launcher is a POSIX shell script")
	}
	shimSetsid(t)
	const nonce = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct{ command, want string }{
		{"printf done", "done"},
		{"printf 'line one\\nline two\\n'", "line one\nline two\n"},
		{"exit 4", ""},
	} {
		logfile := filepath.Join(t.TempDir(), "job.log")
		launch := exec.Command("/bin/sh", "-c", buildJobLaunch(tc.command, logfile, nonce)) // #nosec G204 -- test runs the launcher it builds
		if out, err := launch.CombinedOutput(); err != nil {
			t.Fatalf("launch %q: %v: %s", tc.command, err, out)
		}
		content := waitForFileContaining(t, logfile, jobExitMarker+nonce+":")
		code, done := parseExit(content, nonce)
		if !done {
			t.Fatalf("%q: completion marker not recognised in %q", tc.command, content)
		}
		wantCode := 0
		if tc.command == "exit 4" {
			wantCode = 4
		}
		if code != wantCode {
			t.Fatalf("%q: exit code %d, want %d", tc.command, code, wantCode)
		}
		if got := stripExitMarker(content, nonce); got != tc.want {
			t.Fatalf("%q: displayed output %q, want %q", tc.command, got, tc.want)
		}
	}
}

// Two pollers (the background helper and `fleet job wait`, say) that both see
// a failed job finish must send exactly one "job-failed" notification.
func TestJobFailureNotifiesOnceWithTwoPollers(t *testing.T) {
	dir := t.TempDir()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := NewNotifyStore(dir).Add(NotifyTarget{
		Kind: NotifyKindWebhook, URL: srv.URL, AllowInternal: true, Events: []string{NotifyEventJobFailed},
	}); err != nil {
		t.Fatalf("add notify target: %v", err)
	}

	launch := func(string, string) (string, int, error) { return "", 0, nil }
	rec, err := NewJobStore(dir).Start(launch, "web-01", "false", "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Both pollers read the finished log before either records the result.
	var arrived sync.WaitGroup
	arrived.Add(2)
	release := make(chan struct{})
	go func() { arrived.Wait(); close(release) }()
	var reads atomic.Int32
	exec := func(_, command string) (string, int, error) {
		if reads.Add(1) <= 2 {
			arrived.Done()
			<-release
		}
		log := "boom\n" + jobExitMarker + rec.Nonce + ":2\n"
		if strings.Contains(command, jobProbeHeader) {
			return fakeProbe(command, log), 0, nil
		}
		return log, 0, nil
	}

	var wg sync.WaitGroup
	results := make([]JobRecord, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Separate stores, as two processes would have.
			results[i], _, errs[i] = NewJobStore(dir).Tail(exec, rec.ID)
		}()
	}
	wg.Wait()
	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("poller %d: %v", i, errs[i])
		}
		if results[i].Status != JobDone || results[i].ExitCode != 2 {
			t.Fatalf("poller %d saw %+v, want done exit 2", i, results[i])
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("job-failed notifications sent = %d, want exactly 1", got)
	}
}

// tailBytesPattern finds `tail -c N <file>` reads in a command, and whether
// the bytes are piped into grep on the server (only a matching line comes
// back) rather than sent back.
var tailBytesPattern = regexp.MustCompile(`tail -c ([0-9]+) ('[^']*'|\S+)( 2>/dev/null)?( \| grep)?`)

// Waiting for a job must not pull the whole (up to 4 MiB) log across the
// network on every poll: completion only needs the last few hundred bytes.
func TestJobWaitPollsWithoutReadingTheWholeLog(t *testing.T) {
	store := NewJobStore(t.TempDir())
	launch := func(string, string) (string, int, error) { return "", 0, nil }
	rec, err := store.Start(launch, "web-01", "long-task", "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	polls, maxRead := 0, 0
	exec := func(_, command string) (string, int, error) {
		for _, m := range tailBytesPattern.FindAllStringSubmatch(command, -1) {
			if m[4] != "" {
				continue // searched on the server; only the marker line returns
			}
			if n, _ := strconv.Atoi(m[1]); n > maxRead {
				maxRead = n
			}
		}
		polls++
		body := "working\n"
		if polls >= 3 {
			body += "\n" + jobExitMarker + rec.Nonce + ":0\n"
		}
		if strings.Contains(command, "FLEETPROBE") {
			return fakeProbe(command, body), 0, nil
		}
		return body, 0, nil
	}
	done, err := store.Wait(exec, rec.ID, 10*time.Second, time.Millisecond, func(time.Duration) {})
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if done.Status != JobDone {
		t.Fatalf("Wait returned %+v, want done", done)
	}
	if maxRead == 0 || maxRead > 512 {
		t.Fatalf("a poll read %d bytes of the log, want at most 512", maxRead)
	}
}

// A job whose log file is gone (deleted, swept, or never created) can never
// report completion; waiting on it must end with the job marked lost rather
// than poll until the timeout (or forever).
func TestJobWaitEndsWhenTheLogIsGone(t *testing.T) {
	dir := t.TempDir()
	store := NewJobStore(dir)
	const nonce = "feedfacefeedfacefeedfacefeedface"
	if err := store.write(jobsDocument{Counter: 1, Jobs: []JobRecord{{
		ID: 1, Server: "web-01", Command: "sleep 99999", Logfile: JobLogfile(1, nonce),
		Nonce: nonce, Status: JobRunning, Started: time.Now().Add(-time.Hour).UTC(),
	}}}); err != nil {
		t.Fatal(err)
	}
	exec := func(_, command string) (string, int, error) {
		if strings.Contains(command, "FLEETPROBE") {
			return "FLEETPROBE:unknown:nolog\n", 0, nil
		}
		return "", 0, nil // `tail` of a missing file prints nothing (stderr is discarded)
	}
	rec, err := store.Wait(exec, 1, 2*time.Second, time.Millisecond, func(time.Duration) {})
	if rec.Status != JobStatus("lost") {
		t.Fatalf("Wait ended with status %q (err %v), want lost", rec.Status, err)
	}
	persisted, gerr := NewJobStore(dir).Get(1)
	if gerr != nil || persisted.Status != JobStatus("lost") {
		t.Fatalf("lost status not persisted: %+v, %v", persisted, gerr)
	}
}

// Starting a job must not hold the cross-process jobs lock while the launch
// command crosses the network: one slow or unreachable server would stall
// every other fleet process that touches jobs.json.
func TestJobStartDoesNotHoldTheJobsLockDuringLaunch(t *testing.T) {
	dir := t.TempDir()
	launches := 0
	exec := func(string, string) (string, int, error) {
		launches++
		done := make(chan error, 1)
		go func() {
			_, err := NewJobStore(dir).Prune(time.Now(), nil)
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("another process touching jobs.json during a launch: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("another process could not take the jobs lock while a job was being launched")
			<-done // let it time out before the store is torn down
		}
		return "", 0, nil
	}
	if _, err := NewJobStore(dir).Start(exec, "web-01", "true", ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if launches != 1 {
		t.Fatalf("launches = %d, want 1", launches)
	}
}

// runJobProbe runs buildJobProbe locally, as the agent's shell.exec would.
func runJobProbe(t *testing.T, logfile, nonce string) jobProbe {
	t.Helper()
	out, err := exec.Command("/bin/sh", "-c", buildJobProbe(logfile, nonce, jobProbeBytes, 0)).Output() // #nosec G204 -- test runs the probe it builds
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	p, err := parseJobProbe(string(out))
	if err != nil {
		t.Fatalf("parse probe reply %q: %v", out, err)
	}
	return p
}

// The probe, run by a real shell, tells a running job from a finished one and
// from one whose process or log is gone.
func TestJobProbeAgainstARealShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the job launcher is a POSIX shell script")
	}
	shimSetsid(t)
	dir := t.TempDir()
	gate := filepath.Join(dir, "gate")
	t.Cleanup(func() { _ = os.WriteFile(gate, nil, 0o600) }) // never leave the job looping
	const nonce = "abcdefabcdefabcdefabcdefabcdef12"
	logfile := filepath.Join(dir, "job.log")
	job := "printf started; while [ ! -e " + shellQuote(gate) + " ]; do sleep 0.05; done; printf ' and finished'"
	if out, err := exec.Command("/bin/sh", "-c", buildJobLaunch(job, logfile, nonce)).CombinedOutput(); err != nil { // #nosec G204 -- test runs the launcher it builds
		t.Fatalf("launch: %v: %s", err, out)
	}
	waitForFileContaining(t, logfile, "started")

	p := runJobProbe(t, logfile, nonce)
	if !p.logExists || p.liveness == "dead" {
		t.Fatalf("running job probed as %+v", p)
	}
	if p.liveness != "alive" {
		t.Logf("this system cannot report liveness (%q); only the log rules apply here", p.liveness)
	}
	if _, done := parseExit(p.logTail, nonce); done {
		t.Fatalf("running job probed as finished: %q", p.logTail)
	}

	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForFileContaining(t, logfile, jobExitMarker+nonce)
	p = runJobProbe(t, logfile, nonce)
	if code, done := parseExit(p.logTail, nonce); !done || code != 0 {
		t.Fatalf("finished job probed as not finished: %+v", p)
	}
	if got := stripExitMarker(p.logTail, nonce); got != "started and finished" {
		t.Fatalf("probe tail = %q", got)
	}

	// A log without a marker and no process behind it: the job was killed, or
	// the server rebooted.
	orphan := filepath.Join(dir, "orphan.log")
	if err := os.WriteFile(orphan, []byte("half done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const otherNonce = "00000000000000000000000000000001"
	if p := runJobProbe(t, orphan, otherNonce); !p.logExists || (p.liveness != "dead" && p.liveness != "unknown") {
		t.Fatalf("orphaned log probed as %+v, want dead (or unknown)", p)
	} else if runtime.GOOS == "linux" && p.liveness != "dead" {
		t.Fatalf("Linux must be able to tell the job is gone: %+v", p)
	}
	if p := runJobProbe(t, filepath.Join(dir, "never-created.log"), otherNonce); p.logExists {
		t.Fatalf("missing log probed as present: %+v", p)
	}
}

func TestParseJobProbeRejectsForeignReplies(t *testing.T) {
	for _, reply := range []string{"", "hello\n", "FLEETPROBE:maybe:log\n", "FLEETPROBE:alive:perhaps\n", "FLEETPROBE:alive\n"} {
		if _, err := parseJobProbe(reply); err == nil {
			t.Errorf("parseJobProbe(%q) accepted", reply)
		}
	}
	p, err := parseJobProbe("FLEETPROBE:dead:log\nFLEETPROBE:alive:log\nout")
	if err != nil || p.liveness != "dead" || !p.logExists || p.logTail != "FLEETPROBE:alive:log\nout" {
		t.Fatalf("log content was parsed as a header: %+v, %v", p, err)
	}
	// Shell startup chatter before the header is skipped.
	p, err = parseJobProbe("Welcome to web-01\nFLEETPROBE:alive:nolog\n")
	if err != nil || p.liveness != "alive" || p.logExists {
		t.Fatalf("reply after a banner: %+v, %v", p, err)
	}
}

// seedRunningJob stores one running job started at started and returns its store.
func seedRunningJob(t *testing.T, dir string, started time.Time) (*JobStore, JobRecord) {
	t.Helper()
	const nonce = "11111111111111111111111111111111"
	rec := JobRecord{
		ID: 1, Server: "web-01", Command: "make world", Logfile: JobLogfile(1, nonce),
		Nonce: nonce, Status: JobRunning, Started: started.UTC(),
	}
	store := NewJobStore(dir)
	if err := store.write(jobsDocument{Counter: 1, Jobs: []JobRecord{rec}}); err != nil {
		t.Fatal(err)
	}
	return store, rec
}

func probeReplying(reply string) ExecFunc {
	return func(string, string) (string, int, error) { return reply, 0, nil }
}

func TestJobCheckLostRules(t *testing.T) {
	old := time.Now().Add(-time.Hour)

	t.Run("dead three times in a row", func(t *testing.T) {
		store, _ := seedRunningJob(t, t.TempDir(), old)
		var rec JobRecord
		for i := 1; i <= jobLostStrikes; i++ {
			var err error
			if rec, err = store.Check(probeReplying(jobProbeHeader+"dead:log\nhalf\n"), 1); err != nil {
				t.Fatal(err)
			}
			if i < jobLostStrikes && rec.Status != JobRunning {
				t.Fatalf("lost after %d strike(s)", i)
			}
		}
		if rec.Status != JobLost || !strings.Contains(rec.LostReason, "no process") || rec.Finished.IsZero() {
			t.Fatalf("after %d dead probes: %+v", jobLostStrikes, rec)
		}
		// Once lost, nothing is probed any more.
		if again, err := store.Check(func(string, string) (string, int, error) {
			t.Fatal("probed a lost job")
			return "", 0, nil
		}, 1); err != nil || again.Status != JobLost {
			t.Fatalf("re-check of a lost job: %+v, %v", again, err)
		}
	})

	t.Run("alive resets and unknown changes nothing", func(t *testing.T) {
		store, _ := seedRunningJob(t, t.TempDir(), old)
		for _, reply := range []string{"dead:log", "nolog", "alive:log", "dead:log", "unknown:log", "unknown:nolog"} {
			if !strings.Contains(reply, ":") {
				reply = "unknown:" + reply
			}
			if _, err := store.Check(probeReplying(jobProbeHeader+reply+"\n"), 1); err != nil {
				t.Fatal(err)
			}
		}
		rec, _ := store.Get(1)
		// dead, nolog (2) -> alive resets (0) -> dead (1) -> unknown (1) -> nolog (2)
		if rec.Status != JobRunning || rec.Strikes != 2 {
			t.Fatalf("status %s strikes %d, want running with 2", rec.Status, rec.Strikes)
		}
	})

	t.Run("a young job is never lost", func(t *testing.T) {
		store, _ := seedRunningJob(t, t.TempDir(), time.Now())
		for range jobLostStrikes + 2 {
			if _, err := store.Check(probeReplying(jobProbeHeader+"unknown:nolog\n"), 1); err != nil {
				t.Fatal(err)
			}
		}
		if rec, _ := store.Get(1); rec.Status != JobRunning {
			t.Fatalf("job lost within %s of starting: %+v", jobLostMinAge, rec)
		}
	})

	t.Run("unreachable is retried, removed is lost", func(t *testing.T) {
		store, _ := seedRunningJob(t, t.TempDir(), old)
		unreachable := func(string, string) (string, int, error) { return "", 0, fmt.Errorf("connection refused") }
		removed := false
		store.ServerRemoved = func(name string) bool { return removed && name == "web-01" }
		for range jobLostStrikes + 2 {
			if _, err := store.Check(unreachable, 1); err == nil {
				t.Fatal("transport error swallowed")
			}
		}
		if rec, _ := store.Get(1); rec.Status != JobRunning || rec.Strikes != 0 {
			t.Fatalf("an unreachable server counted against the job: %+v", rec)
		}
		removed = true
		rec, err := store.Check(unreachable, 1)
		if err != nil || rec.Status != JobLost || !strings.Contains(rec.LostReason, "removed") {
			t.Fatalf("job on a removed server: %+v, %v", rec, err)
		}
	})

	t.Run("an odd reply is an error, not a verdict", func(t *testing.T) {
		store, _ := seedRunningJob(t, t.TempDir(), old)
		for range jobLostStrikes + 1 {
			if _, err := store.Check(probeReplying("sh: syntax error\n"), 1); err == nil {
				t.Fatal("accepted a foreign reply")
			}
		}
		if rec, _ := store.Get(1); rec.Status != JobRunning || rec.Strikes != 0 {
			t.Fatalf("foreign replies changed the job: %+v", rec)
		}
	})
}

func TestJobPruneRemovesOldLostJobs(t *testing.T) {
	dir := t.TempDir()
	store, _ := seedRunningJob(t, dir, time.Now().Add(-48*time.Hour))
	if _, err := store.lose(1, "test"); err != nil {
		t.Fatal(err)
	}
	removed, err := store.Prune(time.Now().Add(time.Minute), nil)
	if err != nil || removed != 1 {
		t.Fatalf("Prune removed %d (%v), want the lost job", removed, err)
	}
}

// A job that leaves a child writing to its log gets more output after its
// marker than the probe's window holds. Once the wrapper is gone the probe
// finds the marker in the whole log, so the job is done, not lost.
func TestJobProbeFindsAMarkerFollowedByMoreOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the job launcher is a POSIX shell script")
	}
	shimSetsid(t)
	const nonce = "33333333333333333333333333333333"
	logfile := filepath.Join(t.TempDir(), "job.log")
	job := "(sleep 0.3; printf '%2000s' tail-end) & exit 5"
	if out, err := exec.Command("/bin/sh", "-c", buildJobLaunch(job, logfile, nonce)).CombinedOutput(); err != nil { // #nosec G204 -- test runs the launcher it builds
		t.Fatalf("launch: %v: %s", err, out)
	}
	waitForFileContaining(t, logfile, "tail-end")

	p := runJobProbe(t, logfile, nonce)
	if _, inTail := parseExit(p.logTail, nonce); inTail {
		t.Fatal("test setup: the marker is still inside the probe window")
	}
	if p.liveness == "unknown" {
		t.Logf("this system cannot report liveness; the last %d bytes are searched", maxJobLogBytes)
	}
	if !p.exited || p.exitCode != 5 {
		t.Fatalf("probe = %+v, want the exit code found beyond the window", p)
	}

	// And through Check: the job is done with that code.
	store := NewJobStore(t.TempDir())
	if err := store.write(jobsDocument{Counter: 1, Jobs: []JobRecord{{
		ID: 1, Server: "local", Command: job, Logfile: logfile, Nonce: nonce,
		Status: JobRunning, Started: time.Now().Add(-time.Hour).UTC(),
	}}}); err != nil {
		t.Fatal(err)
	}
	local := func(_, command string) (string, int, error) {
		out, err := exec.Command("/bin/sh", "-c", command).Output() // #nosec G204 -- test runs the probe it builds
		return string(out), 0, err
	}
	rec, err := store.Check(local, 1)
	if err != nil || rec.Status != JobDone || rec.ExitCode != 5 {
		t.Fatalf("Check = %+v, %v", rec, err)
	}
}

func TestParseJobProbeExitCode(t *testing.T) {
	p, err := parseJobProbe("FLEETPROBE:dead:log:7\nout")
	if err != nil || !p.exited || p.exitCode != 7 || p.logTail != "out" {
		t.Fatalf("probe = %+v, %v", p, err)
	}
	if p, err := parseJobProbe("FLEETPROBE:alive:log:\nout"); err != nil || p.exited {
		t.Fatalf("empty code: %+v, %v", p, err)
	}
	for _, bad := range []string{"FLEETPROBE:dead:log:x\n", "FLEETPROBE:dead:log:-1\n", "FLEETPROBE:dead:log:1:2\n"} {
		if _, err := parseJobProbe(bad); err == nil {
			t.Errorf("parseJobProbe(%q) accepted", bad)
		}
	}
}

// Whatever the log holds, a probe's reply is the header and at most
// jobProbeBytes of it, for every liveness outcome.
func TestJobProbeReplyStaysSmall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the probe is a POSIX shell script")
	}
	logfile := filepath.Join(t.TempDir(), "big.log")
	if err := os.WriteFile(logfile, []byte(strings.Repeat("0123456789abcdef\n", 128<<10)), 0o600); err != nil { // ~2 MiB
		t.Fatal(err)
	}
	const nonce = "44444444444444444444444444444444"
	out, err := exec.Command("/bin/sh", "-c", buildJobProbe(logfile, nonce, jobProbeBytes, 0)).Output() // #nosec G204 -- test runs the probe it builds
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > jobProbeBytes+80 {
		t.Fatalf("probe reply is %d bytes for a 2 MiB log", len(out))
	}
	if p, err := parseJobProbe(string(out)); err != nil || p.exited || len(p.logTail) != jobProbeBytes {
		t.Fatalf("probe = %+v, %v", p, err)
	}
}
