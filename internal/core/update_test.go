// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/internal/version"
	"github.com/cenvero/fleet/pkg/proto"
)

func TestApplyFleetUpdateRollsAcrossAgentsAndKeepsFailuresIsolated(t *testing.T) {
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

	app.ControllerUpdater = func(context.Context, update.ApplyOptions) (update.ApplyResult, error) {
		return update.ApplyResult{
			Channel:        "stable",
			CurrentVersion: "dev",
			Version:        "v1.2.3",
		}, nil
	}

	servers := []ServerRecord{
		{
			Name:    "ok-node",
			Address: "127.0.0.1",
			Port:    2301,
			Mode:    transport.ModeDirect,
			User:    "cenvero-agent",
			Agent: AgentInstall{
				Managed:     true,
				ServiceName: "cenvero-fleet-agent",
			},
			Observed: ServerObservation{AgentVersion: "v1.2.2"},
		},
		{
			Name:    "bad-node",
			Address: "127.0.0.1",
			Port:    2302,
			Mode:    transport.ModeDirect,
			User:    "cenvero-agent",
			Agent: AgentInstall{
				Managed:     true,
				ServiceName: "cenvero-fleet-agent",
			},
			Observed: ServerObservation{AgentVersion: "v1.2.2"},
		},
	}
	for _, server := range servers {
		if err := app.AddServer(server); err != nil {
			t.Fatalf("AddServer(%s) error = %v", server.Name, err)
		}
	}

	goodAgent := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "good_agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
		Updater: fakeAgentUpdater{result: proto.UpdateApplyResult{
			Channel:          "stable",
			CurrentVersion:   "v1.2.2",
			Version:          "v1.2.3",
			Applied:          true,
			RestartScheduled: true,
			ServiceName:      "cenvero-fleet-agent",
		}},
	}
	badAgent := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "bad_agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
		Updater:            fakeAgentUpdater{err: &agent.RPCError{Code: "download_failed", Message: "network timeout"}},
	}

	app.NetworkDialContext = func(_ context.Context, _, address string) (net.Conn, error) {
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:50000", address)
		switch address {
		case "127.0.0.1:2301":
			go func() { _ = goodAgent.ServeConn(serverConn) }()
		case "127.0.0.1:2302":
			go func() { _ = badAgent.ServeConn(serverConn) }()
		default:
			t.Fatalf("unexpected dial target %s", address)
		}
		return clientConn, nil
	}

	result, err := app.ApplyFleetUpdate(context.Background(), nil, false, false)
	if err != nil {
		t.Fatalf("ApplyFleetUpdate() error = %v", err)
	}
	if result.Attempted != 2 || result.Succeeded != 1 || result.Failed != 1 {
		t.Fatalf("unexpected rollout counts: %#v", result)
	}
	if len(result.Agents) != 2 {
		t.Fatalf("expected two agent results, got %d", len(result.Agents))
	}
	if result.Agents[0].Server != "bad-node" && result.Agents[1].Server != "bad-node" {
		t.Fatalf("expected bad-node result to be present: %#v", result.Agents)
	}

	okNode, err := app.GetServer("ok-node")
	if err != nil {
		t.Fatalf("GetServer(ok-node) error = %v", err)
	}
	if okNode.Observed.AgentVersion != "v1.2.3" {
		t.Fatalf("expected ok-node version to update, got %q", okNode.Observed.AgentVersion)
	}
	badNode, err := app.GetServer("bad-node")
	if err != nil {
		t.Fatalf("GetServer(bad-node) error = %v", err)
	}
	if badNode.Observed.AgentVersion == "v1.2.3" {
		t.Fatalf("expected bad-node to avoid the new target version after a failed update, got %q", badNode.Observed.AgentVersion)
	}
}

func TestApplyFleetUpdateSkipsControllerForHomebrewInstall(t *testing.T) {
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

	app.ExecutablePath = "/opt/homebrew/bin/fleet"
	app.ControllerUpdater = func(context.Context, update.ApplyOptions) (update.ApplyResult, error) {
		t.Fatalf("ControllerUpdater should not run for Homebrew-managed installs")
		return update.ApplyResult{}, nil
	}

	result, err := app.ApplyFleetUpdate(context.Background(), nil, false, false)
	if err != nil {
		t.Fatalf("ApplyFleetUpdate() error = %v", err)
	}
	if result.Controller.Applied {
		t.Fatalf("expected controller update to be skipped for Homebrew install")
	}
	if result.Controller.Note == "" {
		t.Fatalf("expected Homebrew guidance note in controller result")
	}
	if result.Attempted != 0 {
		t.Fatalf("expected no agent updates without servers, got %d", result.Attempted)
	}
}

type fakeAgentUpdater struct {
	result proto.UpdateApplyResult
	err    error
}

func (f fakeAgentUpdater) Apply(context.Context, proto.UpdateApplyPayload) (agent.UpdateOperation, error) {
	if f.err != nil {
		return agent.UpdateOperation{}, f.err
	}
	return agent.UpdateOperation{Result: f.result}, nil
}

func TestApplyFleetUpdateCanTargetSubset(t *testing.T) {
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

	app.ControllerUpdater = func(context.Context, update.ApplyOptions) (update.ApplyResult, error) {
		return update.ApplyResult{Channel: "stable", CurrentVersion: "dev", Version: "v1.2.3"}, nil
	}
	if err := app.AddServer(ServerRecord{Name: "only-node", Address: "127.0.0.1", Port: 2303, Mode: transport.ModeDirect, User: "cenvero-agent"}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	if _, err = app.ApplyFleetUpdate(context.Background(), []string{"missing-node"}, false, false); err == nil {
		t.Fatalf("expected ApplyFleetUpdate() to fail for a missing targeted server")
	}
}

func TestApplyFleetUpdateSkipsControllerForWinGetInstall(t *testing.T) {
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

	app.ExecutablePath = `C:\Users\operator\AppData\Local\Microsoft\WinGet\Packages\Cenvero.Fleet_Microsoft.Winget.Source_8wekyb3d8bbwe\fleet.exe`
	app.ControllerUpdater = func(context.Context, update.ApplyOptions) (update.ApplyResult, error) {
		t.Fatalf("ControllerUpdater should not run for WinGet-managed installs")
		return update.ApplyResult{}, nil
	}

	result, err := app.ApplyFleetUpdate(context.Background(), nil, false, false)
	if err != nil {
		t.Fatalf("ApplyFleetUpdate() error = %v", err)
	}
	if result.Controller.Applied {
		t.Fatal("expected controller update to be skipped for WinGet install")
	}
	if !strings.Contains(result.Controller.Note, "WinGet") || !strings.Contains(result.Controller.Note, "winget upgrade") {
		t.Fatalf("expected WinGet guidance note, got %q", result.Controller.Note)
	}
	if result.Attempted != 0 {
		t.Fatalf("expected no agent updates without servers, got %d", result.Attempted)
	}
}

func TestAgentSupportsUnattendedUpdateActivation(t *testing.T) {
	t.Parallel()

	if !agentSupportsUnattendedUpdateActivation("linux") || !agentSupportsUnattendedUpdateActivation(" Linux ") {
		t.Fatal("Linux managed agents should support unattended update activation")
	}
	for _, goos := range []string{"windows", "darwin", "", "freebsd"} {
		if agentSupportsUnattendedUpdateActivation(goos) {
			t.Fatalf("%q unexpectedly supports unattended update activation", goos)
		}
	}
}

func TestWindowsAgentDeliveryPreservesObservedVersionUntilReconnect(t *testing.T) {
	originalVersion := version.Version
	version.Version = "v1.2.3"
	defer func() { version.Version = originalVersion }()

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

	server := ServerRecord{
		Name:    "windows-node",
		Address: "127.0.0.1",
		Port:    2310,
		Mode:    transport.ModeDirect,
		User:    "operator",
		Agent: AgentInstall{
			Managed:     true,
			ServiceName: "cenvero-fleet-agent",
		},
		Observed: ServerObservation{AgentVersion: "v1.2.2", OS: "windows"},
	}
	if err := app.AddServer(server); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	windowsAgent := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "windows_agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
		Updater: fakeAgentUpdater{result: proto.UpdateApplyResult{
			Channel:           "stable",
			CurrentVersion:    "v1.2.2",
			Version:           "v1.2.3",
			Applied:           true,
			SHA256Verified:    true,
			SignatureVerified: true,
			RestartScheduled:  false,
			ServiceName:       "cenvero-fleet-agent",
		}},
	}
	app.NetworkDialContext = func(_ context.Context, _, address string) (net.Conn, error) {
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:50000", address)
		go func() { _ = windowsAgent.ServeConn(serverConn) }()
		return clientConn, nil
	}

	var progress []SyncAgentProgress
	result, err := app.SyncAgent(context.Background(), []string{server.Name}, func(update SyncAgentProgress) {
		progress = append(progress, update)
	})
	if err != nil {
		t.Fatalf("SyncAgent() error = %v", err)
	}
	if result.Synced != 1 || result.Failed != 0 || len(result.Agents) != 1 {
		t.Fatalf("unexpected sync result: %#v", result)
	}
	agentResult := result.Agents[0]
	if !agentResult.Updated || !agentResult.ActivationPending || agentResult.RestartHandled {
		t.Fatalf("expected delivered update pending activation: %#v", agentResult)
	}
	if agentResult.AgentVersion != "v1.2.2" || agentResult.WantedVersion != "v1.2.3" {
		t.Fatalf("pending result must preserve live old→target versions: %#v", agentResult)
	}
	if len(progress) != 2 || progress[1].State != "pending-activation" || progress[1].From != "v1.2.2" || progress[1].To != "v1.2.3" {
		t.Fatalf("unexpected pending-activation progress: %#v", progress)
	}
	encoded, err := json.Marshal(agentResult)
	if err != nil {
		t.Fatal(err)
	}
	jsonResult := string(encoded)
	for _, want := range []string{`"agent_version":"v1.2.2"`, `"wanted_version":"v1.2.3"`, `"activation_pending":true`} {
		if !strings.Contains(jsonResult, want) {
			t.Fatalf("pending result JSON missing %s: %s", want, jsonResult)
		}
	}

	stored, err := app.GetServer(server.Name)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Observed.AgentVersion != "v1.2.2" {
		t.Fatalf("observed live version advanced before restart/reconnect: %q", stored.Observed.AgentVersion)
	}
	if stored.Agent.UpdatedAt.IsZero() {
		t.Fatal("successful binary delivery did not record agent update time")
	}

	session, hello, err := app.openDirectSession(server, false)
	if err != nil {
		t.Fatalf("reconnect after activation: %v", err)
	}
	_ = session.Close()
	if hello.AgentVersion != "v1.2.3" {
		t.Fatalf("reconnected agent reported %q, want v1.2.3", hello.AgentVersion)
	}
	stored, err = app.GetServer(server.Name)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Observed.AgentVersion != "v1.2.3" {
		t.Fatalf("reconnect did not activate observed target version: %q", stored.Observed.AgentVersion)
	}
}
