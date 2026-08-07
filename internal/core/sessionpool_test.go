// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

// pooledTestFleet spins up an in-process agent behind a counting dialer so a test
// or benchmark can assert how many SSH connections the controller established.
func pooledTestFleet(t testing.TB) (app *App, dials *atomic.Int64, errCh chan error) {
	t.Helper()
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
	t.Cleanup(func() { _ = app.Close() })

	srv := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
		ServiceManager: &fakeServiceManager{services: []proto.ServiceInfo{
			{Name: "nginx.service", LoadState: "loaded", ActiveState: "active"},
		}},
	}
	dials = &atomic.Int64{}
	errCh = make(chan error, 16)
	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40000", "127.0.0.1:2222")
		go func() { errCh <- srv.ServeConn(serverConn) }()
		return clientConn, nil
	}
	if err := app.AddServer(ServerRecord{
		Name: "loopback", Address: "127.0.0.1", Port: 2222,
		Mode: transport.ModeDirect, User: "cenvero-agent",
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}
	return app, dials, errCh
}

// TestPooledSessionsReuseOneConnection is the point of the pool: a burst of
// control RPCs must ride a single SSH connection rather than handshaking each
// time. Before pooling this cost one full connection setup per call, which is
// what made remote directory browsing feel slow.
func TestPooledSessionsReuseOneConnection(t *testing.T) {
	t.Parallel()
	app, dials, errCh := pooledTestFleet(t)

	const calls = 8
	for range calls {
		if _, err := app.ListServices("loopback"); err != nil {
			t.Fatalf("ListServices() error = %v", err)
		}
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("expected %d RPCs to share 1 connection, got %d dials", calls, got)
	}

	app.DisconnectPooledSessions()
	drainAgentServeErrs(t, errCh)
}

// TestPooledSessionsSurviveConnectionLoss covers the failure the pool
// introduces: a cached connection can die between calls (agent restart, NAT
// eviction). The next call must transparently redial instead of surfacing the
// dead connection as an error.
func TestPooledSessionsSurviveConnectionLoss(t *testing.T) {
	t.Parallel()
	app, dials, errCh := pooledTestFleet(t)

	if _, err := app.ListServices("loopback"); err != nil {
		t.Fatalf("first ListServices() error = %v", err)
	}
	// Kill the pooled connection behind the caller's back, exactly as a remote
	// restart would.
	app.DisconnectPooledSessions()

	if _, err := app.ListServices("loopback"); err != nil {
		t.Fatalf("ListServices() after connection loss error = %v", err)
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("expected a redial after the connection dropped, got %d dials", got)
	}

	app.DisconnectPooledSessions()
	drainAgentServeErrs(t, errCh)
}

// TestPooledSessionsConcurrentCalls checks that parallel callers to the same
// server are served safely — they multiplex extra channels onto the one
// connection rather than corrupting a shared one.
func TestPooledSessionsConcurrentCalls(t *testing.T) {
	t.Parallel()
	app, dials, errCh := pooledTestFleet(t)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			if _, err := app.ListServices("loopback"); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent ListServices() error = %v", err)
	}
	// Channels multiplex over one connection, so this must not fan out into one
	// connection per caller.
	if got := dials.Load(); got > 2 {
		t.Fatalf("expected concurrent calls to multiplex, got %d dials", got)
	}

	app.DisconnectPooledSessions()
	drainAgentServeErrs(t, errCh)
}

// TestRemoveServerDropsPooledSession makes sure a removed server leaves no
// cached connection behind.
func TestRemoveServerDropsPooledSession(t *testing.T) {
	t.Parallel()
	app, _, errCh := pooledTestFleet(t)

	if _, err := app.ListServices("loopback"); err != nil {
		t.Fatalf("ListServices() error = %v", err)
	}
	if _, ok := app.sessions.acquire("loopback"); !ok {
		t.Fatal("expected a pooled connection after an RPC")
	}
	app.sessions.entries["loopback"].idle = append(app.sessions.entries["loopback"].idle,
		app.sessions.entries["loopback"].root)

	if err := app.RemoveServerWithOptions("loopback", RemoveOptions{Force: true}); err != nil {
		t.Fatalf("RemoveServerWithOptions() error = %v", err)
	}
	if _, ok := app.sessions.acquire("loopback"); ok {
		t.Fatal("pooled connection survived server removal")
	}
	drainAgentServeErrs(t, errCh)
}
