// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

func TestApplyTemplateExecutesLiveChanges(t *testing.T) {
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

	templateBody := `
name = "web stack"
description = "Web services and ingress ports"

[firewall]
enabled = true
open_ports = [80, 443]
rules = ["allow 8443/tcp"]

[[services]]
name = "nginx.service"
log_path = "/var/log/nginx/access.log"
critical = true
action = "restart"

[[services]]
name = "sshd.service"
critical = false
action = "start"
`
	if err := os.WriteFile(filepath.Join(configDir, "templates", "web.toml"), []byte(templateBody), 0o644); err != nil {
		t.Fatalf("WriteFile(template) error = %v", err)
	}

	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer app.Close()

	serviceManager := &fakeServiceManager{
		services: []proto.ServiceInfo{
			{Name: "nginx.service", LoadState: "loaded", ActiveState: "active", SubState: "running", Description: "nginx"},
			{Name: "sshd.service", LoadState: "loaded", ActiveState: "active", SubState: "running", Description: "ssh"},
		},
	}
	firewallManager := &fakeFirewallManager{
		status: proto.FirewallInfo{
			Enabled:   false,
			Rules:     []string{},
			OpenPorts: []int{},
		},
	}
	server := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
		ServiceManager:     serviceManager,
		FirewallManager:    firewallManager,
	}
	errCh := make(chan error, 1)

	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40011", "127.0.0.1:2222")
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

	if err := app.ApplyTemplate("loopback", "web.toml"); err != nil {
		t.Fatalf("ApplyTemplate() error = %v", err)
	}

	record, err := app.GetServer("loopback")
	if err != nil {
		t.Fatalf("GetServer() error = %v", err)
	}
	if record.LastTemplate != "web.toml" {
		t.Fatalf("expected last template web.toml, got %q", record.LastTemplate)
	}
	if len(record.Services) != 2 {
		t.Fatalf("expected 2 tracked services, got %d", len(record.Services))
	}
	if record.Services[0].Name != "nginx.service" && record.Services[1].Name != "nginx.service" {
		t.Fatalf("expected nginx.service to be tracked, got %#v", record.Services)
	}
	if !record.Firewall.Enabled {
		t.Fatalf("expected firewall to be enabled")
	}
	if !slices.Contains(record.OpenPorts, 80) || !slices.Contains(record.OpenPorts, 443) {
		t.Fatalf("expected ports 80 and 443 to be opened, got %#v", record.OpenPorts)
	}
	if !slices.Contains(record.Firewall.Rules, "allow 8443/tcp") {
		t.Fatalf("expected firewall rule to be applied, got %#v", record.Firewall.Rules)
	}
	if len(serviceManager.actions) != 2 {
		t.Fatalf("expected 2 service actions, got %#v", serviceManager.actions)
	}
	if !slices.Contains(serviceManager.actions, "restart:nginx.service") || !slices.Contains(serviceManager.actions, "start:sshd.service") {
		t.Fatalf("unexpected service actions %#v", serviceManager.actions)
	}
	if len(firewallManager.actions) != 4 {
		t.Fatalf("expected 4 firewall actions, got %#v", firewallManager.actions)
	}

	for range 4 + 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("agent server exited with error: %v", err)
		}
	}
}

func TestLoadTemplateRejectsUnsupportedServiceAction(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")
	body := `
[[services]]
name = "nginx.service"
action = "reload"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(template) error = %v", err)
	}
	if _, err := LoadTemplate(path); err == nil {
		t.Fatalf("expected LoadTemplate() to reject unsupported action")
	}
}
