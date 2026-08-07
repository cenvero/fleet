// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package transport_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	fleetcrypto "github.com/cenvero/fleet/internal/crypto"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
)

func TestDirectHelloRoundTrip(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	keysDir := filepath.Join(tempDir, "controller-keys")
	if err := fleetcrypto.GenerateKeySet(keysDir, fleetcrypto.AlgorithmEd25519, nil); err != nil {
		t.Fatalf("GenerateKeySet() error = %v", err)
	}

	server := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(tempDir, "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(keysDir, "id_ed25519.pub"),
	}
	clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40000", "127.0.0.1:2222")
	defer clientConn.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ServeConn(serverConn)
	}()

	connector := transport.Connector{
		Mode:             transport.ModeDirect,
		Username:         "cenvero-agent",
		PrivateKeyPath:   filepath.Join(keysDir, "id_ed25519"),
		KnownHostsPath:   filepath.Join(tempDir, "known_hosts"),
		AcceptNewHostKey: false,
		NetworkDialContext: func(context.Context, string, string) (net.Conn, error) {
			return clientConn, nil
		},
	}

	session, err := connector.DialContext(context.Background(), transport.ServerTarget{
		Name:    "test-agent",
		Address: "127.0.0.1",
		Port:    2222,
		Mode:    transport.ModeDirect,
		User:    "cenvero-agent",
	})
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	defer session.Close()

	hello, err := session.Hello(context.Background(), "controller-1")
	if err != nil {
		t.Fatalf("Hello() error = %v", err)
	}
	if hello.NodeName == "" {
		t.Fatalf("hello payload node name should not be empty")
	}
	if hello.Transport != transport.ModeDirect.String() {
		t.Fatalf("hello transport = %s, want %s", hello.Transport, transport.ModeDirect)
	}
	if session.HostKeyFingerprint == "" {
		t.Fatalf("session host key fingerprint should not be empty")
	}

	oldAgentMarker := filepath.Join(tempDir, "old-agent-must-not-run")
	session.SetCapabilities(nil)
	oldCtx, stopOld := context.WithCancel(context.Background())
	_, oldErr := session.Call(oldCtx, proto.Envelope{
		Action:  "shell.exec",
		Payload: proto.ExecPayload{Command: fmt.Sprintf("touch %q", oldAgentMarker)},
	})
	stopOld()
	if oldErr == nil || !strings.Contains(oldErr.Error(), proto.CapabilityRequestCancel) {
		t.Fatalf("old-agent deadline-free exec error = %v, want capability refusal", oldErr)
	}
	if _, err := os.Stat(oldAgentMarker); !os.IsNotExist(err) {
		t.Fatalf("fail-closed old-agent execution reached remote process: %v", err)
	}
	session.SetCapabilities(hello.Capabilities)

	marker := filepath.Join(tempDir, "must-not-be-created")
	execCtx, cancelExec := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, execErr := session.Call(execCtx, proto.Envelope{
		Action:  "shell.exec",
		Payload: proto.ExecPayload{Command: fmt.Sprintf("sleep 0.5; touch %q", marker)},
	})
	cancelExec()
	if !errors.Is(execErr, context.DeadlineExceeded) {
		t.Fatalf("timed exec error = %v, want context deadline exceeded", execErr)
	}
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("timed-out remote process group survived and created marker: %v", err)
	}

	cancelMarker := filepath.Join(tempDir, "cancel-must-not-be-created")
	started := filepath.Join(tempDir, "cancel-started")
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancelResult := make(chan error, 1)
	go func() {
		_, err := session.Call(cancelCtx, proto.Envelope{
			Action:  "shell.exec",
			Payload: proto.ExecPayload{Command: fmt.Sprintf("touch %q; sleep 0.5; touch %q", started, cancelMarker)},
		})
		cancelResult <- err
	}()
	waitForFile(t, started)
	cancel()
	select {
	case err := <-cancelResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("deadline-free cancelled exec error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deadline-free cancelled exec did not return")
	}
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(cancelMarker); !os.IsNotExist(err) {
		t.Fatalf("cancelled remote process group survived and created marker: %v", err)
	}

	response, err := session.Call(context.Background(), proto.Envelope{
		Action:  "shell.exec",
		Payload: proto.ExecPayload{Command: "printf reusable"},
	})
	if err != nil {
		t.Fatalf("post-cancellation call failed: %v", err)
	}
	result, err := proto.DecodePayload[proto.ExecResult](response.Payload)
	if err != nil || result.Stdout != "reusable" {
		t.Fatalf("post-cancellation result = %#v, err=%v", result, err)
	}

	data, err := os.ReadFile(filepath.Join(tempDir, "known_hosts"))
	if err != nil {
		t.Fatalf("read known_hosts: %v", err)
	}
	if !strings.Contains(string(data), "127.0.0.1") {
		t.Fatalf("known_hosts should contain localhost entry, got %q", string(data))
	}
	if err := session.Close(); err != nil {
		t.Fatalf("session.Close() error = %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("server exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("server did not exit")
	}
}

func TestDialContextBoundsStalledSSHHandshake(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	keysDir := filepath.Join(tempDir, "keys")
	if err := fleetcrypto.GenerateKeySet(keysDir, fleetcrypto.AlgorithmEd25519, nil); err != nil {
		t.Fatal(err)
	}
	clientConn, peerConn := net.Pipe()
	defer peerConn.Close()
	connector := transport.Connector{
		PrivateKeyPath: filepath.Join(keysDir, "id_ed25519"),
		KnownHostsPath: filepath.Join(tempDir, "known_hosts"),
		NetworkDialContext: func(context.Context, string, string) (net.Conn, error) {
			return clientConn, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := connector.DialContext(ctx, transport.ServerTarget{Address: "127.0.0.1", Port: 2222})
	if err == nil {
		t.Fatal("stalled SSH handshake unexpectedly succeeded")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("stalled handshake ignored deadline and took %s", elapsed)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %s was not created before timeout", path)
}
