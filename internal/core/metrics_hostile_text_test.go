// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

// TestCollectMetricsBoundsAgentText: every poll saves the snapshot to the
// server record and the metrics history, so a hostile agent reporting a
// multi-megabyte hostname or disk path would grow the controller's database
// by that much every minute. The text is bounded and neutralised first.
func TestCollectMetricsBoundsAgentText(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeDirect,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer app.Close()

	server := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
		MetricsCollector: fakeMetricsCollector{Snapshot: proto.MetricsSnapshot{
			Timestamp:  time.Now().UTC(),
			Hostname:   "web\x1b]0;x\x07" + strings.Repeat("h", 4<<20),
			DiskPath:   "/" + strings.Repeat("d", 4<<20),
			CPUPercent: 1,
		}},
	}
	errCh := make(chan error, 1)
	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40013", "127.0.0.1:2222")
		go func() { errCh <- server.ServeConn(serverConn) }()
		return clientConn, nil
	}
	if err := app.AddServer(ServerRecord{Name: "hostile", Address: "127.0.0.1", Port: 2222,
		Mode: transport.ModeDirect, User: "cenvero-agent"}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}
	if _, err := app.CollectMetrics("hostile"); err != nil {
		t.Fatalf("CollectMetrics() error = %v", err)
	}
	record, err := app.GetServer("hostile")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(record.Metrics.Hostname) + len(record.Metrics.DiskPath); n > 16<<10 {
		t.Fatalf("server record stores %d bytes of agent snapshot text", n)
	}
	if strings.ContainsAny(record.Metrics.Hostname, "\x1b\x07") {
		t.Fatalf("snapshot hostname keeps control characters: %q", record.Metrics.Hostname[:32])
	}
	latest, err := app.MetricsDB.LatestMetricSnapshot("hostile")
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.Payload) > 32<<10 {
		t.Fatalf("metrics history stores a %d-byte snapshot", len(latest.Payload))
	}
	app.DisconnectPooledSessions()
	drainAgentServeErrs(t, errCh)
}

// endlessQueue is a hostile agent's metrics queue: every peek hands out a new
// full page of fresh snapshots and claims there is more.
type endlessQueue struct {
	agent.MetricsQueue
	next atomic.Int64
}

func (q *endlessQueue) PeekPage(n int) (agent.MetricsBatch, error) {
	base := q.next.Add(int64(n)) - int64(n)
	batch := agent.MetricsBatch{ID: fmt.Sprintf("b%d", base), More: true}
	start := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < n; i++ {
		batch.Snapshots = append(batch.Snapshots, proto.MetricsSnapshot{Timestamp: start.Add(time.Duration(base+int64(i)) * time.Millisecond)})
	}
	return batch, nil
}

func (q *endlessQueue) Acknowledge(string) error { return nil }

// TestReverseReplayIsBoundedPerConnection: a reverse agent that never runs out
// of "queued" metrics cannot make one connection write an unbounded history.
func TestReverseReplayIsBoundedPerConnection(t *testing.T) {
	// Not parallel: it lowers the package-level cap so the test stays quick
	// under -race (persisting 20,000 snapshots takes seconds there).
	oldCap := metricsReplayMaxSnapshots
	metricsReplayMaxSnapshots = 3 * metricsReplayPageSize
	t.Cleanup(func() { metricsReplayMaxSnapshots = oldCap })
	app := replayTestApp(t, "endless")
	hub := NewReverseHub(app, "test-token")
	queue := &endlessQueue{MetricsQueue: agent.NewFileMetricsQueue(filepath.Join(t.TempDir(), "q.jsonl"))}
	stop := connectReverseAgent(t, app, hub, "endless", agent.Server{Mode: transport.ModeReverse,
		HostKeyPath: filepath.Join(t.TempDir(), "agent_key"), MetricsQueue: queue})
	defer stop()

	info := waitForReplayedMetrics(t, hub, "endless")
	if info.ReplayedMetrics > metricsReplayMaxSnapshots {
		t.Fatalf("one connection replayed %d snapshots, want at most %d", info.ReplayedMetrics, metricsReplayMaxSnapshots)
	}
	if got := countSnapshots(t, app, "endless"); got > metricsReplayMaxSnapshots {
		t.Fatalf("persisted %d snapshots from one connection, want at most %d", got, metricsReplayMaxSnapshots)
	}
}

// TestPersistReplayedMetricsBoundsAgentText: the offline-metrics replay stores
// agent snapshots too.
func TestPersistReplayedMetricsBoundsAgentText(t *testing.T) {
	app := replayTestApp(t, "hostile")
	snap := proto.MetricsSnapshot{Timestamp: time.Now().UTC(), Hostname: strings.Repeat("h", 1<<20), DiskPath: "/\x1b[2J"}
	if err := app.persistReplayedMetrics("hostile", []proto.MetricsSnapshot{snap}); err != nil {
		t.Fatal(err)
	}
	latest, err := app.MetricsDB.LatestMetricSnapshot("hostile")
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.Payload) > 32<<10 || strings.Contains(latest.Payload, "\x1b") {
		t.Fatalf("replayed snapshot stored unbounded or raw: %d bytes", len(latest.Payload))
	}
}
