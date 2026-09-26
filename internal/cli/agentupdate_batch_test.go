// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
	"github.com/spf13/cobra"
)

// TestUpdateBatchRunsServersConcurrentlyInOrder: a batch used to update its
// servers strictly one at a time; now they overlap (bounded) while the printed
// lines and the failure list keep batch order.
func TestUpdateBatchRunsServersConcurrentlyInOrder(t *testing.T) {
	behave := map[string]fakeExecBehavior{}
	var servers []string
	for i := 1; i <= 6; i++ {
		name := fmt.Sprintf("agent-%d", i)
		behave[name] = fakeExecBehavior{}
		servers = append(servers, name)
	}
	dir, _ := setupExecFanout(t, behave, nil)
	app, err := openApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	var inflight, maxInflight atomic.Int32
	app.ReverseRPCContext = func(ctx context.Context, server string, env proto.Envelope) (proto.Envelope, error) {
		cur := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			prev := maxInflight.Load()
			if cur <= prev || maxInflight.CompareAndSwap(prev, cur) {
				break
			}
		}
		// Later servers finish first.
		n := int(server[len(server)-1] - '0')
		time.Sleep(time.Duration(7-n) * 30 * time.Millisecond)
		if server == "agent-4" {
			return proto.Envelope{Error: &proto.Error{Code: "update_failed", Message: "boom"}}, nil
		}
		return proto.Envelope{Payload: proto.UpdateApplyResult{Version: "9.9.9", Applied: true, RestartScheduled: true}}, nil
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	start := time.Now()
	err = updateBatch(cmd, app, servers)
	elapsed := time.Since(start)
	if err == nil || err.Error() != "1 server(s) failed: agent-4" {
		t.Fatalf("updateBatch error = %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != len(servers) {
		t.Fatalf("got %d lines:\n%s", len(lines), out.String())
	}
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), servers[i]+" ") {
			t.Fatalf("line %d out of order: %q\n%s", i, line, out.String())
		}
	}
	if !strings.Contains(lines[3], "ERROR  update_failed: boom") || !strings.Contains(lines[0], "updated -> ") {
		t.Fatalf("unexpected result lines:\n%s", out.String())
	}
	if got := maxInflight.Load(); got < 2 || got > agentUpdateParallelism {
		t.Fatalf("max in flight = %d, want 2..%d", got, agentUpdateParallelism)
	}
	// Sequential would be 30*(6+5+4+3+2+1) = 630ms.
	if elapsed > 450*time.Millisecond {
		t.Fatalf("batch took %s", elapsed)
	}
}
