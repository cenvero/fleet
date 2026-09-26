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
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

// countingListener counts the TCP connections an agent accepts.
type countingListener struct {
	net.Listener
	accepted atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return conn, err
}

type relayFleet struct {
	configDir string
	cli       *App // a one-shot CLI process: no custom dialer, so it may relay
	daemon    *App
	hub       *ReverseHub
	agentConn *countingListener
	stopCtl   func()
}

// newRelayFleet starts a real direct-mode agent on loopback TCP, a "daemon"
// App serving the control socket, and a separate "CLI" App on the same config
// directory — the shape of `fleet exec` running next to `fleet daemon`.
func newRelayFleet(t *testing.T) *relayFleet {
	t.Helper()
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeDirect,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	counting := &countingListener{Listener: ln}
	srv := agent.Server{
		Mode: transport.ModeDirect, HostKeyPath: filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
		ServiceManager:     &fakeServiceManager{services: []proto.ServiceInfo{{Name: "nginx.service"}}},
	}
	agentCtx, stopAgent := context.WithCancel(context.Background())
	agentDone := make(chan struct{})
	go func() { defer close(agentDone); _ = srv.Serve(agentCtx, counting) }()
	t.Cleanup(func() { stopAgent(); <-agentDone })

	cli, err := Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	if err := cli.AddServer(ServerRecord{Name: "web", Address: "127.0.0.1", Port: port, Mode: transport.ModeDirect, User: "cenvero-agent"}); err != nil {
		t.Fatal(err)
	}
	if err := cli.ReconnectServer("web", false); err != nil { // pins the host key
		t.Fatalf("ReconnectServer: %v", err)
	}

	daemon, err := Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Close() })
	token, err := daemon.generateControlToken()
	if err != nil {
		t.Fatal(err)
	}
	control, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cli.Config.Runtime.ControlAddress = control.Addr().String()
	daemon.Config.Runtime.ControlAddress = control.Addr().String()
	hub := NewReverseHub(daemon, token)
	daemonApps.Store(daemon, struct{}{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = hub.ServeControl(ctx, control) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(func() { stop(); daemonApps.Delete(daemon) })
	directRelayUnreachable.Delete(control.Addr().String())
	return &relayFleet{configDir: configDir, cli: cli, daemon: daemon, hub: hub, agentConn: counting, stopCtl: stop}
}

func TestDirectCallsRelayThroughDaemonPool(t *testing.T) {
	f := newRelayFleet(t)
	base := f.agentConn.accepted.Load()
	for i := 0; i < 5; i++ {
		services, err := f.cli.ListServices("web")
		if err != nil || len(services) != 1 {
			t.Fatalf("relayed call %d = %v, err=%v", i, services, err)
		}
	}
	if got := f.agentConn.accepted.Load() - base; got != 1 {
		t.Fatalf("5 calls opened %d connections to the server; want the daemon's single pooled one", got)
	}
	if f.cli.sessions.has("web") {
		t.Fatal("the CLI dialled the server itself instead of relaying")
	}
	if !f.daemon.sessions.has("web") {
		t.Fatal("the daemon did not pool the relayed connection")
	}
	// A second "process" (fresh App, empty pool) rides the same connection.
	other, err := Open(f.configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.Config.Runtime.ControlAddress = f.cli.Config.Runtime.ControlAddress
	if _, err := other.ListServices("web"); err != nil {
		t.Fatal(err)
	}
	if got := f.agentConn.accepted.Load() - base; got != 1 {
		t.Fatalf("a second CLI process opened its own connection (%d total)", got)
	}
}

func TestDirectCallsFallBackWithoutDaemon(t *testing.T) {
	f := newRelayFleet(t)
	f.stopCtl()
	base := f.agentConn.accepted.Load()
	if _, err := f.cli.ListServices("web"); err != nil {
		t.Fatalf("call with no daemon running: %v", err)
	}
	if got := f.agentConn.accepted.Load() - base; got != 1 || !f.cli.sessions.has("web") {
		t.Fatalf("expected the CLI to dial the server itself (connections=%d)", got)
	}
}

func TestDirectCallsFallBackOnTargetMismatch(t *testing.T) {
	f := newRelayFleet(t)
	// The daemon resolves a different known_hosts file than the CLI: it must
	// refuse to vouch for the connection, and the CLI dials with its own pins.
	f.daemon.Config.Crypto.KnownHostsPath = filepath.Join(t.TempDir(), "other_known_hosts")
	if _, err := f.cli.ListServices("web"); err != nil {
		t.Fatalf("call after a target mismatch: %v", err)
	}
	if f.daemon.sessions.has("web") || !f.cli.sessions.has("web") {
		t.Fatal("a mismatched target was relayed instead of dialled by the CLI")
	}
}

func TestDirectCallsFallBackForOldDaemon(t *testing.T) {
	f := newRelayFleet(t)
	legacyApp, requests, _ := legacyDaemonApp(t, nil)
	f.cli.Config.Runtime.ControlAddress = legacyApp.Config.Runtime.ControlAddress
	if err := os.WriteFile(f.cli.controlTokenPath(), []byte("legacy-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.cli.ListServices("web"); err != nil {
		t.Fatalf("call next to an older daemon: %v", err)
	}
	if !f.cli.sessions.has("web") {
		t.Fatal("the CLI should have dialled the server itself")
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("older daemon saw %d connections; a direct call must never be offered to a daemon that cannot authenticate itself", n)
	}
}

func TestDirectRelayCanBeDisabled(t *testing.T) {
	f := newRelayFleet(t)
	t.Setenv("FLEET_NO_DAEMON_RELAY", "1")
	if _, err := f.cli.ListServices("web"); err != nil {
		t.Fatal(err)
	}
	if f.daemon.sessions.has("web") || !f.cli.sessions.has("web") {
		t.Fatal("FLEET_NO_DAEMON_RELAY did not keep the call local")
	}
}

// A key rotation or host-key re-pin done by another process writes the key or
// known_hosts file; the daemon must not keep riding a connection that was
// authenticated before that.
func TestDirectRelayRedialsAfterCredentialChange(t *testing.T) {
	f := newRelayFleet(t)
	if _, err := f.cli.ListServices("web"); err != nil {
		t.Fatal(err)
	}
	base := f.agentConn.accepted.Load()
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(f.cli.Config.Crypto.KnownHostsPath, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := f.cli.ListServices("web"); err != nil {
		t.Fatal(err)
	}
	if got := f.agentConn.accepted.Load() - base; got != 1 {
		t.Fatalf("daemon reused a connection established before known_hosts changed (%d new connections)", got)
	}
}

// Cancelling a relayed call cancels it on the server, and a relayed call is
// not cut short by the control socket's 30s I/O timeout: it runs under the
// caller's own deadline.
func TestDirectRelayHonoursCancellationAndCallerDeadline(t *testing.T) {
	f := newRelayFleet(t)
	marker := filepath.Join(t.TempDir(), "relay-marker")
	started := filepath.Join(t.TempDir(), "relay-started")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.cli.ExecCommandContext(ctx, "web", fmt.Sprintf("touch %q; sleep 1; touch %q", started, marker))
		done <- err
	}()
	waitForTestPath(t, started)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled relayed exec = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled relayed exec did not return")
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("remote process survived the cancelled relay: %v", err)
	}
	if !f.daemon.sessions.has("web") {
		t.Fatal("the call went direct instead of through the daemon")
	}

	// Direct calls keep their own deadline semantics through the relay.
	deadlineCtx, cancelDeadline := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancelDeadline()
	_, err := f.cli.ExecCommandContext(deadlineCtx, "web", "sleep 2")
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(deadlineCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("relayed exec past its deadline = %v", err)
	}
	if result, err := f.cli.ExecCommandContext(context.Background(), "web", "printf ok"); err != nil || result.Stdout != "ok" {
		t.Fatalf("relayed exec after a timeout = %+v, %v", result, err)
	}
}

func TestDaemonNeverRelaysToItself(t *testing.T) {
	f := newRelayFleet(t)
	server, err := f.daemon.GetServer("web")
	if err != nil {
		t.Fatal(err)
	}
	if f.daemon.directRelayEnabled(server) {
		t.Fatal("the daemon would relay its own calls to its own control socket")
	}
	if !f.cli.directRelayEnabled(server) {
		t.Fatal("a CLI process next to a daemon should relay")
	}
}
