// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

func TestScaleSmoke100ReverseAgents(t *testing.T) {
	if os.Getenv("FLEET_RUN_SCALE_TEST") == "" {
		t.Skip("set FLEET_RUN_SCALE_TEST=1 to run the 100-agent scale smoke test")
	}
	if testing.Short() {
		t.Skip("skipping scale smoke test in short mode")
	}

	agentCount := envInt("FLEET_SCALE_AGENT_COUNT", 100)
	rounds := envInt("FLEET_SCALE_COLLECTION_ROUNDS", 2)
	maxAllocMB := envInt("FLEET_SCALE_ASSERT_ALLOC_MB", 0)

	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeReverse,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer app.Close()

	hub := NewReverseHub(app)
	defer hub.Close()
	app.ReverseRPC = hub.Call
	app.ReverseStatusLookup = hub.Status
	app.ReverseDisconnect = hub.Disconnect

	names := make([]string, 0, agentCount)
	for i := 0; i < agentCount; i++ {
		name := fmt.Sprintf("scale-node-%03d", i+1)
		names = append(names, name)
		if err := app.AddServer(ServerRecord{
			Name:    name,
			Address: "unknown",
			Mode:    transport.ModeReverse,
			User:    "cenvero-agent",
		}); err != nil {
			t.Fatalf("AddServer(%s) error = %v", name, err)
		}
	}

	agentsDir := filepath.Join(t.TempDir(), "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(agentsDir) error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, agentCount)
	for i, name := range names {
		i := i
		name := name
		go func() {
			errCh <- agent.RunReverse(ctx, agent.ReverseOptions{
				ControllerAddress: "127.0.0.1:9443",
				ServerName:        name,
				KnownHostsPath:    filepath.Join(agentsDir, name+"-known_hosts"),
				MetricsQueuePath:  filepath.Join(agentsDir, name+"-metrics.jsonl"),
				MinRetryDelay:     10 * time.Millisecond,
				MaxRetryDelay:     25 * time.Millisecond,
				NetworkDialContext: func(context.Context, string, string) (net.Conn, error) {
					clientConn, serverConn := testutil.NewBufferedConnPair(
						fmt.Sprintf("127.0.0.1:%d", 43000+i),
						"127.0.0.1:9443",
					)
					go func() {
						_ = hub.ServeConn(serverConn)
					}()
					return clientConn, nil
				},
			}, agent.Server{
				Mode:        transport.ModeReverse,
				HostKeyPath: filepath.Join(agentsDir, name+"-host_key"),
				MetricsCollector: &scaleMetricsCollector{
					hostname: name,
					baseCPU:  15 + float64(i%20),
					baseMem:  25 + float64(i%30),
					baseDisk: 35 + float64(i%40),
				},
			})
		}()
	}

	waitForReverseSessions(t, hub, names)

	start := time.Now()
	for round := 0; round < rounds; round++ {
		for _, name := range names {
			if _, err := app.collectMetrics(name, false); err != nil {
				t.Fatalf("collectMetrics(%s, round=%d) error = %v", name, round, err)
			}
		}
	}
	duration := time.Since(start)

	snapshot, err := app.DashboardSnapshot()
	if err != nil {
		t.Fatalf("DashboardSnapshot() error = %v", err)
	}
	if snapshot.Summary.OnlineServers != agentCount {
		t.Fatalf("expected %d online servers, got %d", agentCount, snapshot.Summary.OnlineServers)
	}
	if snapshot.Summary.OfflineServers != 0 {
		t.Fatalf("expected 0 offline servers, got %d", snapshot.Summary.OfflineServers)
	}
	if len(snapshot.Servers) != agentCount {
		t.Fatalf("expected %d servers in dashboard snapshot, got %d", agentCount, len(snapshot.Servers))
	}

	metricStateEntries, err := app.MetricsDB.ListStateEntries()
	if err != nil {
		t.Fatalf("MetricsDB.ListStateEntries() error = %v", err)
	}
	latestCount := 0
	for _, entry := range metricStateEntries {
		if strings.HasPrefix(entry.Key, "latest.scale-node-") {
			latestCount++
		}
	}
	if latestCount != agentCount {
		t.Fatalf("expected %d latest metric state entries, got %d", agentCount, latestCount)
	}

	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	allocMB := float64(stats.Alloc) / (1024 * 1024)
	if maxAllocMB > 0 && allocMB > float64(maxAllocMB) {
		t.Fatalf("controller alloc %.2f MiB exceeded configured ceiling of %d MiB", allocMB, maxAllocMB)
	}

	t.Logf(
		"scale validation: agents=%d rounds=%d duration=%s alloc_mib=%.2f heap_objects=%d goroutines=%d",
		agentCount,
		rounds,
		duration.Round(time.Millisecond),
		allocMB,
		stats.HeapObjects,
		runtime.NumGoroutine(),
	)

	cancel()
	hub.Close()
	for i := 0; i < agentCount; i++ {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("reverse agent exited with error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for reverse agent shutdown")
		}
	}
}

func BenchmarkDashboardSnapshot100Servers(b *testing.B) {
	configDir := filepath.Join(b.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeDirect,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	}); err != nil {
		b.Fatalf("Initialize() error = %v", err)
	}

	app, err := Open(configDir)
	if err != nil {
		b.Fatalf("Open() error = %v", err)
	}
	defer app.Close()

	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("bench-node-%03d", i+1)
		if err := app.AddServer(ServerRecord{
			Name:    name,
			Address: fmt.Sprintf("10.0.0.%d", i+1),
			Mode:    transport.ModeDirect,
			Observed: ServerObservation{
				Reachable: true,
				LastSeen:  time.Now().Add(-time.Duration(i) * time.Second).UTC(),
				OS:        "linux",
				Arch:      "amd64",
			},
			Metrics: proto.MetricsSnapshot{
				CPUPercent:    20 + float64(i%25),
				MemoryPercent: 30 + float64(i%30),
				DiskPercent:   40 + float64(i%35),
				ProcessCount:  uint64(i + 10),
			},
		}); err != nil {
			b.Fatalf("AddServer(%s) error = %v", name, err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := app.DashboardSnapshot(); err != nil {
			b.Fatalf("DashboardSnapshot() error = %v", err)
		}
	}
}

type scaleMetricsCollector struct {
	mu       sync.Mutex
	hostname string
	baseCPU  float64
	baseMem  float64
	baseDisk float64
	next     uint64
}

func (s *scaleMetricsCollector) Collect(context.Context) (proto.MetricsSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return proto.MetricsSnapshot{
		Timestamp:     time.Now().UTC(),
		Hostname:      s.hostname,
		CPUPercent:    s.baseCPU + float64(s.next%5),
		MemoryPercent: s.baseMem + float64(s.next%5),
		DiskPercent:   s.baseDisk + float64(s.next%5),
		ProcessCount:  s.next,
	}, nil
}

func waitForReverseSessions(t *testing.T, hub *ReverseHub, names []string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ready := true
		for _, name := range names {
			if _, err := hub.Status(name); err != nil {
				ready = false
				break
			}
		}
		if ready {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("reverse sessions did not become ready for %d servers", len(names))
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
