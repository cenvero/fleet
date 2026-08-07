// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

type boundedControlConn struct {
	reader    *bytes.Reader
	writer    bytes.Buffer
	mu        sync.Mutex
	deadlines []time.Time
}

func (c *boundedControlConn) Read(p []byte) (int, error)  { return c.reader.Read(p) }
func (c *boundedControlConn) Write(p []byte) (int, error) { return c.writer.Write(p) }
func (c *boundedControlConn) Close() error                { return nil }
func (c *boundedControlConn) LocalAddr() net.Addr         { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (c *boundedControlConn) RemoteAddr() net.Addr        { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (c *boundedControlConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, t)
	c.mu.Unlock()
	return nil
}
func (c *boundedControlConn) SetReadDeadline(time.Time) error  { return nil }
func (c *boundedControlConn) SetWriteDeadline(time.Time) error { return nil }

type fixedAddrListener struct {
	addr   net.Addr
	closed bool
}

func (l *fixedAddrListener) Accept() (net.Conn, error) { return nil, errors.New("unexpected accept") }
func (l *fixedAddrListener) Close() error              { l.closed = true; return nil }
func (l *fixedAddrListener) Addr() net.Addr            { return l.addr }

func TestControlListenerRejectsNonLoopbackBinding(t *testing.T) {
	hub := NewReverseHub(&App{}, "token")
	listener := &fixedAddrListener{addr: &net.TCPAddr{IP: net.IPv4zero, Port: 9999}}
	if err := hub.ServeControl(context.Background(), listener); err == nil {
		t.Fatal("non-loopback control listener must be rejected")
	}
	if !listener.closed {
		t.Fatal("rejected control listener should be closed")
	}
	for _, address := range []string{"0.0.0.0:9999", "192.0.2.5:9999", ":9999", "localhost:9999"} {
		if err := validateLoopbackControlAddress(address); err == nil {
			t.Fatalf("control address %q should be rejected", address)
		}
	}
	for _, address := range []string{"127.0.0.1:9999", "[::1]:9999"} {
		if err := validateLoopbackControlAddress(address); err != nil {
			t.Fatalf("loopback control address %q rejected: %v", address, err)
		}
	}
}

func TestControlRequestReadIsBoundedAndDeadlineArmed(t *testing.T) {
	payload := []byte(`{"token":"` + strings.Repeat("x", maxControlRequestBytes+1024) + `"}`)
	conn := &boundedControlConn{reader: bytes.NewReader(payload)}
	before := conn.reader.Len()
	NewReverseHub(&App{}, "token").handleControlConn(conn)
	read := before - conn.reader.Len()
	if read > maxControlRequestBytes+1 {
		t.Fatalf("control handler read %d bytes, limit is %d", read, maxControlRequestBytes+1)
	}
	if len(conn.deadlines) == 0 || conn.deadlines[0].IsZero() {
		t.Fatal("control handler must arm an I/O deadline before decoding")
	}
	if conn.writer.Len() == 0 {
		t.Fatal("control handler did not return a bounded decode error")
	}
}

func TestReverseAuthenticatedConnectionLimitPerServer(t *testing.T) {
	hub := NewReverseHub(&App{}, "token")
	for i := 0; i < maxReverseConnectionsPerServer; i++ {
		if !hub.acquireAuthenticated("edge") {
			t.Fatalf("connection %d within per-server cap was rejected", i)
		}
	}
	if hub.acquireAuthenticated("edge") {
		t.Fatal("connection beyond reverse per-server cap must be rejected")
	}
	hub.releaseAuthenticated("edge")
	if !hub.acquireAuthenticated("edge") {
		t.Fatal("released reverse connection capacity should be reusable")
	}
}

type failingSSHChannel struct{ closed bool }

func (c *failingSSHChannel) Read([]byte) (int, error)                       { return 0, io.EOF }
func (c *failingSSHChannel) Write([]byte) (int, error)                      { return 0, errors.New("child channel failed") }
func (c *failingSSHChannel) Close() error                                   { c.closed = true; return nil }
func (c *failingSSHChannel) CloseWrite() error                              { return nil }
func (c *failingSSHChannel) SendRequest(string, bool, []byte) (bool, error) { return false, nil }
func (c *failingSSHChannel) Stderr() io.ReadWriter                          { return &bytes.Buffer{} }

type trackingCloser struct{ closed bool }

func (c *trackingCloser) Close() error { c.closed = true; return nil }

func TestReverseChildChannelFailureClosesAndRetiresConnection(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeReverse, CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly}); err != nil {
		t.Fatal(err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err := app.AddServer(ServerRecord{Name: "edge", Mode: transport.ModeReverse}); err != nil {
		t.Fatal(err)
	}
	channel := &failingSSHChannel{}
	closer := &trackingCloser{}
	primary := &transport.Session{Mode: transport.ModeReverse, Channel: channel, Closer: closer}
	hub := NewReverseHub(app, "token")
	hub.sessions["edge"] = &reverseSession{session: primary}
	if _, err := hub.CallContext(context.Background(), "edge", proto.Envelope{Action: "hello"}); err == nil {
		t.Fatal("failed reverse child channel unexpectedly succeeded")
	}
	if !channel.closed || !closer.closed {
		t.Fatalf("failed reverse connection was not fully closed: channel=%v closer=%v", channel.closed, closer.closed)
	}
	if _, err := hub.Status("edge"); err == nil {
		t.Fatal("failed reverse connection remained registered")
	}
}
