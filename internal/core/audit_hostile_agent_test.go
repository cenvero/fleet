// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/transport"
)

// hostileQueue answers every metrics peek with an error message larger than an
// audit log line may be — what a compromised agent can send in any RPC error.
type hostileQueue struct{ agent.MetricsQueue }

func (hostileQueue) PeekPage(int) (agent.MetricsBatch, error) {
	return agent.MetricsBatch{}, errors.New(strings.Repeat("A", 5<<20))
}

// TestHostileAgentErrorCannotBreakAuditLog: a reverse agent that answers the
// automatic metrics replay with a huge error must not leave the controller's
// audit log unreadable. Before audit entries were bounded, the resulting
// "metrics.replay.failed" line exceeded the log's line limit and every later
// append, `fleet logs` and chain verification failed.
func TestHostileAgentErrorCannotBreakAuditLog(t *testing.T) {
	app := replayTestApp(t, "hostile")
	hub := NewReverseHub(app, "test-token")
	queue := hostileQueue{agent.NewFileMetricsQueue(filepath.Join(t.TempDir(), "q.jsonl"))}
	stop := connectReverseAgent(t, app, hub, "hostile", agent.Server{Mode: transport.ModeReverse,
		HostKeyPath: filepath.Join(t.TempDir(), "agent_key"), MetricsQueue: queue})
	defer stop()

	auditPath := filepath.Join(app.ConfigDir, "logs", "_audit.log")
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := logs.NewAuditLog(auditPath).ReadAll()
		if err != nil {
			t.Fatalf("audit log unreadable after a hostile agent error: %v", err)
		}
		found := false
		for _, e := range entries {
			if e.Action == "metrics.replay.failed" && e.Target == "hostile" {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("metrics.replay.failed was never audited")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A different process (a fresh AuditLog, no cached tail) must still append.
	fresh := logs.NewAuditLog(auditPath)
	if err := fresh.Append(logs.AuditEntry{Action: "after", Target: "hostile", Operator: "op"}); err != nil {
		t.Fatalf("Append after hostile entry: %v", err)
	}
	if ok, idx, err := fresh.Verify(); err != nil || !ok {
		t.Fatalf("Verify = (%v, %d, %v), want an intact chain", ok, idx, err)
	}
}
