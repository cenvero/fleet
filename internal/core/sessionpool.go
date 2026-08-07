// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"sync"
	"time"

	"github.com/cenvero/fleet/internal/transport"
)

// sessionIdleTTL is how long a pooled SSH connection may sit unused before the
// next acquire discards it and dials fresh. It exists because the far side can
// drop a connection without telling us — an agent restart, a NAT/conntrack
// eviction, or a firewall idle timeout. Redialing after a quiet period is
// cheaper than surfacing a stale-connection error to the operator.
const sessionIdleTTL = 2 * time.Minute

// maxPooledChannelsPerServer bounds how many multiplexed fleet-rpc channels the
// pool will keep open on one server's SSH connection. Concurrent callers beyond
// this open a channel, use it, and close it rather than growing the cache without
// limit. The agent independently caps channels per connection.
const maxPooledChannelsPerServer = 8

// sessionPool keeps one live SSH connection per server and multiplexes RPC
// channels over it.
//
// Without it, every single control RPC — each directory listing, stat, mkdir,
// rename, delete, properties lookup — paid a full connection setup: TCP
// handshake, SSH version exchange, key exchange, public-key auth, channel open,
// and a hello round trip, plus a private-key read from disk and a TOML rewrite
// of the server record. That is roughly nine network round trips per keystroke
// in the file manager, which is what made browsing a remote host feel slow on
// anything but a LAN. Reusing the connection turns the steady-state cost into a
// single round trip.
//
// Security is unchanged: connections are still created only through
// openDirectSession, so host-key pinning, the curated cipher/KEX/MAC sets, and
// public-key auth all apply exactly as before. Pooling changes when we connect,
// never how we verify.
type sessionPool struct {
	mu      sync.Mutex
	entries map[string]*pooledServer
	dialMu  map[string]*sync.Mutex
	closed  bool
}

type pooledServer struct {
	root     *transport.Session   // owns the underlying ssh.Client
	idle     []*transport.Session // channels ready for reuse (root is one of them)
	lastUsed time.Time
}

func newSessionPool() *sessionPool {
	return &sessionPool{
		entries: make(map[string]*pooledServer),
		dialMu:  make(map[string]*sync.Mutex),
	}
}

// dialLock returns the per-server lock that serialises connection setup.
//
// Without it, a cold burst — a dashboard refresh, a fan-out across services —
// has every caller miss the empty pool simultaneously and dial its own
// connection, so N callers pay N handshakes and all but one of those
// connections is immediately thrown away. Holding this lock across only the dial
// (never across the RPC itself) means the first caller establishes the
// connection and the rest pick it up from the pool.
func (sp *sessionPool) dialLock(serverName string) *sync.Mutex {
	if sp == nil {
		return &sync.Mutex{} // unpooled: nothing to serialise against
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.dialMu == nil {
		sp.dialMu = make(map[string]*sync.Mutex)
	}
	mu, ok := sp.dialMu[serverName]
	if !ok {
		mu = &sync.Mutex{}
		sp.dialMu[serverName] = mu
	}
	return mu
}

// lease is a borrowed RPC channel. Exactly one of release/discard must be
// called: release returns the channel to the pool, discard tears down the whole
// server entry because the connection is no longer trustworthy.
type lease struct {
	pool    *sessionPool
	name    string
	sess    *transport.Session
	fromNew bool // channel was opened outside the pool's cached set
}

func (l *lease) session() *transport.Session { return l.sess }

// release returns a healthy channel to the pool for the next caller. A lease
// with no pool (the pool was closed, or another goroutine won the race to
// install the connection) owns its session outright and closes it here.
func (l *lease) release() {
	if l == nil || l.sess == nil {
		return
	}
	if l.pool == nil {
		_ = l.sess.Close()
		return
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	entry, ok := l.pool.entries[l.name]
	if !ok || l.pool.closed {
		_ = l.sess.Close()
		return
	}
	entry.lastUsed = time.Now()
	if l.fromNew && len(entry.idle) >= maxPooledChannelsPerServer {
		_ = l.sess.Close()
		return
	}
	entry.idle = append(entry.idle, l.sess)
}

// discard drops the entire server entry. A failed call means the connection may
// be half-dead, and every channel rides the same ssh.Client, so the safe move is
// to tear all of it down and let the next call redial.
func (l *lease) discard() {
	if l == nil || l.sess == nil {
		return
	}
	if l.pool == nil {
		_ = l.sess.Close()
		return
	}
	l.pool.mu.Lock()
	entry, ok := l.pool.entries[l.name]
	if ok {
		delete(l.pool.entries, l.name)
	}
	l.pool.mu.Unlock()
	_ = l.sess.Close()
	if ok {
		entry.closeAll()
	}
}

func (p *pooledServer) closeAll() {
	for _, s := range p.idle {
		if s != p.root {
			_ = s.Close()
		}
	}
	p.idle = nil
	if p.root != nil {
		// Closing the root closes its ssh.Client, which tears down any channel
		// still outstanding on this connection.
		_ = p.root.Close()
	}
}

// acquire borrows a channel for serverName, or reports that the caller must dial
// and hand the result to adopt. It returns (nil, false) when nothing usable is
// cached.
func (sp *sessionPool) acquire(serverName string) (*lease, bool) {
	if sp == nil {
		return nil, false
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.closed {
		return nil, false
	}
	entry, ok := sp.entries[serverName]
	if !ok {
		return nil, false
	}
	// A connection that has been quiet for a while may have been dropped by the
	// far side or an intermediary without a FIN we ever noticed. Retire it.
	if time.Since(entry.lastUsed) > sessionIdleTTL {
		delete(sp.entries, serverName)
		go entry.closeAll()
		return nil, false
	}
	if n := len(entry.idle); n > 0 {
		sess := entry.idle[n-1]
		entry.idle = entry.idle[:n-1]
		return &lease{pool: sp, name: serverName, sess: sess}, true
	}
	// All cached channels are busy. Multiplex another one onto the existing
	// connection — far cheaper than a second full handshake.
	if entry.root != nil {
		child, err := entry.root.OpenChannelSession()
		if err == nil {
			return &lease{pool: sp, name: serverName, sess: child, fromNew: true}, true
		}
		// The connection can no longer open channels; treat it as dead.
		delete(sp.entries, serverName)
		go entry.closeAll()
	}
	return nil, false
}

// adopt installs a freshly dialled session as the pooled connection for
// serverName and returns a lease on it. If an entry already exists (another
// goroutine raced us), the newcomer is kept as an extra channel source and the
// loser is closed, so we never leak a connection.
func (sp *sessionPool) adopt(serverName string, sess *transport.Session) *lease {
	if sess == nil {
		return nil
	}
	// No pool (an App built directly, e.g. in tests) means no caching: the lease
	// owns the session and release closes it, exactly matching the original
	// dial-per-call behaviour.
	if sp == nil {
		return &lease{name: serverName, sess: sess}
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.closed {
		return &lease{pool: nil, name: serverName, sess: sess}
	}
	if existing, ok := sp.entries[serverName]; ok && existing.root != nil {
		// Someone else already established the pooled connection. Hand this one
		// back to the caller as a one-shot; release() will close it because the
		// entry it belongs to is not this one.
		existing.lastUsed = time.Now()
		return &lease{pool: nil, name: serverName, sess: sess}
	}
	sp.entries[serverName] = &pooledServer{root: sess, lastUsed: time.Now()}
	return &lease{pool: sp, name: serverName, sess: sess}
}

// evict drops any pooled connection for a server. Call after anything that
// invalidates the connection's identity or credentials — key rotation, a
// re-pinned host key, or removing the server.
func (sp *sessionPool) evict(serverName string) {
	if sp == nil {
		return
	}
	sp.mu.Lock()
	entry, ok := sp.entries[serverName]
	if ok {
		delete(sp.entries, serverName)
	}
	sp.mu.Unlock()
	if ok {
		entry.closeAll()
	}
}

// disconnectAll closes every pooled connection but leaves the pool usable, so a
// later call simply redials. Use when connections should be released without
// shutting the App down.
func (sp *sessionPool) disconnectAll() {
	if sp == nil {
		return
	}
	sp.mu.Lock()
	entries := sp.entries
	sp.entries = make(map[string]*pooledServer)
	sp.mu.Unlock()
	for _, entry := range entries {
		entry.closeAll()
	}
}

// closeAllServers tears down every pooled connection and retires the pool.
// Called from App.Close.
func (sp *sessionPool) closeAllServers() {
	if sp == nil {
		return
	}
	sp.disconnectAll()
	sp.mu.Lock()
	sp.closed = true
	sp.mu.Unlock()
}
