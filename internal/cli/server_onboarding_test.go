// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/spf13/cobra"
)

func newServerCommandTestConfig(t *testing.T) string {
	t.Helper()
	configDir := t.TempDir()
	if _, err := core.Initialize(core.InitOptions{
		ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeDirect,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	return configDir
}

func TestServerAddRejectsDuplicateBeforeAddressPrompt(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	app, err := core.Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.AddServer(core.ServerRecord{Name: "herm", Address: "91.99.197.39", Port: 5911, Mode: transport.ModeDirect}); err != nil {
		t.Fatal(err)
	}
	_ = app.Close()

	cmd := newServerCommand(&configDir)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"add", "herm"})
	err = cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "already exists at 91.99.197.39:5911") {
		t.Fatalf("error=%v, want actionable duplicate rejection", err)
	}
	if strings.Contains(out.String(), "IP address or hostname") {
		t.Fatalf("duplicate add prompted for address before rejection: %q", out.String())
	}
	if !strings.Contains(err.Error(), "server bootstrap herm") || !strings.Contains(err.Error(), "server remove herm --force") {
		t.Fatalf("duplicate error lacks retry/remove guidance: %v", err)
	}
}

func TestServerAddConfirmationEOFDoesNotPersist(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	cmd := newServerCommand(&configDir)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"add", "new-node", "192.0.2.20", "--login-user", "root", "--login-key", "/tmp/test-key"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "server was not added") {
		t.Fatalf("error=%v, want fail-closed confirmation", err)
	}

	app, openErr := core.Open(configDir)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer app.Close()
	servers, listErr := app.ListServers()
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(servers) != 0 {
		t.Fatalf("confirmation failure persisted servers: %+v", servers)
	}
}

func TestServerAddPreservesConfiguredAgentPort(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	cmd := newServerCommand(&configDir)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"add", "custom-port", "192.0.2.30", "--no-agent", "--port", "5911"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("server add error = %v", err)
	}
	if !strings.Contains(out.String(), "fleet-agent serve --listen 0.0.0.0:5911") || !strings.Contains(out.String(), "ensure port 5911 is reachable") {
		t.Fatalf("manual instructions did not use configured port:\n%s", out.String())
	}
	app, err := core.Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	record, err := app.GetServer("custom-port")
	if err != nil {
		t.Fatal(err)
	}
	if record.Port != 5911 {
		t.Fatalf("stored port=%d, want 5911", record.Port)
	}
}

func TestWriteServerTableNormalizesVersionAndInstallState(t *testing.T) {
	servers := []core.ServerRecord{
		{Name: "online", Address: "192.0.2.1", Port: 2222, Mode: transport.ModeDirect, Observed: core.ServerObservation{Reachable: true, AgentVersion: "2.4.0"}},
		{Name: "failed", Address: "192.0.2.2", Port: 5911, Mode: transport.ModeDirect, Agent: core.AgentInstall{Status: "failed"}, Observed: core.ServerObservation{AgentVersion: "version unavailable"}},
		{Name: "new", Address: "192.0.2.3", Port: 3022, Mode: transport.ModeDirect},
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := writeServerTable(cmd, servers); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"online", "v2.4.0", "install-failed", "192.0.2.2:5911", "pending"} {
		if !strings.Contains(text, want) {
			t.Fatalf("table missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "version unavailable") {
		t.Fatalf("table rendered invalid sentinel as a version:\n%s", text)
	}
}

func TestNormalizeAgentVersionRejectsSentinels(t *testing.T) {
	if got := normalizeAgentVersion("2.4.2"); got != "v2.4.2" {
		t.Fatalf("bare version normalized to %q", got)
	}
	if got := normalizeAgentVersion("v2.4.2"); got != "v2.4.2" {
		t.Fatalf("prefixed version normalized to %q", got)
	}
	if got := normalizeAgentVersion("version unavailable"); got != "" {
		t.Fatalf("sentinel normalized to %q", got)
	}
}

func TestOtherVersionSurfacesUseCanonicalDisplay(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := writeInventoryTable(cmd, core.Inventory{Items: []core.InventoryItem{{
		Server: "node", Reachable: true, AgentVersion: "2.4.0",
	}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "v2.4.0") {
		t.Fatalf("inventory did not canonicalize agent version:\n%s", out.String())
	}
	if got := describeStaleAgents([]core.AgentVersionMismatch{{Server: "node", AgentVersion: "2.4.0"}}); !strings.Contains(got, "v2.4.0") {
		t.Fatalf("stale-agent notice did not canonicalize version: %s", got)
	}
}
