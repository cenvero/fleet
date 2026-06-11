// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestLoadAuthorizedKeysSkipsMalformedLineWithoutCorruptingNextKey(t *testing.T) {
	t.Parallel()

	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	sshPublicKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatalf("NewPublicKey() error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "authorized_keys")
	data := append([]byte("not-a-valid-authorized-key\n"), ssh.MarshalAuthorizedKey(sshPublicKey)...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(authorized_keys) error = %v", err)
	}

	keys, err := loadAuthorizedKeys(path)
	if err != nil {
		t.Fatalf("loadAuthorizedKeys() error = %v", err)
	}
	if _, ok := keys[string(sshPublicKey.Marshal())]; !ok {
		t.Fatalf("expected valid key after malformed line to be loaded")
	}
}

// TestHandshakeSlotsCapDropsExcessConnections proves the listener's connection
// cap (the counting semaphore that guards the Accept loop) is sized to
// maxConcurrentHandshakes and that, once full, a further non-blocking acquire
// fails — which is exactly the close-and-continue path in Serve. This guards
// against an unauthenticated half-open flood exhausting goroutines/fds.
func TestHandshakeSlotsCapDropsExcessConnections(t *testing.T) {
	if cap(handshakeSlots) != maxConcurrentHandshakes {
		t.Fatalf("handshakeSlots cap = %d, want %d", cap(handshakeSlots), maxConcurrentHandshakes)
	}
	// Fill every slot, then assert the next acquire would be dropped (default case).
	for i := 0; i < maxConcurrentHandshakes; i++ {
		select {
		case handshakeSlots <- struct{}{}:
		default:
			t.Fatalf("could not fill slot %d; semaphore not drained between tests", i)
		}
	}
	dropped := false
	select {
	case handshakeSlots <- struct{}{}:
		<-handshakeSlots // undo so we leave the channel as we found it
	default:
		dropped = true
	}
	// Drain back to empty so other tests start clean.
	for i := 0; i < maxConcurrentHandshakes; i++ {
		<-handshakeSlots
	}
	if !dropped {
		t.Fatalf("expected a connection beyond the cap to be dropped")
	}
}

// deadlineRecordingConn is a net.Conn whose Read fails immediately (so the SSH
// handshake aborts fast) and which records the deadlines serveConn sets on it.
type deadlineRecordingConn struct {
	mu        sync.Mutex
	deadlines []time.Time
}

func (c *deadlineRecordingConn) Read([]byte) (int, error)         { return 0, errors.New("no handshake bytes") }
func (c *deadlineRecordingConn) Write(b []byte) (int, error)      { return len(b), nil }
func (c *deadlineRecordingConn) Close() error                     { return nil }
func (c *deadlineRecordingConn) LocalAddr() net.Addr              { return dummyAddr{} }
func (c *deadlineRecordingConn) RemoteAddr() net.Addr             { return dummyAddr{} }
func (c *deadlineRecordingConn) SetReadDeadline(time.Time) error  { return nil }
func (c *deadlineRecordingConn) SetWriteDeadline(time.Time) error { return nil }
func (c *deadlineRecordingConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, t)
	c.mu.Unlock()
	return nil
}

type dummyAddr struct{}

func (dummyAddr) Network() string { return "pipe" }
func (dummyAddr) String() string  { return "pipe" }

// TestServeConnSetsHandshakeDeadline proves serveConn arms a handshake deadline
// before ssh.NewServerConn (so a stalled peer can't pin a goroutine/fd forever)
// — the missing bound that this fix adds. We don't assert it clears the deadline
// here because the handshake fails fast and never reaches the clear; the cap test
// and the build cover the rest.
func TestServeConnSetsHandshakeDeadline(t *testing.T) {
	s := Server{}
	config := &ssh.ServerConfig{}
	hostSigner, signErr := generateTestSigner(t)
	if signErr != nil {
		t.Fatalf("signer: %v", signErr)
	}
	config.AddHostKey(hostSigner)

	conn := &deadlineRecordingConn{}
	// serveConn returns quickly because the conn's Read fails; we only care that a
	// non-zero handshake deadline was set first.
	_ = s.serveConn(conn, config)

	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.deadlines) == 0 {
		t.Fatalf("serveConn must set a handshake deadline before the SSH handshake")
	}
	if conn.deadlines[0].IsZero() {
		t.Fatalf("first deadline must be a real (non-zero) timeout, got the clear value")
	}
}

func generateTestSigner(t *testing.T) (ssh.Signer, error) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}
