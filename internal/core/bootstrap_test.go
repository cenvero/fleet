// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
)

func TestBootstrapServerDirectUploadsAgentAndUpdatesServer(t *testing.T) {
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

	if err := app.AddServer(ServerRecord{
		Name:    "web-01",
		Address: "192.0.2.10",
		Mode:    transport.ModeDirect,
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	agentBinary := filepath.Join(t.TempDir(), "fleet-agent")
	if err := os.WriteFile(agentBinary, []byte("binary"), 0o755); err != nil {
		t.Fatalf("WriteFile(agentBinary) error = %v", err)
	}

	executor := &fakeBootstrapExecutor{}
	app.BootstrapExecutor = executor

	result, err := app.BootstrapServer("web-01", BootstrapOptions{
		LoginUser:       "ubuntu",
		LoginPort:       22,
		AgentBinaryPath: agentBinary,
		UseSudo:         true,
	})
	if err != nil {
		t.Fatalf("BootstrapServer() error = %v", err)
	}
	if !result.Executed {
		t.Fatalf("expected bootstrap result to be marked executed")
	}
	if len(executor.requests) != 1 {
		t.Fatalf("expected one bootstrap request, got %d", len(executor.requests))
	}
	if got := len(executor.requests[0].Uploads); got != 4 {
		t.Fatalf("expected 4 uploads for direct bootstrap, got %d", got)
	}
	if !strings.Contains(result.ServiceUnit, "serve --mode direct") {
		t.Fatalf("expected direct mode service unit, got %q", result.ServiceUnit)
	}
	if result.Script != "" {
		t.Fatalf("expected executed bootstrap result to omit the raw script payload")
	}
	if !strings.Contains(string(executor.requests[0].Uploads[len(executor.requests[0].Uploads)-1].Content), "systemctl enable --now") {
		t.Fatalf("expected uploaded bootstrap script to enable systemd service")
	}

	server, err := app.GetServer("web-01")
	if err != nil {
		t.Fatalf("GetServer() error = %v", err)
	}
	if server.User != "root" {
		t.Fatalf("expected server user %q, got %q", "root", server.User)
	}
	if server.Port != 2222 {
		t.Fatalf("expected direct-mode agent port 2222, got %d", server.Port)
	}
}

func TestBootstrapServerReversePrintScript(t *testing.T) {
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
		Name:    "edge-01",
		Address: "198.51.100.25",
		Mode:    transport.ModeReverse,
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	agentBinary := filepath.Join(t.TempDir(), "fleet-agent")
	if err := os.WriteFile(agentBinary, []byte("binary"), 0o755); err != nil {
		t.Fatalf("WriteFile(agentBinary) error = %v", err)
	}

	result, err := app.BootstrapServer("edge-01", BootstrapOptions{
		AgentBinaryPath:   agentBinary,
		ControllerAddress: "203.0.113.7:9443",
		PrintScript:       true,
		UseSudo:           true,
	})
	if err != nil {
		t.Fatalf("BootstrapServer(printScript) error = %v", err)
	}
	if result.Executed {
		t.Fatalf("expected print-script result to avoid execution")
	}
	if !strings.Contains(result.ServiceUnit, "reverse --mode reverse") {
		t.Fatalf("expected reverse mode service unit, got %q", result.ServiceUnit)
	}
	if !strings.Contains(result.ServiceUnit, "203.0.113.7:9443") {
		t.Fatalf("expected controller address in reverse service unit")
	}
	if !strings.Contains(result.Script, "Bootstrapped Cenvero Fleet agent on edge-01") {
		t.Fatalf("expected bootstrap script banner in output")
	}
}

func TestBootstrapServerReverseRequiresControllerAddress(t *testing.T) {
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
		Name:    "edge-02",
		Address: "198.51.100.26",
		Mode:    transport.ModeReverse,
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	agentBinary := filepath.Join(t.TempDir(), "fleet-agent")
	if err := os.WriteFile(agentBinary, []byte("binary"), 0o755); err != nil {
		t.Fatalf("WriteFile(agentBinary) error = %v", err)
	}

	if _, err := app.BootstrapServer("edge-02", BootstrapOptions{
		LoginUser:       "ubuntu",
		AgentBinaryPath: agentBinary,
		UseSudo:         true,
	}); err == nil {
		t.Fatalf("expected reverse bootstrap without controller address to fail")
	}
}

type fakeBootstrapExecutor struct {
	requests []BootstrapRequest
}

func (f *fakeBootstrapExecutor) Bootstrap(_ context.Context, req BootstrapRequest) error {
	f.requests = append(f.requests, req)
	return nil
}
