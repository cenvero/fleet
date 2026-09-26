// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/pkg/proto"
)

// TestAgentUpdateCanaryIgnoresHostConditions: the canary gate used the full
// health result, so a host without swap (or with a full disk, high load, a
// pending reboot) aborted the rollout even when the agent was already up to
// date. It now gates on the agent coming back on the expected version; host
// conditions are printed, and block only with --strict-health.
func TestAgentUpdateCanaryIgnoresHostConditions(t *testing.T) {
	origWait, origPoll := canaryWaitTimeout, canaryPollInterval
	canaryWaitTimeout, canaryPollInterval = 400*time.Millisecond, 50*time.Millisecond
	defer func() { canaryWaitTimeout, canaryPollInterval = origWait, origPoll }()

	dir, _ := setupExecFanout(t, map[string]fakeExecBehavior{"a-canary": {}, "b-rest": {}}, nil)
	app, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a-canary", "b-rest"} {
		rec, _ := app.GetServer(name)
		rec.Observed.AgentVersion = "1.2.3"
		if err := app.SaveServer(rec); err != nil {
			t.Fatal(err)
		}
	}
	_ = app.Close()

	var liveVersion atomic.Value
	liveVersion.Store("1.2.3")
	openAppHook = func(a *core.App) {
		a.ReverseStatusLookup = func(name string) (core.ReverseSessionInfo, error) {
			return core.ReverseSessionInfo{Server: name, Connected: true, Hello: proto.HelloPayload{AgentVersion: liveVersion.Load().(string)}}, nil
		}
		a.ReverseRPCContext = func(ctx context.Context, server string, env proto.Envelope) (proto.Envelope, error) {
			p, _ := proto.DecodePayload[proto.ExecPayload](env.Payload)
			if p.Command == "true" {
				return proto.Envelope{Payload: proto.ExecResult{}}, nil
			}
			// Health probe: no swap, disk 97%, reboot pending.
			out := fmt.Sprintf("EPOCH=%d\nSWAPKB=0\nDISK=97\nREBOOT=1\nLOAD1=0.1\nNPROC=4\n", time.Now().Unix())
			return proto.Envelope{Payload: proto.ExecResult{Stdout: out}}, nil
		}
	}

	r := runExecFleet(t, dir, "agent", "update", "--canary", "1")
	if r.err != nil {
		t.Fatalf("host conditions aborted an up-to-date rollout: %v\n%s", r.err, r.combined)
	}
	if !strings.Contains(r.stdout, "canary OK") || !strings.Contains(r.stdout, "rollout complete: 2 server(s) updated") || !strings.Contains(r.stdout, "note: host checks reported problems on a-canary") {
		t.Fatalf("unexpected output:\n%s", r.stdout)
	}

	if r := runExecFleet(t, dir, "agent", "update", "--canary", "1", "--strict-health"); r.err == nil || !strings.Contains(r.err.Error(), "--strict-health") {
		t.Fatalf("--strict-health did not gate on host checks: %v", r.err)
	}

	// An agent that does not come back on the expected version still aborts.
	liveVersion.Store("1.2.2")
	r = runExecFleet(t, dir, "agent", "update", "--canary", "1")
	if r.err == nil || !strings.Contains(r.err.Error(), "did not come back on the expected agent version: a-canary") {
		t.Fatalf("version mismatch not caught: %v\n%s", r.err, r.combined)
	}
	if strings.Contains(r.stdout, "rolling out to remaining") {
		t.Fatal("rollout continued past a failed canary")
	}
}
