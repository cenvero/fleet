// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/transport"
)

// A launch that failed after its request may have reached the server keeps
// the job tracked (it may be running); one that certainly never reached it,
// or that the agent refused, is forgotten as before.
func TestJobStartKeepsAJobWhoseLaunchMayHaveRun(t *testing.T) {
	cases := []struct {
		name string
		err  error
		kept bool
	}{
		{"dropped after send", errors.New("the connection to web dropped after shell.exec was sent, so it may have run"), true},
		{"never sent", transport.NotSent(errors.New("dial tcp: connection refused")), false},
		{"agent refused before running", &AgentRefusedError{Code: "capability_denied", Message: "shell.exec is disabled"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewJobStore(t.TempDir())
			exec := func(string, string) (string, int, error) { return "", 0, tc.err }
			rec, err := store.Start(exec, "web", "sleep 60", "")
			if err == nil {
				t.Fatal("Start succeeded")
			}
			jobs, lerr := store.List()
			if lerr != nil {
				t.Fatal(lerr)
			}
			if tc.kept {
				if len(jobs) != 1 || jobs[0].Status != JobRunning || rec.ID != jobs[0].ID {
					t.Fatalf("a launch that may have run was not kept: rec=%+v jobs=%+v", rec, jobs)
				}
				if !strings.Contains(err.Error(), "may have run") || !strings.Contains(err.Error(), "fleet job status") {
					t.Fatalf("error does not say the job may be running: %v", err)
				}
				return
			}
			if len(jobs) != 0 || rec.ID != 0 {
				t.Fatalf("a launch that never ran was kept: rec=%+v jobs=%+v", rec, jobs)
			}
		})
	}
}

// A probe that fails because the connection dropped says nothing about the
// job: it is not a strike towards `lost`, and nothing is notified.
func TestJobCheckIgnoresAProbeThatMayHaveRun(t *testing.T) {
	store := NewJobStore(t.TempDir())
	launch := func(string, string) (string, int, error) { return "", 0, nil }
	rec, err := store.Start(launch, "web", "sleep 60", "")
	if err != nil {
		t.Fatal(err)
	}
	dropped := func(string, string) (string, int, error) {
		return "", 0, errors.New("the connection to web dropped after shell.exec was sent, so it may have run")
	}
	for i := 0; i < jobLostStrikes+2; i++ {
		got, err := store.Check(dropped, rec.ID)
		if err == nil {
			t.Fatal("Check hid the probe failure")
		}
		if got.Status != JobRunning || got.Strikes != 0 {
			t.Fatalf("probe %d: a dropped probe changed the job: %+v", i, got)
		}
	}
}

// A job's server counts as removed only when its record file is gone, never
// because the name was written differently (a case-insensitive filesystem
// opens "Web-01" as "web-01") or the list could not be read.
func TestJobServerRemovedOnlyWhenTheRecordIsGone(t *testing.T) {
	app := newImportTestApp(t)
	if err := app.AddServer(ServerRecord{Name: "web-01", Address: "127.0.0.1", Port: 2222, Mode: transport.ModeDirect}); err != nil {
		t.Fatal(err)
	}
	if app.serverRemoved("web-01") {
		t.Fatal("an existing server counted as removed")
	}
	if _, err := os.Stat(filepath.Join(app.ConfigDir, "servers", "Web-01.toml")); err == nil && app.serverRemoved("Web-01") {
		t.Fatal("on a case-insensitive filesystem the differently-cased name counted as removed")
	}
	if app.serverRemoved("../web-01") {
		t.Fatal("an unsafe name counted as removed")
	}
	if err := app.RemoveServerWithOptions("web-01", RemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if !app.serverRemoved("web-01") {
		t.Fatal("a removed server did not count as removed")
	}
}
