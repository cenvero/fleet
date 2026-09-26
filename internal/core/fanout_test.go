// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

func TestForEachLimitBoundsConcurrencyAndVisitsAll(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ n, limit, wantMax int }{
		{0, 4, 0}, {1, 4, 1}, {10, 1, 1}, {50, 4, 4}, {50, 0, DefaultFanoutLimit}, {3, 10, 3},
	} {
		var inflight, maxInflight atomic.Int32
		seen := make([]atomic.Int32, tc.n)
		ForEachLimit(tc.n, tc.limit, func(i int) {
			cur := inflight.Add(1)
			for {
				prev := maxInflight.Load()
				if cur <= prev || maxInflight.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			seen[i].Add(1)
			inflight.Add(-1)
		})
		for i := range seen {
			if seen[i].Load() != 1 {
				t.Fatalf("n=%d limit=%d: index %d visited %d times", tc.n, tc.limit, i, seen[i].Load())
			}
		}
		if got := int(maxInflight.Load()); got > tc.wantMax || (tc.n > 1 && tc.wantMax > 1 && got < 2) {
			t.Fatalf("n=%d limit=%d: max in flight %d, want <= %d (and concurrent)", tc.n, tc.limit, got, tc.wantMax)
		}
	}
}

func TestForEachLimitOneIsSequentialInOrder(t *testing.T) {
	t.Parallel()
	var order []int
	ForEachLimit(20, 1, func(i int) { order = append(order, i) })
	for i, v := range order {
		if v != i {
			t.Fatalf("limit 1 ran out of order: %v", order)
		}
	}
}

func TestOrderedFlusherReleasesInIndexOrder(t *testing.T) {
	t.Parallel()
	const n = 200
	var mu sync.Mutex
	var flushed []int
	f := NewOrderedFlusher(n, func(i int) {
		mu.Lock()
		flushed = append(flushed, i)
		mu.Unlock()
	})
	ForEachLimit(n, 16, func(i int) {
		time.Sleep(time.Duration(rand.Intn(300)) * time.Microsecond) // #nosec G404 -- test jitter
		f.Done(i)
	})
	if len(flushed) != n {
		t.Fatalf("flushed %d of %d", len(flushed), n)
	}
	for i, v := range flushed {
		if v != i {
			t.Fatalf("flush order broken at %d: got %d", i, v)
		}
	}
	f.Done(3) // duplicate/late Done is a no-op
	f.Done(n + 5)
	if len(flushed) != n {
		t.Fatal("duplicate Done flushed again")
	}
}

// TestExecCommandAllIsBounded: ExecCommandAll used to start one goroutine (one
// daemon control connection in reverse mode) per server at once.
func TestExecCommandAllIsBounded(t *testing.T) {
	t.Parallel()
	app := newFanoutTestApp(t, 60)
	var inflight, maxInflight atomic.Int32
	app.ReverseRPC = func(server string, env proto.Envelope) (proto.Envelope, error) {
		cur := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			prev := maxInflight.Load()
			if cur <= prev || maxInflight.CompareAndSwap(prev, cur) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		return proto.Envelope{Payload: proto.ExecResult{Stdout: server}}, nil
	}
	results := app.ExecCommandAll("hostname")
	if len(results) != 60 {
		t.Fatalf("got %d results", len(results))
	}
	for _, r := range results {
		if r.Error != nil || r.Result.Stdout != r.Server {
			t.Fatalf("bad result %+v", r)
		}
	}
	if got := maxInflight.Load(); got > DefaultFanoutLimit || got < 2 {
		t.Fatalf("max in flight = %d, want 2..%d", got, DefaultFanoutLimit)
	}
}

func newFanoutTestApp(t *testing.T, servers int) *App {
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
	for i := 0; i < servers; i++ {
		if err := app.AddServer(ServerRecord{Name: fanoutServerName(i), Address: "127.0.0.1", Mode: transport.ModeReverse}); err != nil {
			t.Fatal(err)
		}
	}
	return app
}

func fanoutServerName(i int) string {
	return "node-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
}
