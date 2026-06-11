// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"golang.org/x/crypto/ssh"
)

// enrollFakeConn is a minimal ssh.ConnMetadata whose User() drives authorizeAgent.
type enrollFakeConn struct{ user string }

func (f enrollFakeConn) User() string          { return f.user }
func (f enrollFakeConn) SessionID() []byte     { return []byte("sid") }
func (f enrollFakeConn) ClientVersion() []byte { return []byte("SSH-2.0-test") }
func (f enrollFakeConn) ServerVersion() []byte { return []byte("SSH-2.0-test") }
func (f enrollFakeConn) RemoteAddr() net.Addr  { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (f enrollFakeConn) LocalAddr() net.Addr   { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func newReverseTestKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey error = %v", err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("NewPublicKey error = %v", err)
	}
	return key
}

// TestReverseEnrollmentTokenGate proves the reverse-mode TOFU race is closed: a
// fresh agent key is pinned only after a valid one-time enrollment token, the
// token is consumed, and a different key is rejected once pinned.
func TestReverseEnrollmentTokenGate(t *testing.T) {
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

	const secret = "correct-horse-battery-staple"
	if err := app.AddServer(ServerRecord{Name: "rev", Mode: transport.ModeReverse, EnrollSecret: secret}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	hub := NewReverseHub(app, "")
	key := newReverseTestKey(t)
	pinPath := filepath.Join(configDir, "keys", "agents", "rev.pub")

	// No token presented → rejected; nothing pinned (closes the TOFU race).
	if _, err := hub.authorizeAgent(enrollFakeConn{user: "rev"}, key); err == nil {
		t.Fatal("expected rejection when no enrollment token is presented")
	}
	// Wrong token → rejected.
	if _, err := hub.authorizeAgent(enrollFakeConn{user: "rev:wrong"}, key); err == nil {
		t.Fatal("expected rejection with an incorrect enrollment token")
	}
	if _, statErr := os.Stat(pinPath); statErr == nil {
		t.Fatal("a key must NOT be pinned after a failed enrollment")
	}

	// Correct token → accepted, key pinned, token consumed.
	if _, err := hub.authorizeAgent(enrollFakeConn{user: "rev:" + secret}, key); err != nil {
		t.Fatalf("valid enrollment token should be accepted: %v", err)
	}
	if _, statErr := os.Stat(pinPath); statErr != nil {
		t.Fatalf("key should be pinned after enrollment: %v", statErr)
	}
	if s, _ := app.GetServer("rev"); s.EnrollSecret != "" {
		t.Fatal("enrollment token should be consumed (one-time) after pinning")
	}

	// Reconnect with the SAME key and no token → accepted (pinned match).
	if _, err := hub.authorizeAgent(enrollFakeConn{user: "rev"}, key); err != nil {
		t.Fatalf("pinned key should reconnect without a token: %v", err)
	}
	// A DIFFERENT key now → rejected (no silent re-pin), even with the (spent) token.
	other := newReverseTestKey(t)
	if _, err := hub.authorizeAgent(enrollFakeConn{user: "rev:" + secret}, other); err == nil {
		t.Fatal("a different key must be rejected once a key is pinned")
	}
}

// TestReverseEnrollmentRequiresPendingToken proves a reverse server with no
// pending token (e.g. registered before tokens existed) refuses first contact
// rather than falling back to insecure TOFU.
func TestReverseEnrollmentRequiresPendingToken(t *testing.T) {
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

	// No EnrollSecret on the record.
	if err := app.AddServer(ServerRecord{Name: "legacy", Mode: transport.ModeReverse}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}
	hub := NewReverseHub(app, "")
	if _, err := hub.authorizeAgent(enrollFakeConn{user: "legacy"}, newReverseTestKey(t)); err == nil {
		t.Fatal("a reverse server with no pending enrollment token must refuse first contact")
	}
}
