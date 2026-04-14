// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package transport_test

import (
	"context"
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
