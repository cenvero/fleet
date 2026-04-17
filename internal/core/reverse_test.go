// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

func TestReverseHubAndReverseModeServiceList(t *testing.T) {
	t.Parallel()

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

	if err := app.AddServer(ServerRecord{
		Name:    "reverse-node",
		Address: "unknown",
		Mode:    transport.ModeReverse,
		User:    "cenvero-agent",
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	hub := NewReverseHub(app, "test-token")
	defer hub.Close()
	app.ReverseRPC = hub.Call
	app.ReverseStatusLookup = hub.Status

	manager := &fakeServiceManager{
		services: []proto.ServiceInfo{
			{Name: "nginx.service", LoadState: "loaded", ActiveState: "active", SubState: "running", Description: "nginx"},
		},
	}
	reverseServer := agent.Server{
		Mode:           transport.ModeReverse,
		HostKeyPath:    filepath.Join(t.TempDir(), "agent_reverse_key"),
		ServiceManager: manager,
	}

	clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:41000", "127.0.0.1:9443")
	controllerErrCh := make(chan error, 1)
	agentErrCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		controllerErrCh <- hub.ServeConn(serverConn)
	}()
	go func() {
		agentErrCh <- agent.RunReverse(ctx, agent.ReverseOptions{
			ControllerAddress: "127.0.0.1:9443",
			ServerName:        "reverse-node",
			KnownHostsPath:    filepath.Join(t.TempDir(), "controller_known_hosts"),
			NetworkDialContext: func(context.Context, string, string) (net.Conn, error) {
				return clientConn, nil
			},
		}, reverseServer)
	}()

	waitForReverseSession(t, hub, "reverse-node")

	services, err := app.ListServices("reverse-node")
	if err != nil {
		t.Fatalf("ListServices(reverse) error = %v", err)
	}
	if len(services) != 1 || services[0].Name != "nginx.service" {
		t.Fatalf("unexpected reverse services %#v", services)
	}

	if err := app.ReconnectServer("reverse-node", false); err != nil {
		t.Fatalf("ReconnectServer(reverse) error = %v", err)
	}

	cancel()
	hub.Close()
	clientConn.Close()

	select {
	case err := <-controllerErrCh:
		if err != nil {
			t.Fatalf("controller reverse hub exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("controller reverse hub did not exit")
	}

	select {
	case err := <-agentErrCh:
		if err != nil {
			t.Fatalf("agent reverse connector exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("agent reverse connector did not exit")
	}
}

func TestRunReverseRetriesAndReplaysQueuedMetrics(t *testing.T) {
	t.Parallel()

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

	if err := app.AddServer(ServerRecord{
		Name:    "reverse-node",
		Address: "unknown",
		Mode:    transport.ModeReverse,
		User:    "cenvero-agent",
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	hub := NewReverseHub(app, "test-token")
	defer hub.Close()
	app.ReverseRPC = hub.Call
	app.ReverseStatusLookup = hub.Status

	var attempts atomic.Int32
	allowConnect := make(chan struct{})
	queuePath := filepath.Join(t.TempDir(), "reverse-metrics.jsonl")
	collector := &sequenceMetricsCollector{base: time.Now().UTC()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agentErrCh := make(chan error, 1)
	go func() {
		agentErrCh <- agent.RunReverse(ctx, agent.ReverseOptions{
			ControllerAddress:      "127.0.0.1:9443",
			ServerName:             "reverse-node",
			KnownHostsPath:         filepath.Join(t.TempDir(), "controller_known_hosts"),
			MinRetryDelay:          25 * time.Millisecond,
			MaxRetryDelay:          50 * time.Millisecond,
			OfflineMetricsInterval: 10 * time.Millisecond,
			MetricsQueuePath:       queuePath,
			NetworkDialContext: func(context.Context, string, string) (net.Conn, error) {
				attempts.Add(1)
				select {
				case <-allowConnect:
					clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:41001", "127.0.0.1:9443")
					go func() {
						_ = hub.ServeConn(serverConn)
					}()
					return clientConn, nil
				default:
					return nil, fmt.Errorf("controller offline")
				}
			},
		}, agent.Server{
			Mode:             transport.ModeReverse,
			HostKeyPath:      filepath.Join(t.TempDir(), "agent_reverse_key"),
			MetricsCollector: collector,
		})
	}()

	time.Sleep(70 * time.Millisecond)
	close(allowConnect)

	waitForReverseSession(t, hub, "reverse-node")
	waitForMetricReplay(t, app, "reverse-node")

	info, err := hub.Status("reverse-node")
	if err != nil {
		t.Fatalf("Status(reverse-node) error = %v", err)
	}
	if info.ReplayedMetrics == 0 {
		t.Fatalf("expected replayed metrics to be recorded, got %#v", info)
	}
	if attempts.Load() < 2 {
		t.Fatalf("expected multiple reverse dial attempts, got %d", attempts.Load())
	}

	record, err := app.GetServer("reverse-node")
	if err != nil {
		t.Fatalf("GetServer(reverse-node) error = %v", err)
	}
	if record.Metrics.ProcessCount == 0 {
		t.Fatalf("expected replayed metrics to update the server record, got %#v", record.Metrics)
	}
	if data, err := os.ReadFile(queuePath); err == nil && len(data) > 0 {
		t.Fatalf("expected queued metrics to be drained, got %q", string(data))
	}

	cancel()
	select {
	case err := <-agentErrCh:
		if err != nil {
			t.Fatalf("agent reverse connector exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("agent reverse connector did not exit")
	}
}

func waitForReverseSession(t *testing.T, hub *ReverseHub, server string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := hub.Status(server); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("reverse session for %q did not become ready", server)
}

func waitForMetricReplay(t *testing.T, app *App, server string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := app.MetricsDB.ListMetricSnapshots(server, 10)
		if err == nil && len(entries) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("metric replay for %q did not persist snapshots", server)
}

type sequenceMetricsCollector struct {
	mu   sync.Mutex
	next uint64
	base time.Time
}

func (s *sequenceMetricsCollector) Collect(context.Context) (proto.MetricsSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return proto.MetricsSnapshot{
		Timestamp:     s.base.Add(time.Duration(s.next) * time.Second),
		Hostname:      "reverse-node",
		CPUPercent:    float64(20 + s.next),
		MemoryPercent: float64(30 + s.next),
		DiskPercent:   float64(40 + s.next),
		ProcessCount:  s.next,
	}, nil
}
