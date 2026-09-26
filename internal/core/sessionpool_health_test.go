// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
	"golang.org/x/crypto/ssh"
)

// stallingConn is an ssh.Conn whose channel opens take `delay` (or hang until
// closed when delay is negative) — a slow or half-dead server.
type stallingConn struct {
	delay     time.Duration
	opens     atomic.Int32
	closed    chan struct{}
	closeOnce sync.Once
}

func newStallingConn(delay time.Duration) *stallingConn {
	return &stallingConn{delay: delay, closed: make(chan struct{})}
}

func (c *stallingConn) OpenChannel(string, []byte) (ssh.Channel, <-chan *ssh.Request, error) {
	c.opens.Add(1)
	var wait <-chan time.Time
	if c.delay >= 0 {
		wait = time.After(c.delay)
	}
	select {
	case <-wait:
		return nil, nil, &ssh.OpenChannelError{Reason: ssh.ResourceShortage, Message: "busy"}
	case <-c.closed:
		return nil, nil, errors.New("connection closed")
	}
}
func (c *stallingConn) SendRequest(string, bool, []byte) (bool, []byte, error) {
	<-c.closed
	return false, nil, errors.New("connection closed")
}
func (c *stallingConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}
func (c *stallingConn) Wait() error           { <-c.closed; return nil }
func (c *stallingConn) User() string          { return "stall" }
func (c *stallingConn) SessionID() []byte     { return nil }
func (c *stallingConn) ClientVersion() []byte { return nil }
func (c *stallingConn) ServerVersion() []byte { return nil }
func (c *stallingConn) RemoteAddr() net.Addr  { return &net.TCPAddr{} }
func (c *stallingConn) LocalAddr() net.Addr   { return &net.TCPAddr{} }

// TestSessionPoolSlowServerDoesNotBlockOthers is the regression for acquire
// holding the pool lock while it waited for a channel-open confirmation: one
// slow server stalled every acquire and release for every other server.
func TestSessionPoolSlowServerDoesNotBlockOthers(t *testing.T) {
	sp := newSessionPool()
	slow := newStallingConn(300 * time.Millisecond)
	defer slow.Close()
	sp.entries["slow"] = &pooledServer{
		root:     &transport.Session{Client: &ssh.Client{Conn: slow}},
		lastUsed: time.Now(),
	}
	fastIdle := &transport.Session{}
	sp.entries["fast"] = &pooledServer{
		root:     &transport.Session{},
		idle:     []*transport.Session{fastIdle},
		lastUsed: time.Now(),
	}

	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		sp.acquire("slow") // all channels busy: opens one, which takes 300ms
	}()
	for slow.opens.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	start := time.Now()
	l, ok := sp.acquire("fast")
	elapsed := time.Since(start)
	t.Logf("acquire(fast) while slow server opens a channel: %s", elapsed)
	if !ok || l.session() != fastIdle {
		t.Fatalf("acquire(fast) = %v, %v; want the idle channel", l, ok)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("acquire for a healthy server waited %s behind another server's channel open", elapsed)
	}
	l.release()
	<-slowDone
}

// A refusal (the agent at its channel cap) says the connection is alive; only
// a timeout retires it.
func TestSessionPoolChannelOpenTimeoutRetiresConnection(t *testing.T) {
	restore := pooledChannelOpenTimeout
	pooledChannelOpenTimeout = 30 * time.Millisecond
	defer func() { pooledChannelOpenTimeout = restore }()

	sp := newSessionPool()
	refusing := newStallingConn(0)
	defer refusing.Close()
	sp.entries["refusing"] = &pooledServer{root: &transport.Session{Client: &ssh.Client{Conn: refusing}}, lastUsed: time.Now()}
	if _, ok := sp.acquire("refusing"); ok {
		t.Fatal("a refused channel open cannot produce a lease")
	}
	if _, ok := sp.entries["refusing"]; !ok {
		t.Fatal("a connection that answered (with a refusal) must stay pooled")
	}

	hung := newStallingConn(-1)
	sp.entries["hung"] = &pooledServer{root: &transport.Session{Client: &ssh.Client{Conn: hung}}, lastUsed: time.Now()}
	start := time.Now()
	if _, ok := sp.acquire("hung"); ok {
		t.Fatal("a hung channel open cannot produce a lease")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("channel open on a half-dead connection blocked for %s", elapsed)
	}
	sp.mu.Lock()
	_, still := sp.entries["hung"]
	sp.mu.Unlock()
	if still {
		t.Fatal("a connection that could not open a channel in time must be retired")
	}
	select {
	case <-hung.closed:
	case <-time.After(time.Second):
		t.Fatal("retired connection was not closed")
	}
}

// A channel leased from a connection that was retired and replaced while the
// call was in flight must not be handed to the replacement's callers.
func TestSessionPoolStaleLeaseIsNotRecycled(t *testing.T) {
	sp := newSessionPool()
	old := &pooledServer{root: &transport.Session{}, lastUsed: time.Now()}
	stale := &transport.Session{}
	sp.entries["s"] = old
	l := &lease{pool: sp, name: "s", sess: stale, entry: old}
	sp.retire("s", old)
	fresh := &pooledServer{root: &transport.Session{}, lastUsed: time.Now()}
	sp.entries["s"] = fresh
	l.release()
	if len(fresh.idle) != 0 {
		t.Fatal("a channel of the retired connection was pooled on its replacement")
	}
	// And discarding it must not tear down the healthy replacement.
	l2 := &lease{pool: sp, name: "s", sess: &transport.Session{}, entry: old}
	l2.discard()
	if sp.entries["s"] != fresh {
		t.Fatal("discarding a stale lease retired the replacement connection")
	}
}

// freezableConn black-holes a connection on demand: writes are swallowed and
// reads block, as when a NAT or firewall silently drops the flow.
type freezableConn struct {
	net.Conn
	frozen atomic.Bool
	gate   chan struct{}
}

func (c *freezableConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.frozen.Load() {
		<-c.gate
		return 0, errors.New("frozen connection closed")
	}
	return n, err
}

func (c *freezableConn) Write(p []byte) (int, error) {
	if c.frozen.Load() {
		return len(p), nil
	}
	return c.Conn.Write(p)
}

func (c *freezableConn) Close() error {
	if c.frozen.CompareAndSwap(true, true) {
		select {
		case <-c.gate:
		default:
			close(c.gate)
		}
	}
	return c.Conn.Close()
}

// TestPooledConnectionKeepaliveRetiresSilentPeer: a pooled connection whose
// peer silently disappears is retired by the keepalive and the next call
// redials transparently instead of hanging on the corpse.
func TestPooledConnectionKeepaliveRetiresSilentPeer(t *testing.T) {
	restoreInterval, restoreMissed := pooledKeepaliveInterval, pooledKeepaliveMaxMissed
	pooledKeepaliveInterval, pooledKeepaliveMaxMissed = 20*time.Millisecond, 3
	defer func() { pooledKeepaliveInterval, pooledKeepaliveMaxMissed = restoreInterval, restoreMissed }()

	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeDirect,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly}); err != nil {
		t.Fatal(err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	srv := agent.Server{
		Mode: transport.ModeDirect, HostKeyPath: filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
		ServiceManager:     &fakeServiceManager{services: []proto.ServiceInfo{{Name: "nginx.service"}}},
	}
	var mu sync.Mutex
	var conns []*freezableConn
	var dials atomic.Int32
	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40000", "127.0.0.1:2222")
		go func() { _ = srv.ServeConn(serverConn) }()
		fc := &freezableConn{Conn: clientConn, gate: make(chan struct{})}
		mu.Lock()
		conns = append(conns, fc)
		mu.Unlock()
		return fc, nil
	}
	if err := app.AddServer(ServerRecord{Name: "edge", Address: "127.0.0.1", Port: 2222, Mode: transport.ModeDirect, User: "cenvero-agent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ListServices("edge"); err != nil {
		t.Fatalf("first call: %v", err)
	}

	mu.Lock()
	conns[0].frozen.Store(true)
	mu.Unlock()

	// The keepalive notices within ~maxMissed intervals and retires the entry.
	deadline := time.Now().Add(3 * time.Second)
	for app.sessions.has("edge") {
		if time.Now().After(deadline) {
			t.Fatal("keepalive did not retire the silent connection")
		}
		time.Sleep(10 * time.Millisecond)
	}
	result := make(chan error, 1)
	go func() {
		_, err := app.ListServices("edge")
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("call after the connection died: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call after the connection died hung instead of redialling")
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("expected exactly one transparent redial, got %d dials", got)
	}
	app.DisconnectPooledSessions()
}
