// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"net"
	"path/filepath"
	"testing"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

func TestLiveServiceListAndControl(t *testing.T) {
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

	manager := &fakeServiceManager{
		services: []proto.ServiceInfo{
			{Name: "nginx.service", LoadState: "loaded", ActiveState: "active", SubState: "running", Description: "nginx"},
			{Name: "sshd.service", LoadState: "loaded", ActiveState: "active", SubState: "running", Description: "ssh"},
		},
	}
	server := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
		ServiceManager:     manager,
	}
	errCh := make(chan error, 1)

	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40000", "127.0.0.1:2222")
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

	services, err := app.ListServices("loopback")
	if err != nil {
		t.Fatalf("ListServices() error = %v", err)
	}
	if len(services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(services))
	}
	if services[0].ActiveState == "" {
		t.Fatalf("expected active state to be populated")
	}

	if err := app.ControlService("loopback", "nginx.service", "restart"); err != nil {
		t.Fatalf("ControlService() error = %v", err)
	}
	if len(manager.actions) != 1 || manager.actions[0] != "restart:nginx.service" {
		t.Fatalf("unexpected service control actions: %#v", manager.actions)
	}

	record, err := app.GetServer("loopback")
	if err != nil {
		t.Fatalf("GetServer() error = %v", err)
	}
	if len(record.Services) != 1 {
		t.Fatalf("expected one tracked service after control, got %d", len(record.Services))
	}
	if record.Services[0].LastAction != "restart" {
		t.Fatalf("expected tracked service last action to be restart, got %s", record.Services[0].LastAction)
	}
	if record.Services[0].ActiveState != "active" {
		t.Fatalf("expected tracked service state to be updated, got %s", record.Services[0].ActiveState)
	}

	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("agent server exited with error: %v", err)
		}
	}
}

type fakeServiceManager struct {
	services []proto.ServiceInfo
	actions  []string
}

func (f *fakeServiceManager) List(context.Context) ([]proto.ServiceInfo, error) {
	return f.services, nil
}

func (f *fakeServiceManager) Control(_ context.Context, service, action string) (proto.ServiceInfo, error) {
	f.actions = append(f.actions, action+":"+service)
	for _, candidate := range f.services {
		if candidate.Name == service {
			candidate.ActiveState = "active"
			candidate.SubState = "running"
			return candidate, nil
		}
	}
	return proto.ServiceInfo{Name: service, ActiveState: "active", SubState: "running"}, nil
}
