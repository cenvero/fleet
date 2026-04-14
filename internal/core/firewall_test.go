// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

func TestLiveFirewallAndPortManagement(t *testing.T) {
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

	manager := &fakeFirewallManager{
		status: proto.FirewallInfo{
			Enabled:   true,
			Rules:     []string{"22/tcp ALLOW Anywhere"},
			OpenPorts: []int{22},
		},
	}
	server := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
		FirewallManager:    manager,
	}
	errCh := make(chan error, 1)

	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40002", "127.0.0.1:2222")
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

	state, err := app.FirewallStatus("loopback")
	if err != nil {
		t.Fatalf("FirewallStatus() error = %v", err)
	}
	if !state.Enabled {
		t.Fatalf("expected firewall to be enabled")
	}

	ports, err := app.ListPorts("loopback")
	if err != nil {
		t.Fatalf("ListPorts() error = %v", err)
	}
	if !slices.Equal(ports, []int{22}) {
		t.Fatalf("unexpected initial ports %#v", ports)
	}

	if err := app.SetPort("loopback", 443, true); err != nil {
		t.Fatalf("SetPort(open) error = %v", err)
	}
	if err := app.AddFirewallRule("loopback", "allow 8443/tcp"); err != nil {
		t.Fatalf("AddFirewallRule() error = %v", err)
	}
	if err := app.SetFirewall("loopback", false); err != nil {
		t.Fatalf("SetFirewall(disable) error = %v", err)
	}

	record, err := app.GetServer("loopback")
	if err != nil {
		t.Fatalf("GetServer() error = %v", err)
	}
	if record.Firewall.Enabled {
		t.Fatalf("expected persisted firewall to be disabled")
	}
	if !slices.Contains(record.OpenPorts, 443) {
		t.Fatalf("expected persisted open ports to include 443, got %#v", record.OpenPorts)
	}
	if len(manager.actions) != 5 {
		t.Fatalf("expected 5 manager actions, got %#v", manager.actions)
	}

	for range 5 {
		if err := <-errCh; err != nil {
			t.Fatalf("agent server exited with error: %v", err)
		}
	}
}

type fakeFirewallManager struct {
	status  proto.FirewallInfo
	actions []string
}

func (f *fakeFirewallManager) Status(context.Context) (proto.FirewallInfo, error) {
	f.actions = append(f.actions, "status")
	return f.clone(), nil
}

func (f *fakeFirewallManager) Enable(_ context.Context, enabled bool) (proto.FirewallInfo, error) {
	f.actions = append(f.actions, fmt.Sprintf("enable:%t", enabled))
	f.status.Enabled = enabled
	return f.clone(), nil
}

func (f *fakeFirewallManager) AddRule(_ context.Context, rule string) (proto.FirewallInfo, error) {
	f.actions = append(f.actions, "rule:"+rule)
	f.status.Rules = append(f.status.Rules, rule)
	return f.clone(), nil
}

func (f *fakeFirewallManager) ListOpenPorts(context.Context) ([]int, error) {
	f.actions = append(f.actions, "ports")
	return append([]int(nil), f.status.OpenPorts...), nil
}

func (f *fakeFirewallManager) SetPort(_ context.Context, port int, open bool) (proto.FirewallInfo, error) {
	f.actions = append(f.actions, fmt.Sprintf("port:%d:%t", port, open))
	if open && !slices.Contains(f.status.OpenPorts, port) {
		f.status.OpenPorts = append(f.status.OpenPorts, port)
		slices.Sort(f.status.OpenPorts)
	}
	if !open {
		next := f.status.OpenPorts[:0]
		for _, candidate := range f.status.OpenPorts {
			if candidate != port {
				next = append(next, candidate)
			}
		}
		f.status.OpenPorts = next
	}
	return f.clone(), nil
}

func (f *fakeFirewallManager) clone() proto.FirewallInfo {
	return proto.FirewallInfo{
		Enabled:   f.status.Enabled,
		Rules:     append([]string(nil), f.status.Rules...),
		OpenPorts: append([]int(nil), f.status.OpenPorts...),
	}
}
