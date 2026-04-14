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

	result, err := app.ApplyFleetUpdate(context.Background(), nil)
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

	if _, err = app.ApplyFleetUpdate(context.Background(), []string{"missing-node"}); err == nil {
		t.Fatalf("expected ApplyFleetUpdate() to fail for a missing targeted server")
	}
}
