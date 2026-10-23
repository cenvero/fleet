// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
)

// teardownRig runs a managed in-process agent whose shell.exec finds a fake
// systemd-run on PATH. The fake records its arguments and, when stopAgent is
// set, marks the agent gone so later dials fail as they would once the real
// teardown stopped the service.
type teardownRig struct {
	app      *App
	argsPath string
	gonePath string
}

func newTeardownRig(t *testing.T, systemdRunExit int, stopAgent bool) teardownRig {
	t.Helper()
	oldSettle, oldGone, oldPoll := agentTeardownSettle, agentTeardownGoneTimeout, agentTeardownPoll
	agentTeardownSettle, agentTeardownGoneTimeout, agentTeardownPoll = 10*time.Millisecond, 500*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() {
		agentTeardownSettle, agentTeardownGoneTimeout, agentTeardownPoll = oldSettle, oldGone, oldPoll
	})

	binDir := t.TempDir()
	state := t.TempDir()
	rig := teardownRig{argsPath: filepath.Join(state, "args"), gonePath: filepath.Join(state, "gone")}
	// NUL-separated: the script argument itself spans lines.
	fake := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done > " + shellQuote(rig.argsPath) + "\n"
	if stopAgent {
		fake += ": > " + shellQuote(rig.gonePath) + "\n"
	}
	if systemdRunExit != 0 {
		fake += "echo 'Failed to start transient service unit' >&2\n"
	}
	fake += "exit " + strconv.Itoa(systemdRunExit) + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "systemd-run"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeDirect,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	rig.app = app

	srv := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
	}
	app.NetworkDialContext = func(_ context.Context, _, addr string) (net.Conn, error) {
		if _, err := os.Stat(rig.gonePath); err == nil {
			return nil, errors.New("connection refused")
		}
		if !strings.HasSuffix(addr, ":2222") {
			// The SSH fallback dials the login port; nothing listens there.
			return nil, errors.New("connection refused")
		}
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40000", "127.0.0.1:2222")
		go func() { _ = srv.ServeConn(serverConn) }()
		return clientConn, nil
	}
	if err := app.AddServer(ServerRecord{
		Name: "box", Address: "127.0.0.1", Port: 2222, Mode: transport.ModeDirect, User: "root",
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}
	record, err := app.GetServer("box")
	if err != nil {
		t.Fatal(err)
	}
	record.Agent = AgentInstall{
		Managed: true, Status: "managed", ServiceName: defaultServiceName,
		LoginUser: "root", LoginPort: 22, LoginKey: filepath.Join(t.TempDir(), "never-authorized"),
	}
	if err := app.SaveServer(record); err != nil {
		t.Fatal(err)
	}
	return rig
}

func (r teardownRig) systemdRunArgs(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(r.argsPath)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
}

func (r teardownRig) serverExists(t *testing.T) bool {
	t.Helper()
	_, err := r.app.GetServer("box")
	return err == nil
}

// TestRemoveServerTearsDownAgentOverItsConnection is the fix: a server whose
// stored login key was never authorized for sshd (a password install) must
// still be removable, through the agent's own authenticated connection.
func TestRemoveServerTearsDownAgentOverItsConnection(t *testing.T) {
	rig := newTeardownRig(t, 0, true)

	if err := rig.app.RemoveServer("box"); err != nil {
		t.Fatalf("RemoveServer() error = %v", err)
	}
	if rig.serverExists(t) {
		t.Fatal("server record survived a successful teardown")
	}
	args := rig.systemdRunArgs(t)
	if len(args) != 7 || !strings.HasPrefix(args[0], "--unit=cenvero-fleet-teardown-") ||
		args[1] != "--collect" || args[2] != "--quiet" || args[3] != "--no-block" ||
		args[4] != "/bin/sh" || args[5] != "-c" {
		t.Fatalf("systemd-run args = %q", args)
	}
	unit := strings.TrimPrefix(args[0], "--unit=cenvero-fleet-teardown-")
	if len(unit) != 16 || strings.Trim(unit, "0123456789abcdef") != "" {
		t.Fatalf("teardown unit suffix %q is not a random hex token", unit)
	}
	script := args[6]
	sleepAt := strings.Index(script, "\nsleep ")
	stopAt := strings.Index(script, "systemctl disable --now")
	if sleepAt < 0 || stopAt < 0 || sleepAt > stopAt {
		t.Fatalf("script must settle before stopping the agent:\n%s", script)
	}
	if !strings.Contains(script, "SERVICE_NAME='cenvero-fleet-agent'") || strings.Contains(script, "sudo ") {
		t.Fatalf("unexpected teardown script:\n%s", script)
	}
}

// TestRemoveServerFallsBackToSSHWhenAgentCannotStartTeardown keeps the old
// route for agents that cannot run the teardown themselves, and reports both
// failures when neither works.
func TestRemoveServerFallsBackToSSHWhenAgentCannotStartTeardown(t *testing.T) {
	rig := newTeardownRig(t, 1, false)

	err := rig.app.RemoveServer("box")
	if err == nil {
		t.Fatal("RemoveServer() succeeded although neither teardown route worked")
	}
	for _, want := range []string{"over the fleet connection", "Failed to start transient service unit", "over SSH", "--via-ssh", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error lacks %q:\n%v", want, err)
		}
	}
	if !rig.serverExists(t) {
		t.Fatal("server record removed although teardown failed")
	}
}

// TestRemoveServerKeepsRecordWhenAgentStillAnswers: once the agent accepted
// the teardown, a lingering agent is reported, not retried over SSH, and the
// record stays so the operator can see what is still running.
func TestRemoveServerKeepsRecordWhenAgentStillAnswers(t *testing.T) {
	rig := newTeardownRig(t, 0, false)

	err := rig.app.RemoveServer("box")
	if err == nil || !strings.Contains(err.Error(), "still answers") {
		t.Fatalf("error = %v, want agent-still-answers", err)
	}
	if strings.Contains(err.Error(), "over SSH") || strings.Contains(err.Error(), "--via-ssh") {
		t.Fatalf("a started teardown must not fall back to SSH: %v", err)
	}
	if !rig.serverExists(t) {
		t.Fatal("server record removed although the agent still answers")
	}
}

// TestRemoveServerViaSSHSkipsAgentConnection: --via-ssh means SSH only.
func TestRemoveServerViaSSHSkipsAgentConnection(t *testing.T) {
	rig := newTeardownRig(t, 0, true)

	err := rig.app.RemoveServerWithOptions("box", RemoveOptions{OverrideSSH: true, LoginUser: "root"})
	if err == nil || strings.Contains(err.Error(), "over the fleet connection") {
		t.Fatalf("error = %v, want an SSH-only failure", err)
	}
	if args := rig.systemdRunArgs(t); args != nil {
		t.Fatalf("--via-ssh still asked the agent to tear down: %q", args)
	}
}

func TestTeardownAgentViaConnectionRefusesNonLinuxAgent(t *testing.T) {
	rig := newTeardownRig(t, 0, true)
	record, err := rig.app.GetServer("box")
	if err != nil {
		t.Fatal(err)
	}
	record.Observed.OS = "windows"
	started, err := rig.app.TeardownAgentViaConnection(context.Background(), record)
	if started || err == nil || !strings.Contains(err.Error(), "needs a Linux agent") {
		t.Fatalf("started=%v err=%v, want refusal before contacting the agent", started, err)
	}
	if args := rig.systemdRunArgs(t); args != nil {
		t.Fatalf("non-Linux agent was asked to run systemd-run: %q", args)
	}
}

func TestBuildAgentTeardownScriptOptionalSteps(t *testing.T) {
	uploaded := buildAgentTeardownScript("svc", "sudo ", "/tmp/cenvero-x/teardown.sh", 0)
	if strings.Contains(uploaded, "\nsleep ") || !strings.Contains(uploaded, "rm -f '/tmp/cenvero-x/teardown.sh'") {
		t.Fatalf("SSH teardown script changed:\n%s", uploaded)
	}
	viaAgent := buildAgentTeardownScript("svc", "", "", 1500*time.Millisecond)
	if !strings.Contains(viaAgent, "\nsleep 2\n") || strings.Contains(viaAgent, "rm -f ''") {
		t.Fatalf("agent teardown script:\n%s", viaAgent)
	}
}
