// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
)

// These tests run the helper in-process; none of them starts a real helper
// process. They shorten package timing variables, so they are not parallel.

func newHelperTestConfig(t *testing.T) string {
	t.Helper()
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeDirect,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	return configDir
}

func fastHelper(t *testing.T) {
	t.Helper()
	oldTick, oldClaim, oldBeat := helperTick, helperClaimWait, helperHeartbeat
	helperTick, helperClaimWait, helperHeartbeat = 10*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { helperTick, helperClaimWait, helperHeartbeat = oldTick, oldClaim, oldBeat })
}

// fakeExecutable is a file standing in for the fleet binary.
func fakeExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fleet")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runHelperInBackground starts RunHelper and returns a func that stops it
// and returns its result.
func runHelperInBackground(t *testing.T, configDir string, opts HelperOptions) (stop func() error, done <-chan error) {
	t.Helper()
	if opts.Executable == "" {
		opts.Executable = fakeExecutable(t)
	}
	if opts.Log == nil {
		opts.Log = testWriter{t}
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	finished := make(chan struct{})
	var result error
	go func() {
		result = RunHelper(ctx, configDir, opts)
		ch <- result
		close(finished)
	}()
	waitFor(t, "the helper to take its lock", func() bool { return HelperRunning(configDir) })
	stop = func() error {
		cancel()
		select {
		case <-finished:
			return result
		case <-time.After(10 * time.Second):
			return errors.New("the helper did not stop")
		}
	}
	t.Cleanup(func() { _ = stop() })
	return stop, ch
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestClaimHelperSingleInstanceUnderConcurrentStarts(t *testing.T) {
	fastHelper(t)
	configDir := t.TempDir()
	const starters = 16
	var wg sync.WaitGroup
	var won atomic.Int32
	instances := make(chan *HelperInstance, starters)
	start := make(chan struct{})
	for range starters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			inst, err := ClaimHelper(configDir)
			switch {
			case err == nil:
				won.Add(1)
				instances <- inst
			case !errors.Is(err, ErrHelperRunning):
				t.Errorf("ClaimHelper: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(instances)
	if got := won.Load(); got != 1 {
		t.Fatalf("%d helpers claimed the config dir at once, want exactly 1", got)
	}
	inst := <-instances
	if !HelperRunning(configDir) {
		t.Fatal("HelperRunning = false while a helper holds the lock")
	}
	if pid, err := ReadHelperPID(configDir); err != nil || pid != os.Getpid() {
		t.Fatalf("pid file = %d, %v", pid, err)
	}
	inst.Release()
	if HelperRunning(configDir) {
		t.Fatal("HelperRunning = true after Release")
	}
	if _, err := os.Stat(HelperPIDPath(configDir)); !os.IsNotExist(err) {
		t.Fatalf("pid file left behind: %v", err)
	}
	// And the next helper can claim it.
	again, err := ClaimHelper(configDir)
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	again.Release()

	// Two whole helpers started together: one runs, the other exits at once.
	configDir = newHelperTestConfig(t)
	stop, _ := runHelperInBackground(t, configDir, HelperOptions{Tasks: []HelperTask{}})
	if err := RunHelper(context.Background(), configDir, HelperOptions{Tasks: []HelperTask{}, Executable: fakeExecutable(t)}); !errors.Is(err, ErrHelperRunning) {
		t.Fatalf("second RunHelper = %v, want ErrHelperRunning", err)
	}
	if err := stop(); err != nil {
		t.Fatalf("RunHelper after stop = %v", err)
	}
}

func TestEnsureHelperLaunchesOnceAndRecordsFailures(t *testing.T) {
	fastHelper(t)
	configDir := newHelperTestConfig(t)
	var launches atomic.Int32
	launch := func(string) error { launches.Add(1); return nil }

	// Many commands at once find no helper: only one launches it.
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := EnsureHelper(configDir, launch); err != nil {
				t.Errorf("EnsureHelper: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := launches.Load(); got != 1 {
		t.Fatalf("launched %d helpers, want 1", got)
	}

	// With a helper running, a command launches nothing (and writes nothing).
	inst, err := ClaimHelper(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(HelperStatusPath(configDir)); err != nil {
		t.Fatal(err)
	}
	if started, err := EnsureHelper(configDir, launch); started || err != nil || launches.Load() != 1 {
		t.Fatalf("EnsureHelper with a helper running: started=%v err=%v launches=%d", started, err, launches.Load())
	}
	if _, err := os.Stat(HelperStatusPath(configDir)); !os.IsNotExist(err) {
		t.Fatal("EnsureHelper wrote the status file although the helper was running")
	}
	inst.Release()

	// A failure to start is recorded for `fleet helper status` / `fleet errors`.
	failing := func(string) error { return errors.New("exec format error") }
	if _, err := launchHelper(configDir, failing, true); err == nil {
		t.Fatal("launch failure not returned")
	}
	status, err := ReadHelperStatus(configDir)
	if err != nil || status.LastLaunchError != "exec format error" || status.LastLaunchErrorAt.IsZero() {
		t.Fatalf("launch failure not recorded: %+v, %v", status, err)
	}
}

func TestHelperRunsRegisteredTasksAndRecoversPanics(t *testing.T) {
	fastHelper(t)
	configDir := newHelperTestConfig(t)
	var okRuns, panicRuns, idleChecks atomic.Int32
	tasks := []HelperTask{
		{Name: "steady", Interval: 10 * time.Millisecond, Run: func(ctx context.Context, app *App) error {
			if app == nil || app.ConfigDir != configDir {
				return errors.New("task got the wrong App")
			}
			okRuns.Add(1)
			helperSay(ctx, "all good")
			return nil
		}},
		{Name: "broken", Interval: 10 * time.Millisecond, Run: func(context.Context, *App) error {
			return errors.New("server exploded")
		}},
		{Name: "panicky", Interval: 10 * time.Millisecond, Run: func(context.Context, *App) error {
			panicRuns.Add(1)
			panic("boom")
		}},
		{Name: "idle", Interval: 10 * time.Millisecond,
			Pending: func(string, time.Time) bool { idleChecks.Add(1); return false },
			Run: func(context.Context, *App) error {
				t.Error("ran a task with nothing pending")
				return nil
			}},
		{Name: "later", Interval: time.Hour, StartDelay: time.Hour, Run: func(context.Context, *App) error {
			t.Error("ran a task before its start delay")
			return nil
		}},
	}
	stop, _ := runHelperInBackground(t, configDir, HelperOptions{Tasks: tasks})
	waitFor(t, "tasks to keep running after a panic", func() bool {
		return okRuns.Load() >= 3 && panicRuns.Load() >= 3 && idleChecks.Load() >= 3
	})
	waitFor(t, "the status file to record every task", func() bool {
		st, _ := ReadHelperStatus(configDir)
		return st.Tasks["steady"].LastResult == HelperResultOK && st.Tasks["broken"].LastResult == HelperResultError &&
			st.Tasks["panicky"].LastResult == HelperResultError
	})
	if err := stop(); err != nil {
		t.Fatalf("RunHelper = %v, want nil after a stop", err)
	}

	st, err := ReadHelperStatus(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Tasks["steady"]; got.LastMessage != "all good" || got.LastError != "" || got.LastRunAt.IsZero() || got.Interval != "10ms" {
		t.Errorf("steady = %+v", got)
	}
	if got := st.Tasks["broken"]; got.LastError != "server exploded" || got.LastErrorAt.IsZero() {
		t.Errorf("broken = %+v", got)
	}
	if got := st.Tasks["panicky"]; !strings.Contains(got.LastError, "panic: boom") {
		t.Errorf("panicky = %+v", got)
	}
	if got := st.Tasks["idle"]; !got.LastRunAt.IsZero() || got.LastResult != "" {
		t.Errorf("idle task recorded a run: %+v", got)
	}
	if got := st.Tasks["later"]; !got.LastRunAt.IsZero() || got.NextRunAt.Before(time.Now().Add(50*time.Minute)) {
		t.Errorf("later = %+v", got)
	}
	if st.StopReason != "stopped" || st.StoppedAt.IsZero() {
		t.Errorf("stop not recorded: %q at %v", st.StopReason, st.StoppedAt)
	}
	if HelperRunning(configDir) {
		t.Error("lock still held after the helper returned")
	}
}

func TestHelperStatusFileFormat(t *testing.T) {
	fastHelper(t)
	configDir := newHelperTestConfig(t)
	if _, err := launchHelper(configDir, func(string) error { return nil }, true); err != nil {
		t.Fatal(err)
	}
	ran := make(chan struct{})
	var once sync.Once
	stop, _ := runHelperInBackground(t, configDir, HelperOptions{Tasks: []HelperTask{{
		Name: "jobs", Interval: time.Hour, Run: func(ctx context.Context, _ *App) error {
			helperSay(ctx, "watching 0 jobs")
			once.Do(func() { close(ran) })
			return nil
		},
	}}})
	<-ran
	waitFor(t, "the run to be recorded", func() bool {
		st, _ := ReadHelperStatus(configDir)
		return st.Tasks["jobs"].LastResult == HelperResultOK
	})

	info, err := os.Stat(HelperStatusPath(configDir))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("status file mode %v, want 0600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(HelperStatusPath(configDir))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("status file is not JSON: %v\n%s", err, raw)
	}
	for _, key := range []string{"format", "pid", "started_at", "version", "updated_at", "last_launch_at", "tasks"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("status file has no %q:\n%s", key, raw)
		}
	}
	if doc["format"] != float64(1) || doc["pid"] != float64(os.Getpid()) {
		t.Errorf("format/pid = %v/%v", doc["format"], doc["pid"])
	}
	if _, ok := doc["stopped_at"]; ok {
		t.Errorf("a running helper's status has stopped_at:\n%s", raw)
	}
	task, _ := doc["tasks"].(map[string]any)["jobs"].(map[string]any)
	for _, key := range []string{"interval", "last_run_at", "next_run_at", "last_result", "last_message"} {
		if _, ok := task[key]; !ok {
			t.Errorf("task entry has no %q:\n%s", key, raw)
		}
	}
	for _, key := range []string{"last_error", "last_error_at"} {
		if _, ok := task[key]; ok {
			t.Errorf("a task that never failed has %q:\n%s", key, raw)
		}
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	// The next helper keeps the task's history and its schedule.
	before, _ := ReadHelperStatus(configDir)
	stop, _ = runHelperInBackground(t, configDir, HelperOptions{Tasks: []HelperTask{{
		Name: "jobs", Interval: time.Hour, Run: func(context.Context, *App) error {
			t.Error("a restarted helper ran an hourly task again at once")
			return nil
		},
	}}})
	waitFor(t, "the new helper's status", func() bool {
		st, _ := ReadHelperStatus(configDir)
		return st.StoppedAt.IsZero() && st.StartedAt.After(before.StartedAt)
	})
	after, _ := ReadHelperStatus(configDir)
	if got := after.Tasks["jobs"]; got.LastMessage != "watching 0 jobs" || !got.LastRunAt.Equal(before.Tasks["jobs"].LastRunAt) {
		t.Errorf("history lost across a restart: %+v", got)
	}
	if after.LastLaunchAt.IsZero() {
		t.Error("the helper dropped last_launch_at, which launching commands own")
	}
	_ = stop()
}

func TestHelperExitsWhenItsExecutableChanges(t *testing.T) {
	fastHelper(t)
	configDir := newHelperTestConfig(t)
	exe := fakeExecutable(t)
	_, done := runHelperInBackground(t, configDir, HelperOptions{Tasks: []HelperTask{}, Executable: exe})

	// An upgrade writes a new binary over (or renames one onto) the old path.
	replacement := exe + ".new"
	if err := os.WriteFile(replacement, []byte("v2, a different build"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, exe); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrHelperBinaryChanged) {
			t.Fatalf("RunHelper = %v, want ErrHelperBinaryChanged", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the helper kept running after its executable changed")
	}
	if HelperRunning(configDir) {
		t.Fatal("lock still held")
	}
	if st, _ := ReadHelperStatus(configDir); !strings.Contains(st.StopReason, "executable changed") {
		t.Fatalf("stop reason = %q", st.StopReason)
	}
}

func TestHelperExitsWhenTheConfigDirGoes(t *testing.T) {
	fastHelper(t)
	configDir := newHelperTestConfig(t)
	_, done := runHelperInBackground(t, configDir, HelperOptions{Tasks: []HelperTask{}})
	if runtime.GOOS == "windows" {
		// The open lock file keeps the directory; losing config.toml is enough.
		if err := os.Remove(ConfigPath(configDir)); err != nil {
			t.Fatal(err)
		}
	} else if err := os.RemoveAll(configDir); err != nil { // self-uninstall
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrHelperConfigGone) {
			t.Fatalf("RunHelper = %v, want ErrHelperConfigGone", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the helper kept running without a config")
	}
	if runtime.GOOS != "windows" {
		if _, err := os.Stat(configDir); !os.IsNotExist(err) {
			t.Fatalf("the exiting helper recreated the removed config dir: %v", err)
		}
	}
}

func TestStopHelper(t *testing.T) {
	fastHelper(t)
	configDir := t.TempDir()

	// Nothing running: not an error, and a stale pid file goes.
	if err := os.MkdirAll(filepath.Join(configDir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HelperPIDPath(configDir), []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stopped, err := StopHelper(configDir, time.Second); stopped || err != nil {
		t.Fatalf("StopHelper with no helper = %v, %v", stopped, err)
	}
	if _, err := os.Stat(HelperPIDPath(configDir)); !os.IsNotExist(err) {
		t.Fatal("stale pid file kept")
	}

	// The lock is held, but by a process that is not running __helper (this
	// test): it must never be signalled.
	inst, err := ClaimHelper(configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Release()
	stopped, err := StopHelper(configDir, time.Second)
	if runtime.GOOS == "windows" {
		t.Skip("Windows relies on the lock alone")
	}
	if stopped || err == nil || !strings.Contains(err.Error(), "refusing to signal") {
		t.Fatalf("StopHelper on a non-helper process = %v, %v", stopped, err)
	}
}
