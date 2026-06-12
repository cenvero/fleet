// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"golang.org/x/crypto/ssh"
)

// newTestHostSigner returns a fresh Ed25519 SSH signer for a fake controller.
func newTestHostSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey error = %v", err)
	}
	signer, err := ssh.NewSignerFromSigner(priv)
	if err != nil {
		t.Fatalf("NewSignerFromSigner error = %v", err)
	}
	return signer
}

// serveFakeController completes one inbound SSH server handshake on conn using
// the given host key, accepts any channel, then drops the connection. It models
// the controller's host-key presentation so the reverse agent's TOFU pinning is
// exercised end to end.
func serveFakeController(conn net.Conn, hostKey ssh.Signer) {
	cfg := &ssh.ServerConfig{
		Config: ssh.Config{
			Ciphers:      transport.SupportedCiphers(),
			KeyExchanges: transport.SupportedKEX(),
			MACs:         transport.SupportedMACs(),
		},
		// Accept any client key — this test only cares about host-key pinning.
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(hostKey)

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	// Drain/close channels then tear down so the agent's session ends and the
	// RunReverse loop comes back around to reconnect.
	go func() {
		for nc := range chans {
			ch, chReqs, err := nc.Accept()
			if err != nil {
				continue
			}
			go ssh.DiscardRequests(chReqs)
			_ = ch.Close()
		}
	}()
	time.Sleep(50 * time.Millisecond)
	_ = sshConn.Close()
}

// TestReverseAcceptNewHostKeyOnlyFirstAttempt proves that --accept-new-host-key
// (forceReplace) is honored only on the FIRST connection attempt: once the agent
// has pinned the controller's key, a later reconnect that presents a DIFFERENT
// host key is rejected (no silent re-pin), closing the future-reconnect MITM
// window.
func TestReverseAcceptNewHostKeyOnlyFirstAttempt(t *testing.T) {
	t.Parallel()

	const controllerAddr = "127.0.0.1:9443"

	dir := t.TempDir()
	knownHosts := filepath.Join(dir, "known_hosts")
	hostKeyPath := filepath.Join(dir, "agent_host_key")

	firstKey := newTestHostSigner(t)
	secondKey := newTestHostSigner(t)

	var mu sync.Mutex
	attempt := 0

	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		attempt++
		n := attempt
		mu.Unlock()

		// memconn addresses must be host:port so the host-key callback (which feeds
		// the remote addr to knownhosts) can split them.
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:55555", controllerAddr)
		// Attempt 1 pins firstKey via first-use TOFU. Every LATER attempt presents
		// secondKey: with AcceptNewHostKey honored only on the first attempt, these
		// reconnects use strict pinning and must be rejected (no silent re-pin).
		key := firstKey
		if n >= 2 {
			key = secondKey
		}
		go serveFakeController(serverConn, key)
		return clientConn, nil
	}

	opts := ReverseOptions{
		ControllerAddress:  controllerAddr,
		ServerName:         "rev",
		KnownHostsPath:     knownHosts,
		AcceptNewHostKey:   true, // honored on the first attempt only
		MinRetryDelay:      time.Millisecond,
		MaxRetryDelay:      2 * time.Millisecond,
		NetworkDialContext: dial,
	}

	server := Server{
		Mode:        transport.ModeReverse,
		HostKeyPath: hostKeyPath,
	}

	// Run the loop in the background; stop it once we've observed enough attempts.
	dialCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = RunReverse(dialCtx, opts, server)
		close(done)
	}()

	// Wait until several reconnects (presenting the changed key) have happened.
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := attempt
		mu.Unlock()
		if n >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("loop did not reach the third attempt (got %d)", n)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done

	// The pinned key must still be firstKey — the changed key on later reconnects
	// must NOT have replaced it despite AcceptNewHostKey being set at startup.
	assertPinnedKeyIs(t, knownHosts, controllerAddr, firstKey.PublicKey(), secondKey.PublicKey())
}

// assertPinnedKeyIs verifies that knownHosts has pinned wantKey for address and
// would reject otherKey (proving no silent re-pin happened on reconnect).
func assertPinnedKeyIs(t *testing.T, knownHosts, address string, wantKey, otherKey ssh.PublicKey) {
	t.Helper()

	// Strict callback (forceReplace=false): the pinned key is accepted, a
	// different key is rejected.
	cb, err := transport.NewTOFUHostKeyCallback(knownHosts, false, &transport.HostKeyState{})
	if err != nil {
		t.Fatalf("NewTOFUHostKeyCallback error = %v", err)
	}
	remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9443}
	if err := cb(address, remote, wantKey); err != nil {
		t.Fatalf("pinned key should be accepted, got %v", err)
	}
	if err := cb(address, remote, otherKey); err == nil {
		t.Fatal("a changed host key must be rejected — accept-new-host-key must NOT have re-pinned on reconnect")
	}
}
