// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
)

func TestInitializeCreatesLayoutAndConfig(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	result, err := Initialize(InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeDirect,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	})
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	if result.Config.ConfigDir != configDir {
		t.Fatalf("unexpected config dir: %s", result.Config.ConfigDir)
	}

	for _, path := range []string{
		filepath.Join(configDir, "config.toml"),
		filepath.Join(configDir, "instance.id"),
		filepath.Join(configDir, "keys", "id_ed25519"),
		filepath.Join(configDir, "keys", "id_ed25519.pub"),
		filepath.Join(configDir, "data", "state.db"),
		filepath.Join(configDir, "data", "metrics.db"),
		filepath.Join(configDir, "data", "events.db"),
		filepath.Join(configDir, "logs", "_audit.log"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s to exist: %v", path, err)
		}
	}

	if result.Config.Database.Backend != "sqlite" {
		t.Fatalf("expected sqlite backend, got %q", result.Config.Database.Backend)
	}
}

func TestValidateAlias(t *testing.T) {
	t.Parallel()

	if err := ValidateAlias("fleet01"); err != nil {
		t.Fatalf("ValidateAlias(valid) error = %v", err)
	}
	if err := ValidateAlias("!"); err == nil {
		t.Fatalf("ValidateAlias(invalid) expected error")
	}
}

func TestDefaultConfigSetsRuntimePollingAndNotifications(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig("/tmp/fleet")
	if cfg.Runtime.MetricsPollInterval == "" {
		t.Fatalf("expected metrics poll interval to be set by default")
	}
	if !cfg.Runtime.DesktopNotifications {
		t.Fatalf("expected desktop notifications to be enabled by default")
	}
	if cfg.Runtime.AggregatedLogDir == "" {
		t.Fatalf("expected aggregated log dir to be set by default")
	}
	if cfg.Runtime.AggregatedLogMaxSize <= 0 {
		t.Fatalf("expected aggregated log max size to be positive by default")
	}
	if cfg.Runtime.AggregatedLogMaxFiles <= 0 {
		t.Fatalf("expected aggregated log max files to be positive by default")
	}
	if cfg.Runtime.AggregatedLogMaxAge == "" {
		t.Fatalf("expected aggregated log max age to be set by default")
	}
	if cfg.Runtime.AlertNotifyCooldown == "" {
		t.Fatalf("expected alert notify cooldown to be set by default")
	}
}

func TestLoadConfigBackfillsAggregatedLogDefaults(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	cfg := DefaultConfig(configDir)
	cfg.Runtime.AggregatedLogDir = ""
	cfg.Runtime.AggregatedLogMaxSize = 0
	cfg.Runtime.AggregatedLogMaxFiles = 0
	cfg.Runtime.AggregatedLogMaxAge = ""
	cfg.Runtime.AlertNotifyCooldown = ""

	path := ConfigPath(configDir)
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := toml.NewEncoder(file).Encode(cfg); err != nil {
		file.Close()
		t.Fatalf("Encode() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if loaded.Runtime.AggregatedLogDir == "" {
		t.Fatalf("expected aggregated log dir to be backfilled")
	}
	if loaded.Runtime.AggregatedLogMaxSize <= 0 {
		t.Fatalf("expected aggregated log max size to be backfilled")
	}
	if loaded.Runtime.AggregatedLogMaxFiles <= 0 {
		t.Fatalf("expected aggregated log max files to be backfilled")
	}
	if loaded.Runtime.AggregatedLogMaxAge == "" {
		t.Fatalf("expected aggregated log max age to be backfilled")
	}
	if loaded.Runtime.AlertNotifyCooldown == "" {
		t.Fatalf("expected alert notify cooldown to be backfilled")
	}
}

func TestConfigValidateRejectsInvalidAggregatedLogMaxAge(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig("/tmp/fleet")
	cfg.Runtime.AggregatedLogMaxAge = "not-a-duration"
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected invalid aggregated log max age to fail validation")
	}

	cfg = DefaultConfig("/tmp/fleet")
	cfg.Runtime.AggregatedLogMaxAge = "0s"
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected non-positive aggregated log max age to fail validation")
	}
}

func TestReconnectServerUpdatesObservedState(t *testing.T) {
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

	server := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
	}
	clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40000", "127.0.0.1:2222")
	defer clientConn.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ServeConn(serverConn)
	}()
	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
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

	if err := app.ReconnectServer("loopback", false); err != nil {
		t.Fatalf("ReconnectServer() error = %v", err)
	}

	record, err := app.GetServer("loopback")
	if err != nil {
		t.Fatalf("GetServer() error = %v", err)
	}
	if !record.Observed.Reachable {
		t.Fatalf("expected server to be reachable after reconnect")
	}
	if record.Observed.AgentVersion == "" {
		t.Fatalf("expected observed agent version to be populated")
	}
	if len(record.Capabilities) == 0 {
		t.Fatalf("expected capabilities to be populated")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("agent server exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("agent server did not exit")
	}
}
