// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
)

// dropAfterFirewall is a firewall that does its work and then cuts the
// connection before the agent can reply, as a network drop would.
type dropAfterFirewall struct {
	enables *atomic.Int32
	drop    func()
}

func (f dropAfterFirewall) Status(context.Context) (proto.FirewallInfo, error) {
	return proto.FirewallInfo{Enabled: false}, nil
}
func (f dropAfterFirewall) Enable(context.Context, bool) (proto.FirewallInfo, error) {
	f.enables.Add(1)
	f.drop()
	return proto.FirewallInfo{Enabled: true}, nil
}
func (f dropAfterFirewall) AddRule(context.Context, string) (proto.FirewallInfo, error) {
	return proto.FirewallInfo{}, nil
}
func (f dropAfterFirewall) ListOpenPorts(context.Context) ([]int, error) { return nil, nil }
func (f dropAfterFirewall) SetPort(context.Context, int, bool) (proto.FirewallInfo, error) {
	return proto.FirewallInfo{}, nil
}

// dropOnceCollector cuts the connection on its first collection only.
type dropOnceCollector struct {
	calls *atomic.Int32
	drop  func()
}

func (c dropOnceCollector) Collect(context.Context) (proto.MetricsSnapshot, error) {
	if c.calls.Add(1) == 1 {
		c.drop()
	}
	return proto.MetricsSnapshot{CPUPercent: 5}, nil
}

// When a pooled connection drops after a request went out, the request may
// already have run. The pool used to redial and send it again, so a command,
// a service restart or a firewall change could run twice. Now only requests
// that never reached the agent, or that only read state, are sent again.
func TestPooledCallIsNotResentAfterItMayHaveRun(t *testing.T) {
	app := newImportTestApp(t)
	var mu sync.Mutex
	var conns []net.Conn
	dropAll := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	}
	var enables, collects atomic.Int32
	srv := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(app.ConfigDir, "keys", "id_ed25519.pub"),
		FirewallManager:    dropAfterFirewall{enables: &enables, drop: dropAll},
		MetricsCollector:   dropOnceCollector{calls: &collects, drop: dropAll},
	}
	app.NetworkDialContext = func(_ context.Context, _, address string) (net.Conn, error) {
		client, server := testutil.NewBufferedConnPair("127.0.0.1:50200", address)
		mu.Lock()
		conns = append(conns, server)
		mu.Unlock()
		go func() { _ = srv.ServeConn(server) }()
		return client, nil
	}
	t.Cleanup(app.DisconnectPooledSessions)
	record := ServerRecord{Name: "web-01", Address: "127.0.0.1", Port: 2222, Mode: transport.ModeDirect, User: "cenvero-agent"}
	if err := app.AddServer(record); err != nil {
		t.Fatal(err)
	}
	record, _ = app.GetServer("web-01")

	// Warm the pool, so the next call goes out on a pooled connection.
	if _, err := app.callRPC(record, proto.Envelope{Action: "firewall.status"}); err != nil {
		t.Fatalf("firewall.status: %v", err)
	}

	_, err := app.callRPC(record, proto.Envelope{Action: "firewall.enable"})
	if got := enables.Load(); got != 1 {
		t.Fatalf("firewall.enable ran %d times, want exactly once", got)
	}
	if err == nil || !strings.Contains(err.Error(), "may have run") {
		t.Fatalf("firewall.enable error = %v, want a may-have-run error", err)
	}
	if transport.RequestNotSent(err) {
		t.Fatal("a request that may have run is reported as not sent, so --retry would run it again")
	}

	// A read-only call that hits the same drop is still retried transparently.
	if _, err := app.callRPC(record, proto.Envelope{Action: "firewall.status"}); err != nil {
		t.Fatalf("re-warm: %v", err)
	}
	resp, err := app.callRPC(record, proto.Envelope{Action: "metrics.collect"})
	if err != nil || resp.Error != nil {
		t.Fatalf("metrics.collect after a drop: %v %+v, want a transparent retry", err, resp.Error)
	}
	if got := collects.Load(); got != 2 {
		t.Fatalf("metrics.collect ran %d times, want 2 (the dropped one and the retry)", got)
	}
}
