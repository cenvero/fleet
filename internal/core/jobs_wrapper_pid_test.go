// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The nonce is not secret on the server (it is in the log file's name), so
// any local user can start a process whose command line carries the marker.
// With the wrapper's pid recorded, only that process decides liveness: a
// decoy cannot keep a dead job "alive".
func TestJobProbeChecksOnlyTheWrapperPID(t *testing.T) {
	nonce := "0123456789abcdef0123456789abcdef"
	logfile := filepath.Join(t.TempDir(), "job.log")
	if err := os.WriteFile(logfile, []byte("working\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	decoy := exec.Command("/bin/sh", "-c", "sleep 30; true", jobExitMarker+nonce+":") // two commands: sh does not exec into sleep
	if err := decoy.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = decoy.Process.Kill(); _ = decoy.Wait() })
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	probe := func(pid int) jobProbe {
		out, err := exec.Command("/bin/sh", "-c", buildJobProbe(logfile, nonce, jobProbeBytes, pid)).Output() // #nosec G204 -- test runs the probe it builds
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		p, err := parseJobProbe(string(out))
		if err != nil {
			t.Fatalf("parse %q: %v", out, err)
		}
		return p
	}
	if p := probe(0); p.liveness != "alive" {
		t.Fatalf("without a pid the decoy is not found (liveness %q): the test does not set up the attack", p.liveness)
	}
	if p := probe(gone.Process.Pid); p.liveness != "dead" {
		t.Fatalf("a decoy kept a job whose wrapper is gone %q", p.liveness)
	}
	if p := probe(decoy.Process.Pid); p.liveness != "alive" {
		t.Fatalf("the wrapper itself (here the process carrying the marker) is %q", p.liveness)
	}
}

// A marker in the job's recent output while its wrapper still runs was
// printed by the job, not by the wrapper (which writes it last): it does
// not finish the job.
func TestJobCheckIgnoresAMarkerWhileTheWrapperRuns(t *testing.T) {
	store := NewJobStore(t.TempDir())
	launch := func(string, string) (string, int, error) { return jobPIDPrefix + "4242\n", 0, nil }
	rec, err := store.Start(launch, "web", "cat app.log", "")
	if err != nil {
		t.Fatal(err)
	}
	if rec.PID != 4242 || rec.Launched.IsZero() {
		t.Fatalf("launch did not record the wrapper: %+v", rec)
	}
	forged := func(_, command string) (string, int, error) {
		if !strings.Contains(command, "/proc/"+strconv.Itoa(4242)+"/cmdline") {
			t.Fatalf("the probe does not check the wrapper's pid: %s", command)
		}
		return jobProbeHeader + "alive:log:\n" + "line\n" + jobExitMarker + rec.Nonce + ":0\n", 0, nil
	}
	got, err := store.Check(forged, rec.ID)
	if err != nil || got.Status != JobRunning {
		t.Fatalf("a marker printed while the wrapper runs finished the job: %+v %v", got, err)
	}
	finished := func(string, string) (string, int, error) {
		return jobProbeHeader + "dead:log:3\nline\n", 0, nil
	}
	if got, err := store.Check(finished, rec.ID); err != nil || got.Status != JobDone || got.ExitCode != 3 {
		t.Fatalf("the wrapper's own result: %+v %v", got, err)
	}
}

// A job whose launch has not returned yet is not counted towards lost.
func TestJobNotStruckWhileItsLaunchIsInFlight(t *testing.T) {
	store := NewJobStore(t.TempDir())
	rec, err := store.Start(func(string, string) (string, int, error) { return "", 0, nil }, "web", "sleep 1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.transition(rec.ID, func(j *JobRecord) bool { j.Launched = time.Time{}; return true }); err != nil {
		t.Fatal(err)
	}
	nolog := func(string, string) (string, int, error) { return jobProbeHeader + "dead:nolog:\n", 0, nil }
	for i := 0; i < jobLostStrikes+1; i++ {
		got, err := store.Check(nolog, rec.ID)
		if err != nil || got.Status != JobRunning || got.Strikes != 0 {
			t.Fatalf("probe %d struck a job whose launch is in flight: %+v %v", i, got, err)
		}
	}
}

// Only agent answers that come before anything runs forget a job; others
// (internal_error can follow a started command) keep it.
func TestJobLaunchAgentErrorsKeepTheJobUnlessPreExec(t *testing.T) {
	for code, kept := range map[string]bool{"bad_payload": false, "unsupported_action": false, "internal_error": true, "something_new": true} {
		store := NewJobStore(t.TempDir())
		_, _ = store.Start(func(string, string) (string, int, error) {
			return "", 0, &AgentRefusedError{Code: code, Message: "x"}
		}, "web", "true", "")
		jobs, _ := store.List()
		if (len(jobs) == 1) != kept {
			t.Errorf("agent code %q: kept=%v, want %v", code, len(jobs) == 1, kept)
		}
	}
}
