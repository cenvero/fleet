// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestRotateKeysUpdatesDirectServerAuthorizedKeys(t *testing.T) {
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

	oldKey, err := readPublicKeyLine(filepath.Join(configDir, "keys", "id_ed25519.pub"))
	if err != nil {
		t.Fatalf("readPublicKeyLine(old) error = %v", err)
	}
	authorizedKeysPath := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(authorizedKeysPath, []byte(oldKey+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(authorized_keys) error = %v", err)
	}

	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer app.Close()

	server := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: authorizedKeysPath,
	}
	var calls atomic.Int32
	errCh := make(chan error, 8)
	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40100", "127.0.0.1:2222")
		calls.Add(1)
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

	oldFingerprint, err := fleetFingerprint(configDir)
	if err != nil {
		t.Fatalf("fleetFingerprint(old) error = %v", err)
	}

	result, err := app.RotateKeys()
	if err != nil {
		t.Fatalf("RotateKeys() error = %v", err)
	}
	if len(result.RotatedServers) != 1 || result.RotatedServers[0] != "loopback" {
		t.Fatalf("unexpected rotated servers %#v", result.RotatedServers)
	}
	if len(result.ArchivedFiles) == 0 {
		t.Fatalf("expected archived key files in result")
	}

	newFingerprint, err := fleetFingerprint(configDir)
	if err != nil {
		t.Fatalf("fleetFingerprint(new) error = %v", err)
	}
	if newFingerprint == oldFingerprint {
		t.Fatalf("expected active controller key fingerprint to change")
	}

	newKey, err := readPublicKeyLine(filepath.Join(configDir, "keys", "id_ed25519.pub"))
	if err != nil {
		t.Fatalf("readPublicKeyLine(new) error = %v", err)
	}
	data, err := os.ReadFile(authorizedKeysPath)
	if err != nil {
		t.Fatalf("ReadFile(authorized_keys) error = %v", err)
	}
	content := string(data)
	if strings.Contains(content, oldKey) {
		t.Fatalf("expected old key to be removed from authorized_keys")
	}
	if !strings.Contains(content, newKey) {
		t.Fatalf("expected new key to be present in authorized_keys")
	}
	if calls.Load() < 3 {
		t.Fatalf("expected at least 3 transport calls, got %d", calls.Load())
	}

	for range int(calls.Load()) {
		if err := <-errCh; err != nil {
			t.Fatalf("agent server exited with error: %v", err)
		}
	}
}

func TestRotateKeysRollsBackOnRemovalFailure(t *testing.T) {
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

	oldKey, err := readPublicKeyLine(filepath.Join(configDir, "keys", "id_ed25519.pub"))
	if err != nil {
		t.Fatalf("readPublicKeyLine(old) error = %v", err)
	}
	authorizedKeysPath := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(authorizedKeysPath, []byte(oldKey+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(authorized_keys) error = %v", err)
	}

	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer app.Close()

	manager := &failingAuthorizedKeysManager{
		path: authorizedKeysPath,
		failOnRemove: func(payload proto.AuthorizedKeysPayload) bool {
			for _, key := range payload.RemoveKeys {
				if strings.TrimSpace(key) == strings.TrimSpace(oldKey) {
					return true
				}
			}
			return false
		},
	}
	server := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: authorizedKeysPath,
		AuthorizedKeysMgr:  manager,
	}
	var calls atomic.Int32
	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40101", "127.0.0.1:2222")
		calls.Add(1)
		go func() {
			_ = server.ServeConn(serverConn)
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

	oldFingerprint, err := fleetFingerprint(configDir)
	if err != nil {
		t.Fatalf("fleetFingerprint(old) error = %v", err)
	}
	if _, err := app.RotateKeys(); err == nil {
		t.Fatalf("expected RotateKeys() to fail on removal error")
	}

	currentFingerprint, err := fleetFingerprint(configDir)
	if err != nil {
		t.Fatalf("fleetFingerprint(current) error = %v", err)
	}
	if currentFingerprint != oldFingerprint {
		t.Fatalf("expected local controller key to roll back after failure")
	}

	data, err := os.ReadFile(authorizedKeysPath)
	if err != nil {
		t.Fatalf("ReadFile(authorized_keys) error = %v", err)
	}
	content := string(data)
	if !strings.Contains(content, oldKey) {
		t.Fatalf("expected old key to remain after rollback")
	}
	newKey, err := readPublicKeyLine(filepath.Join(configDir, "keys", "id_ed25519.pub"))
	if err != nil {
		t.Fatalf("readPublicKeyLine(current) error = %v", err)
	}
	if strings.TrimSpace(newKey) != strings.TrimSpace(oldKey) {
		if strings.Contains(content, newKey) {
			t.Fatalf("expected new key to be removed during rollback")
		}
	}
}

func TestRotateKeysUpdatesReverseAgentControllerKnownHosts(t *testing.T) {
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
		Name:    "reverse-node",
		Address: "unknown",
		Mode:    transport.ModeReverse,
		User:    "cenvero-agent",
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	hub := NewReverseHub(app, "test-token")
	defer hub.Close()
	app.ReverseRPC = hub.Call
	app.ReverseStatusLookup = hub.Status
	app.ReverseDisconnect = hub.Disconnect

	oldKey, err := readPublicKeyLine(filepath.Join(configDir, "keys", "id_ed25519.pub"))
	if err != nil {
		t.Fatalf("readPublicKeyLine(old) error = %v", err)
	}

	knownHostsPath := filepath.Join(t.TempDir(), "controller_known_hosts")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dials atomic.Int32
	agentErrCh := make(chan error, 1)
	go func() {
		agentErrCh <- agent.RunReverse(ctx, agent.ReverseOptions{
			ControllerAddress: "127.0.0.1:9443",
			ServerName:        "reverse-node",
			KnownHostsPath:    knownHostsPath,
			MinRetryDelay:     10 * time.Millisecond,
			MaxRetryDelay:     20 * time.Millisecond,
			NetworkDialContext: func(context.Context, string, string) (net.Conn, error) {
				clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:41010", "127.0.0.1:9443")
				dials.Add(1)
				go func() {
					_ = hub.ServeConn(serverConn)
				}()
				return clientConn, nil
			},
		}, agent.Server{
			Mode:        transport.ModeReverse,
			HostKeyPath: filepath.Join(t.TempDir(), "agent_reverse_key"),
		})
	}()

	waitForReverseSession(t, hub, "reverse-node")

	result, err := app.RotateKeys()
	if err != nil {
		t.Fatalf("RotateKeys() error = %v", err)
	}
	if len(result.RotatedServers) != 1 || result.RotatedServers[0] != "reverse-node" {
		t.Fatalf("unexpected rotated servers %#v", result.RotatedServers)
	}

	newKey, err := readPublicKeyLine(filepath.Join(configDir, "keys", "id_ed25519.pub"))
	if err != nil {
		t.Fatalf("readPublicKeyLine(new) error = %v", err)
	}
	if strings.TrimSpace(newKey) == strings.TrimSpace(oldKey) {
		t.Fatalf("expected controller public key to change")
	}

	data, err := os.ReadFile(knownHostsPath)
	if err != nil {
		t.Fatalf("ReadFile(known_hosts) error = %v", err)
	}
	content := string(data)
	if strings.Contains(content, oldKey) {
		t.Fatalf("expected old controller host key to be removed from reverse known_hosts")
	}
	if !strings.Contains(content, newKey) {
		t.Fatalf("expected new controller host key to be present in reverse known_hosts")
	}
	if dials.Load() < 2 {
		t.Fatalf("expected reverse reconnect during rotation, got %d dial(s)", dials.Load())
	}
	if _, err := hub.Status("reverse-node"); err != nil {
		t.Fatalf("Status(reverse-node) error = %v", err)
	}

	cancel()
	hub.Close()
	select {
	case err := <-agentErrCh:
		if err != nil {
			t.Fatalf("agent reverse connector exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("agent reverse connector did not exit")
	}
}

func TestRotateKeysRollsBackOnReverseKnownHostsFailure(t *testing.T) {
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
		Name:    "reverse-node",
		Address: "unknown",
		Mode:    transport.ModeReverse,
		User:    "cenvero-agent",
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	hub := NewReverseHub(app, "test-token")
	defer hub.Close()
	app.ReverseRPC = hub.Call
	app.ReverseStatusLookup = hub.Status
	app.ReverseDisconnect = hub.Disconnect

	oldKey, err := readPublicKeyLine(filepath.Join(configDir, "keys", "id_ed25519.pub"))
	if err != nil {
		t.Fatalf("readPublicKeyLine(old) error = %v", err)
	}
	oldFingerprint, err := fleetFingerprint(configDir)
	if err != nil {
		t.Fatalf("fleetFingerprint(old) error = %v", err)
	}

	knownHostsPath := filepath.Join(t.TempDir(), "controller_known_hosts")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agentErrCh := make(chan error, 1)
	go func() {
		agentErrCh <- agent.RunReverse(ctx, agent.ReverseOptions{
			ControllerAddress: "127.0.0.1:9443",
			ServerName:        "reverse-node",
			KnownHostsPath:    knownHostsPath,
			MinRetryDelay:     10 * time.Millisecond,
			MaxRetryDelay:     20 * time.Millisecond,
			NetworkDialContext: func(context.Context, string, string) (net.Conn, error) {
				clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:41011", "127.0.0.1:9443")
				go func() {
					_ = hub.ServeConn(serverConn)
				}()
				return clientConn, nil
			},
		}, agent.Server{
			Mode:        transport.ModeReverse,
			HostKeyPath: filepath.Join(t.TempDir(), "agent_reverse_key"),
			ControllerKnownHostsMgr: &failingControllerKnownHostsManager{
				path:    knownHostsPath,
				address: "127.0.0.1:9443",
				failOnRemove: func(payload proto.ControllerKnownHostsPayload) bool {
					for _, key := range payload.RemoveKeys {
						if strings.TrimSpace(key) == strings.TrimSpace(oldKey) {
							return true
						}
					}
					return false
				},
			},
		})
	}()

	waitForReverseSession(t, hub, "reverse-node")

	if _, err := app.RotateKeys(); err == nil {
		t.Fatalf("expected RotateKeys() to fail when reverse known_hosts cleanup fails")
	}

	currentFingerprint, err := fleetFingerprint(configDir)
	if err != nil {
		t.Fatalf("fleetFingerprint(current) error = %v", err)
	}
	if currentFingerprint != oldFingerprint {
		t.Fatalf("expected controller key to roll back after reverse failure")
	}

	data, err := os.ReadFile(knownHostsPath)
	if err != nil {
		t.Fatalf("ReadFile(known_hosts) error = %v", err)
	}
	if !strings.Contains(string(data), oldKey) {
		t.Fatalf("expected old controller host key to remain after rollback")
	}

	cancel()
	hub.Close()
	select {
	case err := <-agentErrCh:
		if err != nil {
			t.Fatalf("agent reverse connector exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("agent reverse connector did not exit")
	}
}

func fleetFingerprint(configDir string) (string, error) {
	keys, err := os.ReadFile(filepath.Join(configDir, "keys", "id_ed25519.pub"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(keys)), nil
}

type failingAuthorizedKeysManager struct {
	path         string
	failOnRemove func(proto.AuthorizedKeysPayload) bool
}

func (m *failingAuthorizedKeysManager) Update(ctx context.Context, payload proto.AuthorizedKeysPayload) (proto.AuthorizedKeysResult, error) {
	_ = ctx
	if m.failOnRemove != nil && m.failOnRemove(payload) {
		return proto.AuthorizedKeysResult{}, &agent.RPCError{
			Code:    "forced_failure",
			Message: "forced authorized_keys removal failure",
		}
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		return proto.AuthorizedKeysResult{}, &agent.RPCError{
			Code:    "read_failed",
			Message: err.Error(),
		}
	}
	lines := make([]string, 0, 4)
	seen := map[string]struct{}{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, ok := seen[line]; ok {
			continue
		}
		seen[line] = struct{}{}
		lines = append(lines, line)
	}
	remove := map[string]struct{}{}
	for _, line := range payload.RemoveKeys {
		remove[strings.TrimSpace(line)] = struct{}{}
	}
	filtered := lines[:0]
	for _, line := range lines {
		if _, drop := remove[line]; drop {
			continue
		}
		filtered = append(filtered, line)
	}
	for _, line := range payload.AddKeys {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, ok := seen[line]; ok {
			continue
		}
		seen[line] = struct{}{}
		filtered = append(filtered, line)
	}
	if err := os.WriteFile(m.path, []byte(strings.Join(filtered, "\n")+"\n"), 0o600); err != nil {
		return proto.AuthorizedKeysResult{}, &agent.RPCError{
			Code:    "write_failed",
			Message: err.Error(),
		}
	}
	return proto.AuthorizedKeysResult{Keys: filtered}, nil
}

type failingControllerKnownHostsManager struct {
	path         string
	address      string
	failOnRemove func(proto.ControllerKnownHostsPayload) bool
}

func (m *failingControllerKnownHostsManager) Update(ctx context.Context, payload proto.ControllerKnownHostsPayload) (proto.ControllerKnownHostsResult, error) {
	_ = ctx
	if m.failOnRemove != nil && m.failOnRemove(payload) {
		return proto.ControllerKnownHostsResult{}, &agent.RPCError{
			Code:    "forced_failure",
			Message: "forced controller known_hosts removal failure",
		}
	}

	address := strings.TrimSpace(payload.Address)
	if address == "" {
		address = m.address
	}
	normalizedAddress := knownhosts.Normalize(address)

	data, err := os.ReadFile(m.path)
	if err != nil && !os.IsNotExist(err) {
		return proto.ControllerKnownHostsResult{}, &agent.RPCError{
			Code:    "read_failed",
			Message: err.Error(),
		}
	}

	remove := map[string]struct{}{}
	for _, line := range payload.RemoveKeys {
		remove[strings.TrimSpace(line)] = struct{}{}
	}
	keys := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.Join(fields[1:], " ")))
		if err != nil {
			continue
		}
		canonical := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
		if normalizedAddress != knownhosts.Normalize(fields[0]) {
			keys["other:"+line] = line
			continue
		}
		if _, drop := remove[canonical]; drop {
			continue
		}
		keys[canonical] = strings.TrimSpace(knownhosts.Line([]string{normalizedAddress}, pub))
	}
	for _, line := range payload.AddKeys {
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(line)))
		if err != nil {
			return proto.ControllerKnownHostsResult{}, &agent.RPCError{
				Code:    "invalid_authorized_key",
				Message: err.Error(),
			}
		}
		canonical := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
		keys[canonical] = strings.TrimSpace(knownhosts.Line([]string{normalizedAddress}, pub))
	}

	lines := make([]string, 0, len(keys))
	for _, line := range keys {
		lines = append(lines, line)
	}
	if err := os.WriteFile(m.path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return proto.ControllerKnownHostsResult{}, &agent.RPCError{
			Code:    "write_failed",
			Message: err.Error(),
		}
	}
	return proto.ControllerKnownHostsResult{
		Address:    normalizedAddress,
		EntryCount: len(lines),
	}, nil
}
