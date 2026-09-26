// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/pkg/proto"
)

func TestSnapshotSwapPercent(t *testing.T) {
	t.Parallel()
	if _, ok := snapshotSwapPercent(proto.MetricsSnapshot{}); ok {
		t.Fatal("an old agent's snapshot (no swap fields) must not report swap")
	}
	if _, ok := snapshotSwapPercent(proto.MetricsSnapshot{SwapReported: true}); ok {
		t.Fatal("no swap configured must render as '-'")
	}
	if pct, ok := snapshotSwapPercent(proto.MetricsSnapshot{SwapReported: true, SwapTotalBytes: 400, SwapUsedBytes: 100}); !ok || pct != 25 {
		t.Fatalf("swap pct = %v %v, want 25", pct, ok)
	}
}

// TestTopUsesSnapshotSwapHonoursGroupAndSkipsAudit: top used to write an audit
// entry per server per frame, run `free -b` on every server every frame even
// though the agent can report swap, and ignore --group.
func TestTopUsesSnapshotSwapHonoursGroupAndSkipsAudit(t *testing.T) {
	behave := map[string]fakeExecBehavior{"web-new": {}, "web-old": {}, "db-01": {}}
	tags := map[string]map[string]string{"web-new": {"role": "web"}, "web-old": {"role": "web"}, "db-01": {"role": "db"}}
	dir, _ := setupExecFanout(t, behave, tags)

	var mu sync.Mutex
	execs := map[string][]string{}
	collected := map[string]int{}
	openAppHook = func(a *core.App) {
		a.ReverseRPCContext = func(ctx context.Context, server string, env proto.Envelope) (proto.Envelope, error) {
			mu.Lock()
			defer mu.Unlock()
			switch env.Action {
			case "metrics.collect":
				collected[server]++
				snap := proto.MetricsSnapshot{Timestamp: time.Now().UTC(), CPUPercent: 12, MemoryPercent: 34, DiskPercent: 56, Load1: 0.5, Load5: 0.4, Load15: 0.3}
				if server == "web-new" {
					snap.SwapReported, snap.SwapTotalBytes, snap.SwapUsedBytes = true, 1000, 250
				}
				return proto.Envelope{Payload: snap}, nil
			case "shell.exec":
				payload, _ := proto.DecodePayload[proto.ExecPayload](env.Payload)
				execs[server] = append(execs[server], payload.Command)
				return proto.Envelope{Payload: proto.ExecResult{Stdout: "              total        used        free\nMem:  100 50 50\nSwap:  1000 500 500\n"}}, nil
			}
			return proto.Envelope{}, nil
		}
	}

	r := runExecFleet(t, dir, "top", "--once", "--group", "role=web")
	if r.err != nil {
		t.Fatalf("top: %v\n%s", r.err, r.combined)
	}
	if strings.Contains(r.stdout, "db-01") || !strings.Contains(r.stdout, "web-new") || !strings.Contains(r.stdout, "web-old") {
		t.Fatalf("--group not honoured:\n%s", r.stdout)
	}
	if strings.Contains(r.stdout, "showing all") || !strings.Contains(r.stdout, `group="role=web"`) {
		t.Fatalf("header still claims tag filtering is pending:\n%s", r.stdout)
	}
	mu.Lock()
	if len(execs["web-new"]) != 0 {
		t.Fatalf("top probed `free` on an agent that reports swap: %v", execs["web-new"])
	}
	if len(execs["web-old"]) != 1 || execs["web-old"][0] != "free -b" {
		t.Fatalf("old agent fallback: execs = %v, want one `free -b`", execs["web-old"])
	}
	if collected["db-01"] != 0 {
		t.Fatal("top collected metrics from a server outside --group")
	}
	mu.Unlock()
	if !strings.Contains(r.stdout, "25.0") || !strings.Contains(r.stdout, "50.0") {
		t.Fatalf("swap columns (25.0 from the snapshot, 50.0 from free) missing:\n%s", r.stdout)
	}

	entries, err := logs.NewAuditLog(dir + "/logs/_audit.log").ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Action == "metrics.collect" {
			t.Fatalf("top wrote a metrics.collect audit entry: %+v", e)
		}
	}

	if r := runExecFleet(t, dir, "top", "--once", "--group", "role=none"); r.err != nil || !strings.Contains(r.stdout, `no servers match --group "role=none"`) {
		t.Fatalf("empty group: %v %q", r.err, r.stdout)
	}
}
