// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
)

type fakeExecBehavior struct {
	delay  time.Duration
	exit   int
	stdout string
	stderr string
	err    error
	// killAtDeadline mimics an agent that kills the command at the request
	// deadline and replies (exit -1) just BEFORE the controller's own timer.
	killAtDeadline bool
	timedOutFlag   bool
}

// fakeExecRPC is an in-process agent for reverse-mode servers.
type fakeExecRPC struct {
	mu          sync.Mutex
	behave      map[string]fakeExecBehavior
	inflight    atomic.Int32
	maxInflight atomic.Int32
	calls       atomic.Int32
}

func (f *fakeExecRPC) call(ctx context.Context, server string, env proto.Envelope) (proto.Envelope, error) {
	f.calls.Add(1)
	cur := f.inflight.Add(1)
	defer f.inflight.Add(-1)
	for {
		prev := f.maxInflight.Load()
		if cur <= prev || f.maxInflight.CompareAndSwap(prev, cur) {
			break
		}
	}
	payload, err := proto.DecodePayload[proto.ExecPayload](env.Payload)
	if err != nil {
		return proto.Envelope{}, err
	}
	f.mu.Lock()
	b := f.behave[server]
	f.mu.Unlock()
	if b.killAtDeadline {
		deadline := time.UnixMilli(env.DeadlineUnixMilli) // truncated, like the wire
		select {
		case <-time.After(time.Until(deadline) - 2*time.Millisecond):
		case <-ctx.Done():
			return proto.Envelope{}, ctx.Err()
		}
		return proto.Envelope{Payload: proto.ExecResult{ExitCode: -1, TimedOut: b.timedOutFlag}}, nil
	}
	select {
	case <-time.After(b.delay):
	case <-ctx.Done():
		return proto.Envelope{}, ctx.Err()
	}
	if b.err != nil {
		return proto.Envelope{}, b.err
	}
	return proto.Envelope{Payload: proto.ExecResult{
		Stdout:   fmt.Sprintf("%s ran %q\n%s", server, payload.Command, b.stdout),
		Stderr:   b.stderr,
		ExitCode: b.exit,
	}}, nil
}

// setupExecFanout creates a controller with reverse-mode servers backed by an
// in-process fake agent and returns its config dir.
func setupExecFanout(t *testing.T, behave map[string]fakeExecBehavior, tags map[string]map[string]string) (string, *fakeExecRPC) {
	t.Helper()
	t.Setenv("FLEET_TOKEN", "")
	dir := newServerCommandTestConfig(t)
	app, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name := range behave {
		if err := app.AddServer(core.ServerRecord{Name: name, Address: "127.0.0.1", Port: 1, Mode: transport.ModeReverse}); err != nil {
			t.Fatal(err)
		}
	}
	_ = app.Close()
	tagStore := core.NewTagStore(dir)
	for server, kv := range tags {
		if err := tagStore.SetTags(server, kv); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeExecRPC{behave: behave}
	openAppHook = func(a *core.App) { a.ReverseRPCContext = fake.call }
	t.Cleanup(func() { openAppHook = nil })
	return dir, fake
}

type fleetRun struct {
	stdout, stderr, combined string
	err                      error
	exitCode                 int // from --propagate-exit, -1 if exitProcess was not called
}

// runExecFleet runs the root command in-process twice-free: stdout and stderr are
// captured separately and also into one combined stream (to check interleaving).
func runExecFleet(t *testing.T, dir string, args ...string) fleetRun {
	t.Helper()
	res := fleetRun{exitCode: -1}
	orig := exitProcess
	exitProcess = func(code int) { res.exitCode = code }
	defer func() { exitProcess = orig }()

	var stdout, stderr, combined bytes.Buffer
	root := NewRootCommand()
	root.SetOut(io2(&stdout, &combined))
	root.SetErr(io2(&stderr, &combined))
	root.SetArgs(append([]string{"--config-dir", dir}, args...))
	res.err = root.Execute()
	res.stdout, res.stderr, res.combined = stdout.String(), stderr.String(), combined.String()
	return res
}

type teeWriter struct{ a, b *bytes.Buffer }

func (w teeWriter) Write(p []byte) (int, error) {
	w.a.Write(p)
	w.b.Write(p)
	return len(p), nil
}

func io2(a, b *bytes.Buffer) teeWriter { return teeWriter{a: a, b: b} }

func eightServers() map[string]fakeExecBehavior {
	behave := map[string]fakeExecBehavior{}
	for i := 1; i <= 8; i++ {
		// Later servers finish FIRST, so a naive concurrent printer would
		// reorder the output.
		behave[fmt.Sprintf("srv-%02d", i)] = fakeExecBehavior{
			delay:  time.Duration(9-i) * 25 * time.Millisecond,
			stdout: fmt.Sprintf("out-%d\n", i),
			stderr: fmt.Sprintf("err-%d\n", i),
		}
	}
	return behave
}

// TestExecFanoutParallelOutputMatchesSequential: running on all servers
// concurrently must print byte-for-byte what one-at-a-time printed (including
// the stdout/stderr interleaving), while taking about as long as the slowest
// server instead of the sum.
func TestExecFanoutParallelOutputMatchesSequential(t *testing.T) {
	dir, fake := setupExecFanout(t, eightServers(), nil)

	start := time.Now()
	seq := runExecFleet(t, dir, "exec", "--all", "--parallel", "1", "--", "uptime")
	seqDur := time.Since(start)
	if seq.err != nil {
		t.Fatalf("sequential exec: %v\n%s", seq.err, seq.combined)
	}
	if fake.maxInflight.Load() != 1 {
		t.Fatalf("--parallel 1 ran %d servers at once", fake.maxInflight.Load())
	}

	fake.maxInflight.Store(0)
	start = time.Now()
	par := runExecFleet(t, dir, "exec", "--all", "--", "uptime")
	parDur := time.Since(start)
	if par.err != nil {
		t.Fatalf("parallel exec: %v\n%s", par.err, par.combined)
	}
	if par.combined != seq.combined || par.stdout != seq.stdout || par.stderr != seq.stderr {
		t.Fatalf("parallel output differs from sequential:\n--- sequential\n%s\n--- parallel\n%s", seq.combined, par.combined)
	}
	if !strings.HasPrefix(par.stdout, "=== srv-01 [exit 0] ===\nsrv-01 ran \"uptime\"\nout-1\n=== srv-02 [exit 0] ===") {
		t.Fatalf("unexpected block format/order:\n%s", par.stdout)
	}
	if fake.maxInflight.Load() < 2 {
		t.Fatalf("default fan-out did not run servers concurrently (max in flight %d)", fake.maxInflight.Load())
	}
	// Sum of delays is 900ms; the slowest single server is 200ms.
	if parDur >= seqDur || parDur > 700*time.Millisecond {
		t.Fatalf("parallel fan-out took %s (sequential %s)", parDur, seqDur)
	}

	// --json keeps target order too.
	js := runExecFleet(t, dir, "exec", "--all", "--json", "--", "uptime")
	var arr []execJSON
	if err := json.Unmarshal([]byte(js.stdout), &arr); err != nil {
		t.Fatalf("json output invalid: %v\n%s", err, js.stdout)
	}
	for i, r := range arr {
		if want := fmt.Sprintf("srv-%02d", i+1); r.Server != want || r.Status != "" {
			t.Fatalf("json[%d] = %+v, want server %s with no status", i, r, want)
		}
	}
}

// TestExecFanoutParallelIsBounded: --parallel caps the in-flight calls.
func TestExecFanoutParallelIsBounded(t *testing.T) {
	dir, fake := setupExecFanout(t, eightServers(), nil)
	if r := runExecFleet(t, dir, "exec", "--all", "--parallel", "3", "--", "uptime"); r.err != nil {
		t.Fatal(r.err)
	}
	if got := fake.maxInflight.Load(); got > 3 || got < 2 {
		t.Fatalf("max in flight = %d, want 2..3", got)
	}
	if r := runExecFleet(t, dir, "exec", "--all", "--parallel", "0", "--", "uptime"); r.err == nil {
		t.Fatal("--parallel 0 accepted")
	}
}

// TestExecFanoutExitStatus: a fan-out used to exit 0 whatever happened (QA bug a).
func TestExecFanoutExitStatus(t *testing.T) {
	behave := map[string]fakeExecBehavior{
		"a-ok":   {},
		"b-four": {exit: 4},
		"c-five": {exit: 5},
		"d-down": {err: errors.New("dial tcp: connection refused")},
	}
	dir, _ := setupExecFanout(t, behave, nil)

	r := runExecFleet(t, dir, "exec", "--all", "--", "false")
	if r.err == nil || !strings.Contains(r.err.Error(), "3 of 4 server(s): b-four, c-five, d-down") {
		t.Fatalf("human fan-out with failures: err = %v", r.err)
	}
	if !strings.Contains(r.stdout, "=== b-four [exit 4] ===") || !strings.Contains(r.stdout, "=== d-down [unreachable] ===") {
		t.Fatalf("headers changed:\n%s", r.stdout)
	}

	r = runExecFleet(t, dir, "exec", "--all", "--propagate-exit", "--", "false")
	if r.exitCode != 4 {
		t.Fatalf("--propagate-exit exit code = %d, want 4 (first non-zero remote exit in target order)", r.exitCode)
	}

	// JSON never errors on remote failures (as for one server), but still
	// propagates the first remote exit code when asked.
	r = runExecFleet(t, dir, "exec", "--all", "--json", "--", "false")
	if r.err != nil || r.exitCode != -1 {
		t.Fatalf("--json fan-out: err=%v code=%d", r.err, r.exitCode)
	}
	r = runExecFleet(t, dir, "exec", "--all", "--json", "--propagate-exit", "--", "false")
	if r.exitCode != 4 {
		t.Fatalf("--json --propagate-exit code = %d, want 4", r.exitCode)
	}

	// Only transport failures, no remote exit codes: propagate falls back to 1.
	dir2, _ := setupExecFanout(t, map[string]fakeExecBehavior{"x": {}, "y": {err: errors.New("dial: refused")}}, nil)
	r = runExecFleet(t, dir2, "exec", "--all", "--propagate-exit", "--", "true")
	if r.exitCode != -1 || r.err == nil {
		t.Fatalf("propagate with only a transport failure: code=%d err=%v, want error", r.exitCode, r.err)
	}

	// All good: exit 0.
	dir3, _ := setupExecFanout(t, map[string]fakeExecBehavior{"x": {}, "y": {}}, nil)
	if r = runExecFleet(t, dir3, "exec", "--all", "--propagate-exit", "--", "true"); r.err != nil || r.exitCode != -1 {
		t.Fatalf("all-ok fan-out: code=%d err=%v", r.exitCode, r.err)
	}
}

// TestExecFanoutJSONIncludesBlockedTargets: policy-blocked servers used to vanish
// from the JSON array (printing []) while the command exited 0.
func TestExecFanoutJSONIncludesBlockedTargets(t *testing.T) {
	dir, fake := setupExecFanout(t, map[string]fakeExecBehavior{"a": {}, "b": {}}, nil)
	policy, err := core.NewCmdPolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.SetDenyPatterns([]string{"rm -rf *"}); err != nil {
		t.Fatal(err)
	}
	r := runExecFleet(t, dir, "exec", "--all", "--json", "--", "rm -rf /tmp/x")
	if r.err == nil {
		t.Fatal("fan-out where policy blocks every server exited 0")
	}
	var arr []execJSON
	if err := json.Unmarshal([]byte(r.stdout), &arr); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%q", err, r.stdout)
	}
	if len(arr) != 2 || arr[0].Server != "a" || arr[0].Status != execStatusBlocked || !strings.Contains(arr[0].Error, "deny pattern") {
		t.Fatalf("blocked targets missing from JSON: %+v", arr)
	}
	if fake.calls.Load() != 0 {
		t.Fatalf("a blocked command reached an agent %d times", fake.calls.Load())
	}
	// Human mode: non-zero too.
	if r := runExecFleet(t, dir, "exec", "--all", "--", "rm -rf /tmp/x"); r.err == nil {
		t.Fatal("human fan-out with every server blocked exited 0")
	}
}

// TestExecJSONDryRunIsPureJSON: --json --dry-run printed "would run" lines on
// stdout before the JSON (QA bug c).
func TestExecJSONDryRunIsPureJSON(t *testing.T) {
	dir, fake := setupExecFanout(t, map[string]fakeExecBehavior{"a": {}, "b": {}}, nil)
	r := runExecFleet(t, dir, "exec", "--all", "--json", "--dry-run", "--", "uptime")
	if r.err != nil {
		t.Fatal(r.err)
	}
	var arr []execJSON
	if err := json.Unmarshal([]byte(r.stdout), &arr); err != nil {
		t.Fatalf("fan-out dry-run stdout is not JSON: %v\n%q", err, r.stdout)
	}
	if len(arr) != 2 || arr[1].Status != execStatusDryRun || arr[1].Command != "uptime" {
		t.Fatalf("dry-run entries = %+v", arr)
	}
	if !strings.Contains(r.stderr, "would run: uptime [a]") {
		t.Fatalf("dry-run note not on stderr: %q", r.stderr)
	}

	r = runExecFleet(t, dir, "exec", "a", "--json", "--dry-run", "--", "uptime")
	var one execJSON
	if err := json.Unmarshal([]byte(r.stdout), &one); err != nil || one.Status != execStatusDryRun {
		t.Fatalf("single dry-run stdout = %q (%v)", r.stdout, err)
	}
	if fake.calls.Load() != 0 {
		t.Fatal("dry-run executed a command")
	}

	// Human mode keeps printing the note on stdout.
	r = runExecFleet(t, dir, "exec", "--all", "--dry-run", "--", "uptime")
	if r.stdout != "would run: uptime [a]\nwould run: uptime [b]\n" {
		t.Fatalf("human dry-run output changed: %q", r.stdout)
	}
}

// TestExecGroupWithoutMatchesFails: --group matching nothing silently exited 0
// (QA bug d).
func TestExecGroupWithoutMatchesFails(t *testing.T) {
	dir, _ := setupExecFanout(t, map[string]fakeExecBehavior{"a": {}}, map[string]map[string]string{"a": {"role": "web"}})
	r := runExecFleet(t, dir, "exec", "--group", "role=db", "--", "uptime")
	if r.err == nil || !strings.Contains(r.err.Error(), `no servers match --group "role=db"`) {
		t.Fatalf("empty --group: err = %v", r.err)
	}
	if r := runExecFleet(t, dir, "exec", "--group", "role=web", "--", "uptime"); r.err != nil || !strings.Contains(r.stdout, "=== a [exit 0] ===") {
		t.Fatalf("matching --group failed: %v %q", r.err, r.stdout)
	}
}

// TestExecTimeoutRaceReportsTimedOut: an agent that kills the command at the
// (millisecond-truncated) deadline and replies a hair before the controller's
// timer used to surface as {"exit_code":-1,"timed_out":false} (QA bug b).
func TestExecTimeoutRaceReportsTimedOut(t *testing.T) {
	for _, flag := range []bool{false, true} { // old agent (bare -1) and new agent (timed_out)
		dir, _ := setupExecFanout(t, map[string]fakeExecBehavior{"a": {killAtDeadline: true, timedOutFlag: flag}}, nil)
		for i := 0; i < 3; i++ {
			r := runExecFleet(t, dir, "exec", "a", "--json", "--timeout", "300ms", "--", "sleep 5")
			var j execJSON
			if err := json.Unmarshal([]byte(r.stdout), &j); err != nil {
				t.Fatalf("stdout %q: %v", r.stdout, err)
			}
			if !j.TimedOut {
				t.Fatalf("agentFlag=%v: deadline kill reported as %+v, want timed_out", flag, j)
			}
		}
	}
}

func TestExecTimedOutClassification(t *testing.T) {
	parent := context.Background()
	ctx, cancel := context.WithTimeout(parent, time.Hour) // not expired
	defer cancel()
	deadline := time.Now()
	cases := []struct {
		name     string
		returned time.Time
		r        proto.ExecResult
		err      error
		want     bool
	}{
		{"agent flag", deadline.Add(-time.Minute), proto.ExecResult{ExitCode: -1, TimedOut: true}, nil, true},
		{"old agent kill at deadline", deadline.Add(-5 * time.Millisecond), proto.ExecResult{ExitCode: -1}, nil, true},
		{"signal exit long before deadline", deadline.Add(-30 * time.Second), proto.ExecResult{ExitCode: -1}, nil, false},
		{"normal exit at deadline", deadline.Add(-time.Millisecond), proto.ExecResult{ExitCode: 0}, nil, false},
		{"transport gave up at deadline", deadline.Add(-3 * time.Millisecond), proto.ExecResult{}, errors.New("reverse_call_failed: i/o timeout"), true},
		{"fast refusal", deadline.Add(-29 * time.Second), proto.ExecResult{}, errors.New("connection refused"), false},
		{"ctx deadline error", deadline.Add(-time.Minute), proto.ExecResult{}, fmt.Errorf("x: %w", context.DeadlineExceeded), true},
	}
	for _, c := range cases {
		if got := execTimedOut(ctx, parent, 30*time.Second, deadline, c.returned, c.r, c.err); got != c.want {
			t.Errorf("%s: execTimedOut = %v, want %v", c.name, got, c.want)
		}
	}
	// Short timeouts shrink the window: a refusal 50ms into a 300ms budget is not a timeout.
	if execTimedOut(ctx, parent, 300*time.Millisecond, deadline, deadline.Add(-250*time.Millisecond), proto.ExecResult{}, errors.New("refused")) {
		t.Error("fast failure within a short timeout classified as a timeout")
	}
	// An operator interrupt is never a timeout.
	cancelled, stop := context.WithCancel(parent)
	stop()
	if execTimedOut(ctx, cancelled, 30*time.Second, deadline, deadline, proto.ExecResult{ExitCode: -1}, nil) {
		t.Error("interrupt classified as timeout")
	}
	if !agentDefaultLimitHit(proto.ExecResult{ExitCode: -1}, agentDefaultExecTimeout) || agentDefaultLimitHit(proto.ExecResult{ExitCode: -1}, time.Second) {
		t.Error("agentDefaultLimitHit misclassifies")
	}
}
