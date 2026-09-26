// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

func replayTestApp(t testing.TB, server string) *App {
	t.Helper()
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeReverse,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	if err := app.AddServer(ServerRecord{Name: server, Address: "unknown", Mode: transport.ModeReverse,
		User: "cenvero-agent", EnrollSecret: testReverseEnroll}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}
	return app
}

// connectReverseAgent runs a reverse agent for server against hub over an
// in-memory connection and returns a stop function.
func connectReverseAgent(t *testing.T, app *App, hub *ReverseHub, server string, srv agent.Server) func() {
	t.Helper()
	clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:41010", "127.0.0.1:9443")
	ctx, cancel := context.WithCancel(context.Background())
	agentDone := make(chan struct{})
	hubDone := make(chan struct{})
	fingerprint := testControllerFingerprint(t, app)
	go func() { defer close(hubDone); _ = hub.ServeConn(serverConn) }()
	go func() {
		defer close(agentDone)
		_ = agent.RunReverse(ctx, agent.ReverseOptions{
			EnrollToken: testReverseEnroll, ControllerFingerprint: fingerprint,
			ControllerAddress: "127.0.0.1:9443", ServerName: server,
			KnownHostsPath:     filepath.Join(t.TempDir(), "controller_known_hosts"),
			NetworkDialContext: func(context.Context, string, string) (net.Conn, error) { return clientConn, nil },
		}, srv)
	}()
	return func() {
		cancel()
		hub.Close()
		_ = clientConn.Close()
		<-agentDone
		<-hubDone
	}
}

// pagingProbe exposes the file queue's paging and counts pages served.
type pagingProbe struct {
	agent.MetricsQueue
	pages atomic.Int32
	gate  chan struct{} // when set, the first page waits for it
}

func (p *pagingProbe) PeekPage(n int) (agent.MetricsBatch, error) {
	if p.pages.Add(1) == 1 && p.gate != nil {
		<-p.gate
	}
	return p.MetricsQueue.(agent.MetricsPager).PeekPage(n)
}

// legacyQueue hides PeekPage, so the agent answers every peek with the whole
// queue — what an agent older than paging does.
type legacyQueue struct{ agent.MetricsQueue }

func fillQueue(t *testing.T, queue agent.MetricsQueue, n int) {
	t.Helper()
	base := time.Now().UTC().Add(-time.Duration(n) * time.Minute)
	for i := 0; i < n; i++ {
		if err := queue.Enqueue(proto.MetricsSnapshot{Timestamp: base.Add(time.Duration(i) * time.Minute),
			Hostname: "replay", CPUPercent: 10, ProcessCount: uint64(i + 1)}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
}

func countSnapshots(t *testing.T, app *App, server string) int {
	t.Helper()
	entries, err := app.MetricsDB.ListMetricSnapshots(server, 0)
	if err != nil {
		t.Fatalf("ListMetricSnapshots: %v", err)
	}
	return len(entries)
}

func TestReverseReplayPagesLargeBacklog(t *testing.T) {
	app := replayTestApp(t, "paged")
	hub := NewReverseHub(app, "test-token")
	queue := &pagingProbe{MetricsQueue: agent.NewFileMetricsQueue(filepath.Join(t.TempDir(), "q.jsonl"))}
	const backlog = 2*metricsReplayPageSize + 234
	fillQueue(t, queue, backlog)

	stop := connectReverseAgent(t, app, hub, "paged", agent.Server{Mode: transport.ModeReverse,
		HostKeyPath: filepath.Join(t.TempDir(), "agent_key"), MetricsQueue: queue})
	defer stop()

	info := waitForReplayedMetrics(t, hub, "paged")
	if info.ReplayedMetrics != backlog {
		t.Fatalf("replayed %d snapshots, want %d", info.ReplayedMetrics, backlog)
	}
	if got := countSnapshots(t, app, "paged"); got != backlog {
		t.Fatalf("persisted %d snapshots, want %d", got, backlog)
	}
	if pages := queue.pages.Load(); pages < 3 {
		t.Fatalf("backlog of %d replayed in %d pages; want it paged by %d", backlog, pages, metricsReplayPageSize)
	}
	if rest, err := queue.Peek(); err != nil || len(rest.Snapshots) != 0 {
		t.Fatalf("queue not drained after replay: %d left, err=%v", len(rest.Snapshots), err)
	}
	record, err := app.GetServer("paged")
	if err != nil || record.Metrics.ProcessCount != backlog {
		t.Fatalf("server record should carry the newest replayed snapshot, got %+v (err=%v)", record.Metrics, err)
	}
}

// An agent that predates paging ignores max_snapshots and returns its whole
// queue with no "more" flag; the controller must accept that as one page.
func TestReverseReplayAcceptsWholeQueueFromOlderAgent(t *testing.T) {
	app := replayTestApp(t, "legacy")
	hub := NewReverseHub(app, "test-token")
	queue := legacyQueue{agent.NewFileMetricsQueue(filepath.Join(t.TempDir(), "q.jsonl"))}
	const backlog = metricsReplayPageSize + 77
	fillQueue(t, queue, backlog)

	stop := connectReverseAgent(t, app, hub, "legacy", agent.Server{Mode: transport.ModeReverse,
		HostKeyPath: filepath.Join(t.TempDir(), "agent_key"), MetricsQueue: queue})
	defer stop()

	if info := waitForReplayedMetrics(t, hub, "legacy"); info.ReplayedMetrics != backlog {
		t.Fatalf("replayed %d, want %d", info.ReplayedMetrics, backlog)
	}
	if got := countSnapshots(t, app, "legacy"); got != backlog {
		t.Fatalf("persisted %d, want %d", got, backlog)
	}
}

// The session is usable while its backlog replays: registration no longer
// waits for the replay.
func TestReverseSessionIsLiveDuringReplay(t *testing.T) {
	app := replayTestApp(t, "live")
	hub := NewReverseHub(app, "test-token")
	app.ReverseRPCContext = hub.CallContext
	gate := make(chan struct{})
	queue := &pagingProbe{MetricsQueue: agent.NewFileMetricsQueue(filepath.Join(t.TempDir(), "q.jsonl")), gate: gate}
	fillQueue(t, queue, 10)
	manager := &fakeServiceManager{services: []proto.ServiceInfo{{Name: "nginx.service"}}}

	stop := connectReverseAgent(t, app, hub, "live", agent.Server{Mode: transport.ModeReverse,
		HostKeyPath: filepath.Join(t.TempDir(), "agent_key"), MetricsQueue: queue, ServiceManager: manager})
	defer stop()

	waitForReverseSession(t, hub, "live")
	for queue.pages.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	// The replay is parked inside its first page; the server must answer.
	if services, err := app.ListServices("live"); err != nil || len(services) != 1 {
		t.Fatalf("call during replay = %v, err=%v", services, err)
	}
	close(gate)
	if info := waitForReplayedMetrics(t, hub, "live"); info.ReplayedMetrics != 10 {
		t.Fatalf("replayed %d, want 10", info.ReplayedMetrics)
	}
}

// Re-delivering a batch whose acknowledgement was lost must not duplicate
// history, and the latest-snapshot state never moves backwards.
func TestPersistReplayedMetricsIsIdempotent(t *testing.T) {
	app := replayTestApp(t, "idem")
	base := time.Date(2026, 9, 1, 12, 0, 0, 123456789, time.UTC)
	batch := make([]proto.MetricsSnapshot, 20)
	for i := range batch {
		batch[i] = proto.MetricsSnapshot{Timestamp: base.Add(time.Duration(i) * time.Minute), ProcessCount: uint64(i)}
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := app.persistReplayedMetrics("idem", batch); err != nil {
			t.Fatalf("persist attempt %d: %v", attempt, err)
		}
	}
	if got := countSnapshots(t, app, "idem"); got != len(batch) {
		t.Fatalf("stored %d rows after re-delivery, want %d", got, len(batch))
	}
	// A partially overlapping batch adds only what is new.
	overlap := append(append([]proto.MetricsSnapshot{}, batch[15:]...),
		proto.MetricsSnapshot{Timestamp: base.Add(30 * time.Minute), ProcessCount: 30})
	if err := app.persistReplayedMetrics("idem", overlap); err != nil {
		t.Fatal(err)
	}
	if got := countSnapshots(t, app, "idem"); got != len(batch)+1 {
		t.Fatalf("stored %d rows after an overlapping batch, want %d", got, len(batch)+1)
	}

	// latest.<server> follows the newest snapshot and never goes backwards.
	if err := app.persistMetricsSnapshot("idem", proto.MetricsSnapshot{Timestamp: base.Add(time.Hour), ProcessCount: 999}); err != nil {
		t.Fatal(err)
	}
	if err := app.persistReplayedMetrics("idem", batch[:3]); err != nil {
		t.Fatal(err)
	}
	latest, err := app.MetricsDB.GetState("latest.idem")
	if err != nil {
		t.Fatal(err)
	}
	decoded, _ := proto.DecodePayload[proto.MetricsSnapshot](json.RawMessage(latest))
	if decoded.ProcessCount != 999 {
		t.Fatalf("replaying older snapshots moved latest back to %+v", decoded)
	}
}

// BenchmarkReplayPersist compares storing a 500-snapshot replay page one
// snapshot at a time (two commits each, as before) with one transaction.
func BenchmarkReplayPersist(b *testing.B) {
	const page = 500
	snapshots := make([]proto.MetricsSnapshot, page)
	for _, mode := range []string{"per-snapshot", "batched"} {
		b.Run(mode, func(b *testing.B) {
			app := replayTestApp(b, "bench")
			base := time.Now().UTC()
			round := 0
			b.ReportAllocs()
			for b.Loop() {
				round++
				for i := range snapshots {
					snapshots[i] = proto.MetricsSnapshot{Timestamp: base.Add(time.Duration(round*page+i) * time.Second), ProcessCount: uint64(i)}
				}
				if mode == "per-snapshot" {
					for _, s := range snapshots {
						if err := app.persistMetricsSnapshot("bench", s); err != nil {
							b.Fatal(err)
						}
					}
					continue
				}
				if err := app.persistReplayedMetrics("bench", snapshots); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N*page), "µs/snapshot")
		})
	}
}
