// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

func TestCollectMetricsPersistsSnapshotAndGeneratesAlerts(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeDirect,
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
	notifier := &fakeNotifier{}
	app.Notifier = notifier

	server := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
		MetricsCollector: fakeMetricsCollector{
			Snapshot: proto.MetricsSnapshot{
				Timestamp:        time.Now().UTC(),
				Hostname:         "loopback",
				CPUPercent:       92.5,
				MemoryPercent:    87.2,
				MemoryUsedBytes:  8 << 30,
				MemoryTotalBytes: 16 << 30,
				DiskPath:         "/",
				DiskPercent:      72.4,
				DiskUsedBytes:    72 << 30,
				DiskTotalBytes:   100 << 30,
				UptimeSeconds:    3600,
				ProcessCount:     42,
			},
		},
	}
	errCh := make(chan error, 1)

	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40003", "127.0.0.1:2222")
		go func() {
			errCh <- server.ServeConn(serverConn)
		}()
		return clientConn, nil
	}

	if err := app.AddServer(ServerRecord{
		Name:    "loopback",
		Address: "127.0.0.1",
		Port:    2222,
		Mode:    transport.ModeDirect,
		User:    "cenvero-agent",
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	snapshot, err := app.CollectMetrics("loopback")
	if err != nil {
		t.Fatalf("CollectMetrics() error = %v", err)
	}
	if snapshot.CPUPercent < 90 {
		t.Fatalf("expected high CPU in snapshot, got %.1f", snapshot.CPUPercent)
	}

	record, err := app.GetServer("loopback")
	if err != nil {
		t.Fatalf("GetServer() error = %v", err)
	}
	if record.Metrics.Hostname != "loopback" {
		t.Fatalf("expected persisted metrics hostname, got %q", record.Metrics.Hostname)
	}

	latest, err := app.MetricsDB.LatestMetricSnapshot("loopback")
	if err != nil {
		t.Fatalf("LatestMetricSnapshot() error = %v", err)
	}
	var persisted proto.MetricsSnapshot
	if err := json.Unmarshal([]byte(latest.Payload), &persisted); err != nil {
		t.Fatalf("json.Unmarshal(latest metrics) error = %v", err)
	}
	if persisted.ProcessCount != 42 {
		t.Fatalf("expected persisted process count 42, got %d", persisted.ProcessCount)
	}

	alerts, err := app.ListAlerts("loopback", "")
	if err != nil {
		t.Fatalf("ListAlerts() error = %v", err)
	}
	if len(alerts) != 2 {
		t.Fatalf("expected 2 generated alerts, got %#v", alerts)
	}
	if notifier.Count() != 1 {
		t.Fatalf("expected exactly 1 critical notification, got %d", notifier.Count())
	}

	if err := <-errCh; err != nil {
		t.Fatalf("agent server exited with error: %v", err)
	}
}

func TestCollectMetricsDoesNotDuplicateCriticalNotifications(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeDirect,
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
	notifier := &fakeNotifier{}
	app.Notifier = notifier

	server := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
		MetricsCollector: fakeMetricsCollector{
			Snapshot: proto.MetricsSnapshot{
				Timestamp:     time.Now().UTC(),
				Hostname:      "loopback",
				CPUPercent:    97.1,
				MemoryPercent: 52.0,
				DiskPercent:   40.0,
			},
		},
	}

	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40004", "127.0.0.1:2222")
		go func() {
			_ = server.ServeConn(serverConn)
		}()
		return clientConn, nil
	}

	if err := app.AddServer(ServerRecord{
		Name:    "loopback",
		Address: "127.0.0.1",
		Port:    2222,
		Mode:    transport.ModeDirect,
		User:    "cenvero-agent",
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	if _, err := app.CollectMetrics("loopback"); err != nil {
		t.Fatalf("first CollectMetrics() error = %v", err)
	}
	if _, err := app.CollectMetrics("loopback"); err != nil {
		t.Fatalf("second CollectMetrics() error = %v", err)
	}
	if notifier.Count() != 1 {
		t.Fatalf("expected a single desktop notification for repeated critical alert, got %d", notifier.Count())
	}
}

type fakeMetricsCollector struct {
	Snapshot proto.MetricsSnapshot
	Err      error
}

func (f fakeMetricsCollector) Collect(context.Context) (proto.MetricsSnapshot, error) {
	return f.Snapshot, f.Err
}

type fakeNotifier struct {
	mu       sync.Mutex
	messages []string
}

func (f *fakeNotifier) Notify(title, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, title+"::"+message)
	return nil
}

func (f *fakeNotifier) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages)
}
