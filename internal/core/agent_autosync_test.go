// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/internal/version"
	"github.com/cenvero/fleet/pkg/proto"
)

// The tests that set version.Version are NOT t.Parallel: it is a package
// global other tests read.

func setControllerVersion(t *testing.T, v string) {
	t.Helper()
	orig := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = orig })
}

// agentSyncTestApp is a reverse-mode controller whose agents answer
// update.apply through a stub. windows names the servers whose update is only
// delivered (no managed restart), like a Windows agent.
type agentSyncTestApp struct {
	*App
	mu    sync.Mutex
	calls []string
}

func newAgentSyncTestApp(t *testing.T, agents map[string]string, windows ...string) *agentSyncTestApp {
	t.Helper()
	configDir := t.TempDir()
	if _, err := Initialize(InitOptions{
		ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeReverse,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatal(err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	app.Notifier = nil
	for name, ver := range agents {
		if err := app.AddServer(ServerRecord{
			Name: name, Address: "127.0.0.1", Mode: transport.ModeReverse,
			Agent:    AgentInstall{Managed: true},
			Observed: ServerObservation{AgentVersion: ver},
		}); err != nil {
			t.Fatal(err)
		}
	}
	delivered := map[string]bool{}
	for _, name := range windows {
		delivered[name] = true
	}
	ta := &agentSyncTestApp{App: app}
	app.ReverseRPCContext = func(_ context.Context, server string, env proto.Envelope) (proto.Envelope, error) {
		if env.Action != "update.apply" {
			return proto.Envelope{}, errors.New("unexpected action " + env.Action)
		}
		ta.mu.Lock()
		ta.calls = append(ta.calls, server)
		ta.mu.Unlock()
		return proto.Envelope{Payload: proto.UpdateApplyResult{
			Channel: "stable", CurrentVersion: agents[server], Version: version.Version,
			Applied: true, SHA256Verified: true, SignatureVerified: true,
			RestartScheduled: !delivered[server],
		}}, nil
	}
	return ta
}

func (ta *agentSyncTestApp) takeCalls() []string {
	ta.mu.Lock()
	defer ta.mu.Unlock()
	out := ta.calls
	ta.calls = nil
	sort.Strings(out)
	return out
}

func TestClaimAgentAutoSyncOncePerInterval(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t0 := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)

	if !ClaimAgentAutoSync(dir, "cli", t0) {
		t.Fatal("first claim should succeed")
	}
	if ClaimAgentAutoSync(dir, "cli", t0.Add(59*time.Minute)) {
		t.Fatal("claim within the interval should fail")
	}
	if !ClaimAgentAutoSync(dir, "daemon", t0.Add(61*time.Minute)) {
		t.Fatal("claim after the interval should succeed")
	}
	if got := ReadAgentSyncState(dir).LastTrigger; got != "daemon" {
		t.Fatalf("LastTrigger = %q, want daemon", got)
	}

	// The clock went back two hours: the recorded start is in the future, which
	// must not block syncing until the clock catches up.
	if !ClaimAgentAutoSync(dir, "cli", t0.Add(-time.Hour)) {
		t.Fatal("claim with a start far in the future should succeed")
	}
}

func TestClaimAgentAutoSyncExactlyOneWinner(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now()
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ClaimAgentAutoSync(dir, "cli", now) {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := wins.Load(); got != 1 {
		t.Fatalf("%d concurrent claims won, want exactly 1", got)
	}
}

func TestRunAgentSyncAutoTouchesOnlyOlderAgents(t *testing.T) {
	setControllerVersion(t, "v2.5.1")
	ta := newAgentSyncTestApp(t, map[string]string{
		"old":     "v2.5.0",
		"old-win": "v2.5.0",
		"current": "v2.5.1",
		"newer":   "v2.6.0",
		"never":   "",
	}, "old-win")

	result, err := ta.RunAgentSync(context.Background(), nil, AgentSyncOptions{Trigger: "cli", Auto: true})
	if err != nil {
		t.Fatalf("RunAgentSync: %v", err)
	}
	if got := ta.takeCalls(); len(got) != 2 || got[0] != "old" || got[1] != "old-win" {
		t.Fatalf("updated %v, want [old old-win]", got)
	}
	if result.Synced != 2 || result.Failed != 0 {
		t.Fatalf("result = %+v", result)
	}
	state := ReadAgentSyncState(ta.ConfigDir)
	if state.Pending["old-win"] != "v2.5.1" || len(state.Pending) != 1 {
		t.Fatalf("pending = %v, want only old-win awaiting v2.5.1", state.Pending)
	}
	if state.LastSynced != 2 || state.LastFinishedAt.IsZero() {
		t.Fatalf("state = %+v", state)
	}

	// Next hour: "old" now reports the new version, and old-win's delivered
	// update is still waiting for a restart, so nothing is re-sent.
	result, err = ta.RunAgentSync(context.Background(), nil, AgentSyncOptions{Trigger: "cli", Auto: true})
	if err != nil {
		t.Fatalf("second RunAgentSync: %v", err)
	}
	if got := ta.takeCalls(); len(got) != 0 {
		t.Fatalf("second run updated %v, want nothing", got)
	}
	if got := ReadAgentSyncState(ta.ConfigDir).LastSkipped; got != 1 {
		t.Fatalf("LastSkipped = %d, want 1 (old-win)", got)
	}

	// A manual run still re-delivers it.
	if _, err := ta.RunAgentSync(context.Background(), []string{"old-win"}, AgentSyncOptions{Trigger: "manual"}); err != nil {
		t.Fatalf("manual RunAgentSync: %v", err)
	}
	if got := ta.takeCalls(); len(got) != 1 || got[0] != "old-win" {
		t.Fatalf("manual run updated %v, want [old-win]", got)
	}

	// Once old-win reconnects on the new version it is no longer pending.
	server, _ := ta.GetServer("old-win")
	server.Observed.AgentVersion = "v2.5.1"
	if err := ta.SaveServer(server); err != nil {
		t.Fatal(err)
	}
	if _, err := ta.RunAgentSync(context.Background(), nil, AgentSyncOptions{Trigger: "daemon", Auto: true}); err != nil {
		t.Fatal(err)
	}
	if p := ReadAgentSyncState(ta.ConfigDir).Pending; len(p) != 0 {
		t.Fatalf("pending = %v after activation, want none", p)
	}
}

func TestRunAgentSyncAutoRespectsOff(t *testing.T) {
	setControllerVersion(t, "v2.5.1")
	ta := newAgentSyncTestApp(t, map[string]string{"old": "v2.5.0"})
	if err := SetAgentAutoSync(ta.ConfigDir, false); err != nil {
		t.Fatal(err)
	}
	if _, err := ta.RunAgentSync(context.Background(), nil, AgentSyncOptions{Trigger: "cli", Auto: true}); !errors.Is(err, ErrAgentAutoSyncOff) {
		t.Fatalf("err = %v, want ErrAgentAutoSyncOff", err)
	}
	if got := ta.takeCalls(); len(got) != 0 {
		t.Fatalf("updated %v while auto-sync is off", got)
	}
	// Turning it off never blocks an explicit sync.
	if _, err := ta.RunAgentSync(context.Background(), nil, AgentSyncOptions{Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	if got := ta.takeCalls(); len(got) != 1 {
		t.Fatalf("manual run updated %v, want [old]", got)
	}
}

func TestRunAgentSyncNeverOverlaps(t *testing.T) {
	setControllerVersion(t, "v2.5.1")
	ta := newAgentSyncTestApp(t, map[string]string{"old": "v2.5.0"})
	release, err := acquireAgentSyncLock(context.Background(), ta.ConfigDir, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !AgentSyncRunning(ta.ConfigDir) {
		t.Fatal("AgentSyncRunning = false while the lock is held")
	}
	if _, err := ta.RunAgentSync(context.Background(), nil, AgentSyncOptions{Trigger: "cli", Auto: true}); !errors.Is(err, ErrAgentSyncBusy) {
		t.Fatalf("err = %v, want ErrAgentSyncBusy", err)
	}

	var waited atomic.Bool
	done := make(chan error, 1)
	go func() {
		_, err := ta.RunAgentSync(context.Background(), nil, AgentSyncOptions{
			Trigger: "manual", Wait: true, OnWait: func() { waited.Store(true) },
		})
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if got := ta.takeCalls(); len(got) != 0 {
		t.Fatalf("a waiting run updated %v before the lock was free", got)
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("waiting run: %v", err)
	}
	if !waited.Load() {
		t.Fatal("OnWait was not called")
	}
	if got := ta.takeCalls(); len(got) != 1 {
		t.Fatalf("updated %v after the lock was free, want [old]", got)
	}
	// A manual full-fleet run restarts the automatic schedule.
	if AgentAutoSyncDue(ReadAgentSyncState(ta.ConfigDir), time.Now()) {
		t.Fatal("automatic run still due right after a manual full run")
	}
}

func TestQueueConnectedAgentSyncOnlyInDaemon(t *testing.T) {
	setControllerVersion(t, "v2.5.1")
	ta := newAgentSyncTestApp(t, nil)
	if ta.queueConnectedAgentSync("web", "v2.5.0") {
		t.Fatal("queued outside a daemon")
	}
	q := &agentConnectSyncQueue{queued: map[string]struct{}{}, lastQueue: map[string]time.Time{}, kick: make(chan struct{}, 1)}
	ta.agentSyncQ = q
	if ta.queueConnectedAgentSync("web", "v2.5.1") || ta.queueConnectedAgentSync("web", "v2.6.0") || ta.queueConnectedAgentSync("web", "") {
		t.Fatal("queued an agent that is not older than the controller")
	}
	if !ta.queueConnectedAgentSync("web", "v2.5.0") {
		t.Fatal("did not queue an older agent")
	}
	delete(q.queued, "web")
	if !ta.queueConnectedAgentSync("web", "v2.5.0") {
		t.Fatal("a reconnect within the hour should still be reported as handled")
	}
	if _, again := q.queued["web"]; again {
		t.Fatal("re-queued an agent within the hour")
	}
}

func TestDaemonAgentAutoSyncLoop(t *testing.T) {
	setControllerVersion(t, "v2.5.1")
	origDelay, origPoll, origDebounce := agentAutoSyncStartDelay, agentAutoSyncPollInterval, agentConnectSyncDebounce
	agentAutoSyncStartDelay, agentAutoSyncPollInterval, agentConnectSyncDebounce = 50*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() {
		agentAutoSyncStartDelay, agentAutoSyncPollInterval, agentConnectSyncDebounce = origDelay, origPoll, origDebounce
	})

	ta := newAgentSyncTestApp(t, map[string]string{"old": "v2.5.0", "current": "v2.5.1"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := ta.startAgentAutoSync(ctx)
	defer stop()

	waitForCalls := func(want int) []string {
		t.Helper()
		var got []string
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			got = append(got, ta.takeCalls()...)
			if len(got) >= want {
				return got
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("got calls %v, want %d", got, want)
		return nil
	}

	// The scheduled run syncs the out-of-date agent once…
	if got := waitForCalls(1); got[0] != "old" {
		t.Fatalf("scheduled run updated %v, want [old]", got)
	}
	time.Sleep(200 * time.Millisecond) // several polls: not due again for an hour
	if got := ta.takeCalls(); len(got) != 0 {
		t.Fatalf("polls re-ran the sync: %v", got)
	}

	// …and an agent that connects out of date is synced right away.
	server, _ := ta.GetServer("current")
	server.Observed.AgentVersion = "v2.4.0"
	if err := ta.SaveServer(server); err != nil {
		t.Fatal(err)
	}
	if !ta.queueConnectedAgentSync("current", "v2.4.0") {
		t.Fatal("daemon did not take the connected agent")
	}
	if got := waitForCalls(1); got[0] != "current" {
		t.Fatalf("connect run updated %v, want [current]", got)
	}
}
