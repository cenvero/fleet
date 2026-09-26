// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

// TestSuccessfulCallsRefreshLastSeen: pooled/relayed calls never redial, so a
// server's LastSeen froze at its last dial even while it answered every poll.
func TestSuccessfulCallsRefreshLastSeen(t *testing.T) {
	t.Parallel()
	app := newFanoutTestApp(t, 1)
	name := fanoutServerName(0)
	stale := time.Now().Add(-time.Hour).UTC()
	rec, _ := app.GetServer(name)
	rec.Observed = ServerObservation{Reachable: true, LastSeen: stale, AgentVersion: "1.0.0"}
	if err := app.SaveServer(rec); err != nil {
		t.Fatal(err)
	}
	app.ReverseRPCContext = func(ctx context.Context, server string, env proto.Envelope) (proto.Envelope, error) {
		if env.Action == "metrics.collect" {
			// A redial during the call records a fresh hello; the poller must
			// not revert it with the copy it read before the call.
			cur, _ := app.GetServer(server)
			cur.Observed.AgentVersion = "2.0.0"
			_ = app.SaveServer(cur)
			return metricsReply(server, 10), nil
		}
		return proto.Envelope{Payload: proto.ExecResult{}}, nil
	}

	if _, err := app.SampleMetrics(name); err != nil {
		t.Fatal(err)
	}
	got, _ := app.GetServer(name)
	if time.Since(got.Observed.LastSeen) > time.Minute || !got.Observed.Reachable {
		t.Fatalf("metrics poll did not refresh LastSeen: %+v", got.Observed)
	}
	if got.Observed.AgentVersion != "2.0.0" {
		t.Fatalf("poll reverted the observation recorded during the call: %q", got.Observed.AgentVersion)
	}

	// Any other successful call refreshes a stale LastSeen...
	got.Observed.LastSeen = stale
	if err := app.SaveServer(got); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ExecCommand(name, "true"); err != nil {
		t.Fatal(err)
	}
	got, _ = app.GetServer(name)
	if time.Since(got.Observed.LastSeen) > time.Minute {
		t.Fatal("successful exec did not refresh a stale LastSeen")
	}
	// ...but a recent one is left alone: no record rewrite per call.
	path := filepath.Join(app.ConfigDir, "servers", name+".toml")
	before, _ := os.Stat(path)
	for i := 0; i < 5; i++ {
		if _, err := app.ExecCommand(name, "true"); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("server record rewritten although LastSeen was recent")
	}
}
