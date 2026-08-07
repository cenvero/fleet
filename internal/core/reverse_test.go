// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"errors"
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

func testControllerFingerprint(t *testing.T, app *App) string {
	t.Helper()
	fingerprint, err := app.ControllerHostKeyFingerprint()
	if err != nil {
		t.Fatalf("ControllerHostKeyFingerprint() error = %v", err)
	}
	return fingerprint
}

const testReverseEnroll = "test-reverse-enroll-token"

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
		Name:         "reverse-node",
		Address:      "unknown",
		Mode:         transport.ModeReverse,
		User:         "cenvero-agent",
		EnrollSecret: testReverseEnroll,
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
			EnrollToken:           testReverseEnroll,
			ControllerFingerprint: testControllerFingerprint(t, app),
			ControllerAddress:     "127.0.0.1:9443",
			ServerName:            "reverse-node",
			KnownHostsPath:        filepath.Join(t.TempDir(), "controller_known_hosts"),
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
		Name:         "reverse-node",
		Address:      "unknown",
		Mode:         transport.ModeReverse,
		User:         "cenvero-agent",
		EnrollSecret: testReverseEnroll,
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
	queue := &persistenceCheckingMetricsQueue{
		MetricsQueue: agent.NewFileMetricsQueue(queuePath),
		app:          app,
		server:       "reverse-node",
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agentErrCh := make(chan error, 1)
	go func() {
		agentErrCh <- agent.RunReverse(ctx, agent.ReverseOptions{
			EnrollToken:            testReverseEnroll,
			ControllerFingerprint:  testControllerFingerprint(t, app),
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
			MetricsQueue:     queue,
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
	if queue.acknowledgements.Load() == 0 {
		t.Fatal("expected persisted metrics batch to be acknowledged")
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

func TestReverseHubClearSessionIgnoresStaleDisconnect(t *testing.T) {
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
		Name:         "reverse-node",
		Address:      "unknown",
		Mode:         transport.ModeReverse,
		User:         "cenvero-agent",
		EnrollSecret: testReverseEnroll,
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	hub := NewReverseHub(app, "test-token")
	defer hub.Close()

	oldSession := &transport.Session{Closer: reverseTestCloser{}}
	newSession := &transport.Session{Closer: reverseTestCloser{}}
	hub.setSession("reverse-node", oldSession, ReverseSessionInfo{
		Server:             "reverse-node",
		Connected:          true,
		HostKeyFingerprint: "old",
		Hello:              proto.HelloPayload{AgentVersion: "v1.2.3"},
	}, nil)
	hub.setSession("reverse-node", newSession, ReverseSessionInfo{
		Server:             "reverse-node",
		Connected:          true,
		HostKeyFingerprint: "new",
		Hello:              proto.HelloPayload{AgentVersion: "v1.2.3"},
	}, nil)

	hub.clearSession("reverse-node", "stale disconnect", oldSession)

	info, err := hub.Status("reverse-node")
	if err != nil {
		t.Fatalf("expected replacement reverse session to remain connected: %v", err)
	}
	if info.HostKeyFingerprint != "new" {
		t.Fatalf("expected replacement session to remain current, got %#v", info)
	}
	record, err := app.GetServer("reverse-node")
	if err != nil {
		t.Fatalf("GetServer() error = %v", err)
	}
	if !record.Observed.Reachable || record.Observed.LastError == "stale disconnect" {
		t.Fatalf("stale disconnect mutated live server observation: %#v", record.Observed)
	}

	hub.clearSession("reverse-node", "fresh disconnect", newSession)
	if _, err := hub.Status("reverse-node"); err == nil {
		t.Fatalf("expected current session to be cleared")
	}
}

type reverseTestCloser struct{}

func (reverseTestCloser) Close() error {
	return nil
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

type persistenceCheckingMetricsQueue struct {
	agent.MetricsQueue
	app              *App
	server           string
	acknowledgements atomic.Int32
}

func (q *persistenceCheckingMetricsQueue) Acknowledge(batchID string) error {
	entries, err := q.app.MetricsDB.ListMetricSnapshots(q.server, 1)
	if err != nil {
		return fmt.Errorf("verify persistence before acknowledgement: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("metrics queue acknowledged before controller persistence")
	}
	if err := q.MetricsQueue.Acknowledge(batchID); err != nil {
		return err
	}
	q.acknowledgements.Add(1)
	return nil
}

func TestReverseNoDeadlineCancellationKillsProcessAndKeepsSession(t *testing.T) {
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
		Name: "cancel-node", Address: "unknown", Mode: transport.ModeReverse,
		User: "cenvero-agent", EnrollSecret: testReverseEnroll,
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	hub := NewReverseHub(app, "test-token")
	defer hub.Close()
	controlListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen reverse control: %v", err)
	}
	app.Config.Runtime.ControlAddress = controlListener.Addr().String()
	if err := os.WriteFile(filepath.Join(configDir, "data", "control.token"), []byte("test-token"), 0o600); err != nil {
		t.Fatalf("write control token: %v", err)
	}
	controlCtx, stopControl := context.WithCancel(context.Background())
	defer stopControl()
	controlErrCh := make(chan error, 1)
	go func() { controlErrCh <- hub.ServeControl(controlCtx, controlListener) }()

	clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:41003", "127.0.0.1:9443")
	controllerErrCh := make(chan error, 1)
	agentErrCh := make(chan error, 1)
	runCtx, stopAgent := context.WithCancel(context.Background())
	defer stopAgent()
	knownHostsPath := filepath.Join(t.TempDir(), "controller_known_hosts")
	agentKeyPath := filepath.Join(t.TempDir(), "agent_reverse_key")
	fingerprint := testControllerFingerprint(t, app)
	go func() { controllerErrCh <- hub.ServeConn(serverConn) }()
	go func() {
		agentErrCh <- agent.RunReverse(runCtx, agent.ReverseOptions{
			EnrollToken:           testReverseEnroll,
			ControllerFingerprint: fingerprint,
			ControllerAddress:     "127.0.0.1:9443",
			ServerName:            "cancel-node",
			KnownHostsPath:        knownHostsPath,
			NetworkDialContext: func(context.Context, string, string) (net.Conn, error) {
				return clientConn, nil
			},
		}, agent.Server{Mode: transport.ModeReverse, HostKeyPath: agentKeyPath})
	}()
	waitForReverseSession(t, hub, "cancel-node")

	marker := filepath.Join(t.TempDir(), "reverse-cancel-marker")
	started := filepath.Join(t.TempDir(), "reverse-cancel-started")
	execCtx, cancelExec := context.WithCancel(context.Background())
	execDone := make(chan error, 1)
	go func() {
		_, err := app.ExecCommandContext(execCtx, "cancel-node", fmt.Sprintf("touch %q; sleep 0.5; touch %q", started, marker))
		execDone <- err
	}()
	waitForTestPath(t, started)
	cancelExec()
	select {
	case err := <-execDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("reverse cancelled exec error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reverse deadline-free cancelled exec did not return")
	}
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("reverse cancelled process group survived and created marker: %v", err)
	}
	if _, err := hub.Status("cancel-node"); err != nil {
		t.Fatalf("reverse session was retired by clean cancellation: %v", err)
	}
	result, err := app.ExecCommandContext(context.Background(), "cancel-node", "printf reusable")
	if err != nil || result.Stdout != "reusable" {
		t.Fatalf("post-cancellation reverse exec = %#v, err=%v", result, err)
	}

	stopControl()
	select {
	case err := <-controlErrCh:
		if err != nil {
			t.Fatalf("reverse control server exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reverse control server did not exit")
	}
	stopAgent()
	hub.Close()
	_ = clientConn.Close()
	select {
	case err := <-agentErrCh:
		if err != nil {
			t.Fatalf("agent reverse connector exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent reverse connector did not exit")
	}
	select {
	case err := <-controllerErrCh:
		if err != nil {
			t.Fatalf("controller reverse hub exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("controller reverse hub did not exit")
	}
}

func waitForTestPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("path %s was not created before timeout", path)
}

func TestReplayQueuedMetricsOldAgentSkipsLegacyDestructiveFlush(t *testing.T) {
	hub := &ReverseHub{}
	// The nil-channel session would fail immediately if replay attempted any RPC.
	// No capability must therefore return cleanly without probing flush_queue.
	replayed, err := hub.replayQueuedMetrics("old-node", &transport.Session{}, []string{"metrics.collect"})
	if err != nil {
		t.Fatalf("old-agent replay gate returned error: %v", err)
	}
	if replayed != 0 {
		t.Fatalf("old-agent replayed = %d, want 0", replayed)
	}
}

func TestReverseMetricsPersistenceFailureLeavesAgentQueue(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeReverse,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer app.Close()
	if err := app.AddServer(ServerRecord{
		Name: "persist-fail", Address: "unknown", Mode: transport.ModeReverse,
		User: "cenvero-agent", EnrollSecret: testReverseEnroll,
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}
	if err := app.MetricsDB.Close(); err != nil {
		t.Fatalf("close metrics database: %v", err)
	}

	queuePath := filepath.Join(t.TempDir(), "metrics.jsonl")
	queue := agent.NewFileMetricsQueue(queuePath)
	if err := queue.Enqueue(proto.MetricsSnapshot{Timestamp: time.Now().UTC(), ProcessCount: 7}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	hub := NewReverseHub(app, "test-token")
	defer hub.Close()
	clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:41004", "127.0.0.1:9443")
	runCtx, stopAgent := context.WithCancel(context.Background())
	defer stopAgent()
	controllerErrCh := make(chan error, 1)
	agentErrCh := make(chan error, 1)
	knownHostsPath := filepath.Join(t.TempDir(), "controller_known_hosts")
	agentKeyPath := filepath.Join(t.TempDir(), "agent_reverse_key")
	fingerprint := testControllerFingerprint(t, app)
	go func() { controllerErrCh <- hub.ServeConn(serverConn) }()
	go func() {
		agentErrCh <- agent.RunReverse(runCtx, agent.ReverseOptions{
			EnrollToken: testReverseEnroll, ControllerFingerprint: fingerprint,
			ControllerAddress: "127.0.0.1:9443", ServerName: "persist-fail",
			KnownHostsPath:     knownHostsPath,
			NetworkDialContext: func(context.Context, string, string) (net.Conn, error) { return clientConn, nil },
		}, agent.Server{Mode: transport.ModeReverse, HostKeyPath: agentKeyPath, MetricsQueue: queue})
	}()
	waitForReverseSession(t, hub, "persist-fail")

	batch, err := queue.Peek()
	if err != nil {
		t.Fatalf("Peek() after persistence failure: %v", err)
	}
	if batch.ID == "" || len(batch.Snapshots) != 1 || batch.Snapshots[0].ProcessCount != 7 {
		t.Fatalf("persistence failure lost queued metrics: %#v", batch)
	}

	stopAgent()
	hub.Close()
	_ = clientConn.Close()
	select {
	case err := <-agentErrCh:
		if err != nil {
			t.Fatalf("agent reverse connector exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent reverse connector did not exit")
	}
	select {
	case err := <-controllerErrCh:
		if err != nil {
			t.Fatalf("controller reverse hub exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("controller reverse hub did not exit")
	}
}
