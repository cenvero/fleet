// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
)

// jobTrackingRig runs the helper's "jobs" task against the in-process agent,
// which runs job launchers and probes in a real /bin/sh on this machine. Job
// logs go to a temp dir, and "job-failed" notifications to a counting webhook.
type jobTrackingRig struct {
	app      *App
	tracker  *jobTracker
	notified *atomic.Int32
}

func newJobTrackingRig(t *testing.T) *jobTrackingRig {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("background jobs run through a POSIX shell")
	}
	shimSetsid(t)
	oldDir, oldFirst, oldMax, oldRetry := jobLogDir, jobProbeFirst, jobProbeMax, serverRetryFirst
	jobLogDir = t.TempDir()
	jobProbeFirst, jobProbeMax, serverRetryFirst = 5*time.Millisecond, 20*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { jobLogDir, jobProbeFirst, jobProbeMax, serverRetryFirst = oldDir, oldFirst, oldMax, oldRetry })

	configDir := newHelperTestConfig(t)
	rig := &jobTrackingRig{tracker: newJobTracker(), notified: &atomic.Int32{}}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rig.notified.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)
	if err := NewNotifyStore(configDir).Add(NotifyTarget{
		Kind: NotifyKindWebhook, URL: hook.URL, AllowInternal: true, Events: []string{NotifyEventJobFailed},
	}); err != nil {
		t.Fatal(err)
	}

	app, err := Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	// Nothing must reach a daemon this machine may be running.
	app.Config.Runtime.ControlAddress = "127.0.0.1:1"
	srv := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
	}
	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40000", "127.0.0.1:2222")
		go func() { _ = srv.ServeConn(serverConn) }()
		return clientConn, nil
	}
	if err := app.AddServer(ServerRecord{Name: "box", Address: "127.0.0.1", Port: 2222, Mode: transport.ModeDirect, User: "cenvero-agent"}); err != nil {
		t.Fatal(err)
	}
	rig.app = app
	return rig
}

func (r *jobTrackingRig) start(t *testing.T, command string) JobRecord {
	t.Helper()
	rec, err := r.app.Jobs().Start(r.app.jobExec(context.Background()), "box", command, "")
	if err != nil {
		t.Fatalf("start %q: %v", command, err)
	}
	return rec
}

// track runs the jobs task the way the helper does (only when something is
// pending) until job id stops running, and returns its record and the task's
// last summary.
func (r *jobTrackingRig) track(t *testing.T, id int) (JobRecord, string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var said string
	for {
		if r.tracker.pending(r.app.ConfigDir, wallNow()) {
			report := &helperReport{}
			if err := r.tracker.run(context.WithValue(context.Background(), helperReportKey{}, report), r.app); err != nil {
				t.Fatalf("jobs task: %v", err)
			}
			said = report.msg
		}
		rec, err := r.app.Jobs().Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status != JobRunning {
			return rec, said
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d still running: %+v", id, rec)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHelperTracksAJobToCompletion(t *testing.T) {
	rig := newJobTrackingRig(t)
	rec := rig.start(t, "printf done")
	got, said := rig.track(t, rec.ID)
	if got.Status != JobDone || got.ExitCode != 0 || got.Finished.IsZero() {
		t.Fatalf("tracked job = %+v", got)
	}
	if !strings.Contains(said, "just finished") {
		t.Errorf("task summary = %q", said)
	}
	if n := rig.notified.Load(); n != 0 {
		t.Fatalf("a successful job sent %d job-failed notification(s)", n)
	}
	// Its output, with the marker stripped, is what `fleet job logs` shows.
	if _, out, err := rig.app.Jobs().Tail(rig.app.jobExec(context.Background()), rec.ID); err != nil || out != "done" {
		t.Fatalf("job output = %q, %v", out, err)
	}
}

// The helper and `fleet job wait` polling the same failed job together send
// one "job-failed" notification between them.
func TestHelperAndJobWaitNotifyAFailureOnce(t *testing.T) {
	rig := newJobTrackingRig(t)
	rec := rig.start(t, "printf oops; exit 3")
	var wg sync.WaitGroup
	var waited JobRecord
	var waitErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		rig.track(t, rec.ID)
	}()
	go func() {
		defer wg.Done()
		waited, waitErr = rig.app.Jobs().Wait(rig.app.jobExec(context.Background()), rec.ID, 15*time.Second, 2*time.Millisecond, nil)
	}()
	wg.Wait()
	if waitErr != nil || waited.Status != JobDone || waited.ExitCode != 3 {
		t.Fatalf("job wait = %+v, %v", waited, waitErr)
	}
	if n := rig.notified.Load(); n != 1 {
		t.Fatalf("job-failed notifications = %d, want exactly 1", n)
	}
}

// A job whose log file disappears (swept, deleted) can never report back; the
// helper marks it lost and says so once.
func TestHelperMarksAJobWithoutALogLost(t *testing.T) {
	rig := newJobTrackingRig(t)
	gate := filepath.Join(t.TempDir(), "gate")
	rec := rig.start(t, "while [ ! -e "+shellQuote(gate)+" ]; do sleep 0.05; done")
	t.Cleanup(func() {
		// Let the job end, and wait until no process runs it any more.
		_ = os.WriteFile(gate, nil, 0o600)
		deadline := time.Now().Add(5 * time.Second)
		for runJobProbe(t, rec.Logfile, rec.Nonce).liveness == "alive" && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
	})
	waitFor(t, "the job's log", func() bool { _, err := os.Stat(rec.Logfile); return err == nil })

	// Started long enough ago to be given up on, then its log is swept.
	if _, _, err := rig.app.Jobs().transition(rec.ID, func(j *JobRecord) bool {
		j.Started = j.Started.Add(-time.Hour)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rec.Logfile); err != nil {
		t.Fatal(err)
	}
	got, said := rig.track(t, rec.ID)
	if got.Status != JobLost || !strings.Contains(got.LostReason, "log file") || got.Strikes < jobLostStrikes {
		t.Fatalf("job without a log = %+v", got)
	}
	if !strings.Contains(said, "1 lost") {
		t.Errorf("task summary = %q", said)
	}
	if n := rig.notified.Load(); n != 1 {
		t.Fatalf("job-failed notifications for a lost job = %d, want 1", n)
	}
}

// Jobs on reverse-mode servers wait while no daemon can reach them: the task
// neither fails nor retries them in a tight loop.
func TestHelperDefersReverseModeJobsWithoutADaemon(t *testing.T) {
	rig := newJobTrackingRig(t)
	if err := rig.app.AddServer(ServerRecord{Name: "rev", Address: "203.0.113.9", Port: 2222, Mode: transport.ModeReverse}); err != nil {
		t.Fatal(err)
	}
	const nonce = "22222222222222222222222222222222"
	if err := rig.app.Jobs().write(jobsDocument{Counter: 1, Jobs: []JobRecord{{
		ID: 1, Server: "rev", Command: "sleep 1", Logfile: JobLogfile(1, nonce), Nonce: nonce,
		Status: JobRunning, Started: time.Now().Add(-time.Minute).UTC(),
	}}}); err != nil {
		t.Fatal(err)
	}
	now := wallNow()
	if !rig.tracker.pending(rig.app.ConfigDir, now) {
		t.Fatal("a new running job is not pending")
	}
	report := &helperReport{}
	if err := rig.tracker.run(context.WithValue(context.Background(), helperReportKey{}, report), rig.app); err != nil {
		t.Fatalf("jobs task failed for a reverse-mode server without a daemon: %v", err)
	}
	if !strings.Contains(report.msg, "wait for the daemon") {
		t.Errorf("task summary = %q", report.msg)
	}
	if rig.tracker.pending(rig.app.ConfigDir, now.Add(reverseRetry/2)) {
		t.Fatal("the reverse-mode job is retried before reverseRetry")
	}
	if !rig.tracker.pending(rig.app.ConfigDir, now.Add(reverseRetry+time.Second)) {
		t.Fatal("the reverse-mode job is never retried")
	}
	if rec, _ := rig.app.Jobs().Get(1); rec.Status != JobRunning || rec.Strikes != 0 {
		t.Fatalf("job changed: %+v", rec)
	}
}

// Wall-clock deadlines: after the machine sleeps, every job is overdue, and
// one run probes at most jobProbesPerRound of them.
func TestHelperSpreadsOverdueJobsAcrossRuns(t *testing.T) {
	rig := newJobTrackingRig(t)
	// Jobs started an hour ago that nobody has probed: all overdue at once.
	// Their logs don't exist, so each probe is one strike, not yet a verdict.
	var jobs []JobRecord
	for i := 1; i <= jobProbesPerRound*2+1; i++ {
		jobs = append(jobs, JobRecord{ID: i, Server: "box", Command: "x", Logfile: JobLogfile(i, "n"), Nonce: "n",
			Status: JobRunning, Started: time.Now().Add(-time.Hour).UTC()})
	}
	if err := rig.app.Jobs().write(jobsDocument{Counter: len(jobs), Jobs: jobs}); err != nil {
		t.Fatal(err)
	}
	probedPerRun := func() int {
		if err := rig.tracker.run(context.Background(), rig.app); err != nil {
			t.Fatal(err)
		}
		struck := 0
		all, _ := rig.app.Jobs().List()
		for _, j := range all {
			struck += j.Strikes
		}
		return struck
	}
	if got := probedPerRun(); got != jobProbesPerRound {
		t.Fatalf("the first run probed %d overdue jobs, want %d", got, jobProbesPerRound)
	}
	if got := probedPerRun(); got != 2*jobProbesPerRound {
		t.Fatalf("after two runs %d probes, want %d", got, 2*jobProbesPerRound)
	}
}
