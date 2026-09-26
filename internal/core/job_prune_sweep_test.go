// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

// TestSweepOrphanJobLogsRotatesAndHonorsBudget: the orphan sweep started at the
// first server every run and checked its budget only between servers, so a slow
// server blew the budget and late servers were never swept.
func TestSweepOrphanJobLogsRotatesAndHonorsBudget(t *testing.T) {
	app := newFanoutTestApp(t, 6)
	servers, err := app.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	slow := servers[2].Name
	var mu sync.Mutex
	var swept []string
	slowOnce := atomic.Bool{}
	app.ReverseRPCContext = func(ctx context.Context, server string, env proto.Envelope) (proto.Envelope, error) {
		if server == slow && !slowOnce.Load() {
			slowOnce.Store(true)
			<-ctx.Done() // hangs until the budget expires (first time only)
			return proto.Envelope{}, ctx.Err()
		}
		mu.Lock()
		swept = append(swept, server)
		mu.Unlock()
		return proto.Envelope{Payload: proto.ExecResult{}}, nil
	}
	jobSweepCursor.Store(0)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	start := time.Now()
	err = app.sweepOrphanJobLogs(ctx, servers, "true")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("budget not enforced inside the slow call: err=%v after %s", err, time.Since(start))
	}
	if next := jobSweepCursor.Load(); next != 2 {
		t.Fatalf("next sweep starts at %d, want 2 (the server that was cut short)", next)
	}
	if err := app.sweepOrphanJobLogs(context.Background(), servers, "true"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{servers[0].Name, servers[1].Name, servers[2].Name, servers[3].Name, servers[4].Name, servers[5].Name, servers[0].Name, servers[1].Name}
	if strings.Join(swept, ",") != strings.Join(want, ",") {
		t.Fatalf("sweep order = %v, want %v", swept, want)
	}
}
