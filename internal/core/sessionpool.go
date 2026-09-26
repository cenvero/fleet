// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"os"
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

// Pooled connections are probed with SSH keepalives so a connection whose peer
// vanished without a FIN is retired within about a minute instead of hanging
// the next caller, and channel opens on them are bounded. Variables rather than
// constants so tests can shorten them.
var (
	pooledKeepaliveInterval   = transport.DefaultKeepaliveInterval
	pooledKeepaliveMaxMissed  = transport.DefaultKeepaliveMaxMissed
	pooledChannelOpenTimeout  = transport.DefaultChannelOpenTimeout
	pooledDialHelloTimeoutCap = 45 * time.Second
)

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
//
// Locking: mu guards the maps and every pooledServer's fields, and is only ever
// held for bookkeeping — never across network I/O. Opening an extra channel
// waits for a round trip to the agent, so it happens after the entry has been
// looked up and mu released; otherwise one slow or half-dead server would stall
// every acquire and release for every other server behind it.
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
	// established is when the connection was pooled; credential files changed
	// after it mean the connection was authenticated with stale material.
	established time.Time
	// dead is set when the keepalive gave up on the connection; the entry is
	// retired on the spot, and this stops any lookup racing that retirement
	// from handing the connection out again.
	dead          bool
	stopKeepalive func()
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
	// entry is the pooled connection this channel rides on. A lease only ever
	// goes back to that entry: if the server's connection was retired and
	// redialled while the call was in flight, the channel belongs to a dead
	// connection and must not be handed to the replacement's next caller.
	entry *pooledServer
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
	if !ok || l.pool.closed || (l.entry != nil && entry != l.entry) || entry.dead {
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
	_ = l.sess.Close()
	entry := l.entry
	if entry == nil {
		l.pool.mu.Lock()
		entry = l.pool.entries[l.name]
		l.pool.mu.Unlock()
	}
	// Only the entry this channel came from is retired; a replacement installed
	// since is a different, healthy connection.
	l.pool.retire(l.name, entry)
}

// detachLocked marks the entry dead and takes its idle channels, so nothing can
// hand them out again. sp.mu must be held.
func (p *pooledServer) detachLocked() []*transport.Session {
	p.dead = true
	idle := p.idle
	p.idle = nil
	return idle
}

// shutdown closes a detached entry: its keepalive, its idle channels and the
// connection itself. It must be called without the pool lock (closing waits on
// the network) and is safe to repeat.
func (p *pooledServer) shutdown(idle []*transport.Session) {
	if p.stopKeepalive != nil {
		p.stopKeepalive()
	}
	for _, s := range idle {
		if s != p.root {
			_ = s.Close()
		}
	}
	if p.root != nil {
		// Closing the root closes its ssh.Client, which tears down any channel
		// still outstanding on this connection.
		_ = p.root.Close()
	}
}

// retire removes entry from the pool if it is still the current connection for
// serverName, and closes it either way.
func (sp *sessionPool) retire(serverName string, entry *pooledServer) {
	if sp == nil || entry == nil {
		return
	}
	sp.mu.Lock()
	if current, ok := sp.entries[serverName]; ok && current == entry {
		delete(sp.entries, serverName)
	}
	idle := entry.detachLocked()
	sp.mu.Unlock()
	entry.shutdown(idle)
}

// removeAllLocked detaches every entry and empties the map. sp.mu must be held.
func (sp *sessionPool) removeAllLocked() map[*pooledServer][]*transport.Session {
	out := make(map[*pooledServer][]*transport.Session, len(sp.entries))
	for name, entry := range sp.entries {
		out[entry] = entry.detachLocked()
		delete(sp.entries, name)
	}
	return out
}

// acquire borrows a channel for serverName, or reports that the caller must dial
// and hand the result to adopt. It returns (nil, false) when nothing usable is
// cached.
func (sp *sessionPool) acquire(serverName string) (*lease, bool) {
	return sp.acquireContext(context.Background(), serverName)
}

// acquireContext is acquire bounded by ctx: opening an extra channel waits for
// the agent to confirm it, and the caller's cancellation or deadline applies to
// that wait as well as the pool's own channel-open timeout.
func (sp *sessionPool) acquireContext(ctx context.Context, serverName string) (*lease, bool) {
	if sp == nil {
		return nil, false
	}
	sp.mu.Lock()
	if sp.closed {
		sp.mu.Unlock()
		return nil, false
	}
	entry, ok := sp.entries[serverName]
	if !ok {
		sp.mu.Unlock()
		return nil, false
	}
	// A connection that has been quiet for a while may have been dropped by the
	// far side or an intermediary without a FIN we ever noticed. Retire it.
	if entry.dead || time.Since(entry.lastUsed) > sessionIdleTTL {
		delete(sp.entries, serverName)
		idle := entry.detachLocked()
		sp.mu.Unlock()
		go entry.shutdown(idle)
		return nil, false
	}
	if n := len(entry.idle); n > 0 {
		sess := entry.idle[n-1]
		entry.idle = entry.idle[:n-1]
		sp.mu.Unlock()
		return &lease{pool: sp, name: serverName, sess: sess, entry: entry}, true
	}
	root := entry.root
	sp.mu.Unlock()
	if root == nil {
		return nil, false
	}

	// All cached channels are busy. Multiplex another one onto the existing
	// connection — far cheaper than a second full handshake. This waits for the
	// agent's confirmation, so it runs without the pool lock.
	child, err := root.OpenChannelSessionContext(ctx, pooledChannelOpenTimeout)
	if err == nil {
		return &lease{pool: sp, name: serverName, sess: child, fromNew: true, entry: entry}, true
	}
	switch {
	case ctx.Err() != nil:
		// The caller gave up; that says nothing about the connection.
	case transport.ChannelOpenRejected(err):
		// The agent answered and refused (it is at its channel cap). The
		// connection is alive and stays pooled; this caller dials its own.
	default:
		// The connection could not open a channel in time, or is closed.
		sp.retire(serverName, entry)
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
	if existing, ok := sp.entries[serverName]; ok && existing.root != nil && !existing.dead {
		// Someone else already established the pooled connection. Hand this one
		// back to the caller as a one-shot; release() will close it because the
		// entry it belongs to is not this one.
		existing.lastUsed = time.Now()
		return &lease{pool: nil, name: serverName, sess: sess}
	}
	entry := &pooledServer{root: sess, lastUsed: time.Now(), established: time.Now()}
	// Probe the connection while it is pooled. When the probe gives up it has
	// already closed the connection; retiring the entry right away means the
	// next caller redials instead of tripping over the corpse.
	entry.stopKeepalive = sess.StartKeepalive(pooledKeepaliveInterval, pooledKeepaliveMaxMissed, func() {
		sp.retire(serverName, entry)
	})
	sp.entries[serverName] = entry
	return &lease{pool: sp, name: serverName, sess: sess, entry: entry}
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
	var idle []*transport.Session
	if ok {
		delete(sp.entries, serverName)
		idle = entry.detachLocked()
	}
	sp.mu.Unlock()
	if ok {
		entry.shutdown(idle)
	}
}

// has reports whether a live pooled connection exists for serverName.
func (sp *sessionPool) has(serverName string) bool {
	if sp == nil {
		return false
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	entry, ok := sp.entries[serverName]
	return ok && !entry.dead && time.Since(entry.lastUsed) <= sessionIdleTTL
}

// evictIfCredentialsChanged drops the pooled connection for serverName when
// either credential file (the private key, the known_hosts pins) was modified
// after the connection was established — a rotated key or a re-pinned host key
// written by another process, which could not evict this process's pool.
func (sp *sessionPool) evictIfCredentialsChanged(serverName string, paths ...string) {
	if sp == nil {
		return
	}
	sp.mu.Lock()
	entry, ok := sp.entries[serverName]
	var established time.Time
	if ok {
		established = entry.established
	}
	sp.mu.Unlock()
	if !ok {
		return
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue // a missing file fails the next dial on its own
		}
		if info.ModTime().After(established) {
			sp.retire(serverName, entry)
			return
		}
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
	detached := sp.removeAllLocked()
	sp.mu.Unlock()
	for entry, idle := range detached {
		entry.shutdown(idle)
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
