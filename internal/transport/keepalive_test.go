// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package transport

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// stubConn is an ssh.Conn whose requests and channel opens either answer at
// once or hang until the connection is closed — a half-dead peer.
type stubConn struct {
	hang      atomic.Bool
	requests  atomic.Int32
	closed    chan struct{}
	closeOnce sync.Once
	openCh    ssh.Channel
}

func newStubConn() *stubConn { return &stubConn{closed: make(chan struct{})} }

func (c *stubConn) SendRequest(string, bool, []byte) (bool, []byte, error) {
	c.requests.Add(1)
	if c.hang.Load() {
		<-c.closed
		return false, nil, errors.New("connection closed")
	}
	select {
	case <-c.closed:
		return false, nil, errors.New("connection closed")
	default:
		return false, nil, nil // any reply, even a refusal, proves liveness
	}
}

func (c *stubConn) OpenChannel(string, []byte) (ssh.Channel, <-chan *ssh.Request, error) {
	if c.hang.Load() {
		<-c.closed
		return nil, nil, errors.New("connection closed")
	}
	reqs := make(chan *ssh.Request)
	close(reqs)
	return c.openCh, reqs, nil
}

func (c *stubConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}
func (c *stubConn) Wait() error           { <-c.closed; return nil }
func (c *stubConn) User() string          { return "stub" }
func (c *stubConn) SessionID() []byte     { return nil }
func (c *stubConn) ClientVersion() []byte { return nil }
func (c *stubConn) ServerVersion() []byte { return nil }
func (c *stubConn) RemoteAddr() net.Addr  { return &net.TCPAddr{} }
func (c *stubConn) LocalAddr() net.Addr   { return &net.TCPAddr{} }

func (c *stubConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func TestKeepaliveKeepsResponsiveConnection(t *testing.T) {
	conn := newStubConn()
	var dead atomic.Bool
	stop := StartKeepalive(conn, 5*time.Millisecond, 2, func() { dead.Store(true) })
	time.Sleep(80 * time.Millisecond)
	stop()
	if conn.isClosed() || dead.Load() {
		t.Fatal("keepalive closed a connection that answered every probe")
	}
	if conn.requests.Load() < 3 {
		t.Fatalf("expected regular probes, got %d", conn.requests.Load())
	}
}

// A peer that stops answering (no FIN, no RST) must be declared dead after
// maxMissed intervals and closed, and the owner told.
func TestKeepaliveRetiresUnresponsiveConnection(t *testing.T) {
	conn := newStubConn()
	conn.hang.Store(true)
	deadCh := make(chan struct{})
	start := time.Now()
	StartKeepalive(conn, 10*time.Millisecond, 3, func() { close(deadCh) })
	select {
	case <-deadCh:
	case <-time.After(2 * time.Second):
		t.Fatal("keepalive never gave up on an unresponsive connection")
	}
	if !conn.isClosed() {
		t.Fatal("dead connection was not closed")
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("gave up after %s, before maxMissed intervals elapsed", elapsed)
	}
}

// A connection closed by someone else ends probing via the failed request.
func TestKeepaliveNoticesClosedConnection(t *testing.T) {
	conn := newStubConn()
	deadCh := make(chan struct{})
	StartKeepalive(conn, 5*time.Millisecond, 100, func() { close(deadCh) })
	_ = conn.Close()
	select {
	case <-deadCh:
	case <-time.After(2 * time.Second):
		t.Fatal("keepalive did not notice the connection closing")
	}
}

func TestOpenChannelTimeoutBoundsAHalfDeadConnection(t *testing.T) {
	conn := newStubConn()
	conn.hang.Store(true)
	start := time.Now()
	_, _, err := OpenChannelTimeout(context.Background(), conn, RPCChannelType, nil, 30*time.Millisecond)
	if !errors.Is(err, ErrChannelOpenTimeout) {
		t.Fatalf("OpenChannelTimeout error = %v, want ErrChannelOpenTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("channel open took %s despite a 30ms timeout", elapsed)
	}
	_ = conn.Close() // releases the abandoned opener
}

func TestOpenChannelTimeoutHonoursContext(t *testing.T) {
	conn := newStubConn()
	conn.hang.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, _, err := OpenChannelTimeout(ctx, conn, RPCChannelType, nil, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenChannelTimeout error = %v, want context.Canceled", err)
	}
	_ = conn.Close()
}

func TestChannelOpenRejectedDistinguishesRefusalFromFailure(t *testing.T) {
	if !ChannelOpenRejected(&ssh.OpenChannelError{Reason: ssh.ResourceShortage, Message: "full"}) {
		t.Fatal("an explicit refusal must be recognised")
	}
	if ChannelOpenRejected(ErrChannelOpenTimeout) || ChannelOpenRejected(errors.New("EOF")) {
		t.Fatal("timeouts and transport errors are not refusals")
	}
}
