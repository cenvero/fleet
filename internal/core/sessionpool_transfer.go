// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
)

// transferChannelBudget bounds how many pooled fleet-rpc channels file transfers
// may hold at once on one server's connection. Transfers multiplex their
// streams onto the pooled connection instead of dialling their own, and the
// agent refuses channels beyond transport.MaxChannelsPerConn per connection.
// Half of that is left for control RPCs and the pool's idle cache, so a
// directory transfer running many files at once — or several transfers started
// from the daemon — runs with fewer streams instead of failing to open one.
const transferChannelBudget = transport.MaxChannelsPerConn / 2

// channelBudget counts the transfer channels still available for one server.
// free goes negative when transfers beyond the budget each take their single
// guaranteed channel.
type channelBudget struct {
	mu   sync.Mutex
	free int
}

type budgetKey struct {
	pool   *sessionPool
	server string
}

var (
	transferBudgetsMu sync.Mutex
	transferBudgets   = map[budgetKey]*channelBudget{}
)

func (a *App) transferBudget(serverName string) *channelBudget {
	key := budgetKey{pool: a.sessions, server: serverName}
	transferBudgetsMu.Lock()
	defer transferBudgetsMu.Unlock()
	b, ok := transferBudgets[key]
	if !ok {
		b = &channelBudget{free: transferChannelBudget}
		transferBudgets[key] = b
	}
	return b
}

// acquire takes up to want channel slots and never fewer than one. It never
// waits: a transfer that finds the budget exhausted still gets its single
// channel (running one stream), so transfers holding channels on two servers
// — a relay copy — can never deadlock against each other.
func (b *channelBudget) acquire(want int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	got := max(1, min(want, b.free))
	b.free -= got
	return got
}

// tryAcquire takes up to want slots without waiting.
func (b *channelBudget) tryAcquire(want int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	got := max(0, min(want, b.free))
	b.free -= got
	return got
}

func (b *channelBudget) release(n int) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.free += n
	b.mu.Unlock()
}

// transferChannels is a set of fleet-rpc channels leased from the session pool
// for one transfer: one worker slot per channel, all multiplexed on the pooled
// SSH connection. A slot whose channel breaks is replaced on its next call — on
// the same connection if that is still alive, otherwise on a freshly dialled
// pooled connection.
type transferChannels struct {
	app    *App
	server ServerRecord
	budget *channelBudget

	mu     sync.Mutex
	leases []*lease
	tokens int
	// owned holds clients of leases the pool does not manage (no pool, or the
	// pool was closed); close() shuts them down.
	owned []*ssh.Client
	caps  []string
}

// leaseTransferChannels borrows up to want channels to server. It always
// returns at least one; fewer than want is normal when the budget is busy or
// the agent refuses extra channels, and only means fewer parallel streams.
func (a *App) leaseTransferChannels(server ServerRecord, want int) (*transferChannels, error) {
	tc := &transferChannels{app: a, server: server, budget: a.transferBudget(server.Name)}
	tc.tokens = tc.budget.acquire(want)
	first, err := a.acquireTransferLease(server)
	if err != nil {
		tc.budget.release(tc.tokens)
		return nil, err
	}
	tc.track(first)
	tc.leases = []*lease{first}
	tc.caps = sessionCapabilities(first.session())
	if len(tc.caps) == 0 {
		// Pooled sessions always carry their hello capabilities; this only
		// guards a session built some other way.
		if fresh, err := a.GetServer(server.Name); err == nil {
			tc.caps = fresh.Capabilities
		}
	}
	tc.grow(tc.tokens)
	return tc, nil
}

// knownTransferCapabilities is every capability a transfer consults.
var knownTransferCapabilities = []string{
	proto.CapabilityBinaryFrames,
	proto.CapabilityFileChunkDigests,
	proto.CapabilityFilePut,
	proto.CapabilityFileReadStat,
	proto.CapabilityFileCopy,
	proto.CapabilityFileTree,
}

// sessionCapabilities reports the transfer capabilities a pooled session learned
// from its agent's hello.
func sessionCapabilities(s *transport.Session) []string {
	var caps []string
	for _, c := range knownTransferCapabilities {
		if s.SupportsCapability(c) {
			caps = append(caps, c)
		}
	}
	return caps
}

// acquireTransferLease borrows one channel from the pool, dialling the pooled
// connection if there is none.
func (a *App) acquireTransferLease(server ServerRecord) (*lease, error) {
	if l, ok := a.sessions.acquire(server.Name); ok {
		return l, nil
	}
	return a.dialPooledContext(context.Background(), server)
}

func (tc *transferChannels) track(l *lease) {
	if l != nil && l.pool == nil && l.session() != nil && l.session().Client != nil {
		tc.mu.Lock()
		tc.owned = append(tc.owned, l.session().Client)
		tc.mu.Unlock()
	}
}

// grow adds channels until the set holds want of them, as far as budget allows.
// Idle pooled channels are reused first; the rest are opened concurrently on
// the pooled connection, so setting up N streams costs about one round trip
// instead of N.
func (tc *transferChannels) grow(want int) {
	tc.mu.Lock()
	if extra := want - tc.tokens; extra > 0 {
		tc.tokens += tc.budget.tryAcquire(extra)
	}
	want = min(want, tc.tokens)
	have := len(tc.leases)
	base := tc.leases[0]
	tc.mu.Unlock()
	if have >= want || base == nil {
		return
	}
	var added []*lease
	for len(added) < want-have {
		l, ok := tc.app.sessions.acquire(tc.server.Name)
		if !ok {
			break
		}
		added = append(added, l)
		if l.fromNew {
			break // the pool had no idle channel; open the rest concurrently
		}
	}
	if missing := want - have - len(added); missing > 0 {
		opened := make([]*lease, missing)
		var wg sync.WaitGroup
		for i := range missing {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				child, err := base.session().OpenChannelSession()
				if err != nil {
					return // fewer streams than asked for is fine
				}
				opened[i] = &lease{pool: base.pool, name: base.name, sess: child, fromNew: true}
			}(i)
		}
		wg.Wait()
		for _, l := range opened {
			if l != nil {
				added = append(added, l)
			}
		}
	}
	tc.mu.Lock()
	tc.leases = append(tc.leases, added...)
	var unused int
	if n := tc.tokens - len(tc.leases); n > 0 {
		unused = n
		tc.tokens -= n
	}
	tc.mu.Unlock()
	tc.budget.release(unused)
}

// size is the number of worker slots.
func (tc *transferChannels) size() int {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return len(tc.leases)
}

func (tc *transferChannels) supports(capability string) bool {
	for _, c := range tc.caps {
		if c == capability {
			return true
		}
	}
	return false
}

// call runs one RPC on slot's channel. A transport failure retires that
// channel — not the pooled connection its siblings share — and the next call
// on the slot gets a replacement.
func (tc *transferChannels) call(slot int, env proto.Envelope) (proto.Envelope, error) {
	l, err := tc.slotLease(slot)
	if err != nil {
		return proto.Envelope{}, err
	}
	resp, err := l.session().Call(context.Background(), env)
	if err != nil && !transport.SessionUsableAfterError(err) {
		tc.retire(slot, l)
		return resp, err
	}
	// A long transfer keeps the pool's connection busy through channels the
	// pool cannot see; stop it from being retired as idle underneath us.
	tc.app.sessions.touchConn(tc.server.Name, l.session().Client)
	return resp, err
}

func (tc *transferChannels) slotLease(slot int) (*lease, error) {
	tc.mu.Lock()
	l := tc.leases[slot]
	tc.mu.Unlock()
	if l != nil {
		return l, nil
	}
	repl, err := tc.app.acquireTransferLease(tc.server)
	if err != nil {
		return nil, err
	}
	tc.track(repl)
	tc.mu.Lock()
	tc.leases[slot] = repl
	tc.mu.Unlock()
	return repl, nil
}

// retire closes a broken channel. When its connection can no longer open
// channels either, that connection is evicted from the pool — but only if the
// pool still holds that very connection, so a failure noticed late on an old
// connection never tears down the replacement a sibling already dialled.
func (tc *transferChannels) retire(slot int, l *lease) {
	tc.mu.Lock()
	if tc.leases[slot] == l {
		tc.leases[slot] = nil
	}
	tc.mu.Unlock()
	sess := l.session()
	if sess.Channel != nil {
		// Close only this channel: Session.Close on the pool's root session
		// would close the client every sibling channel rides on.
		_ = sess.Channel.Close()
	}
	fresh, err := sess.OpenChannelSession()
	if err != nil {
		tc.app.sessions.evictConn(tc.server.Name, sess.Client)
		return
	}
	tc.mu.Lock()
	if tc.leases[slot] == nil {
		tc.leases[slot] = &lease{pool: l.pool, name: l.name, sess: fresh, fromNew: true}
		fresh = nil
	}
	tc.mu.Unlock()
	if fresh != nil {
		_ = fresh.Close()
	}
}

// close hands every healthy channel back to the pool and returns the budget.
func (tc *transferChannels) close() {
	tc.mu.Lock()
	leases, owned, tokens := tc.leases, tc.owned, tc.tokens
	tc.leases, tc.owned, tc.tokens = nil, nil, 0
	tc.mu.Unlock()
	for _, l := range leases {
		if l == nil {
			continue
		}
		if l.pool != nil && !l.pool.isCurrentConn(l.name, l.session().Client) {
			// The pool has moved on to another connection (this one was
			// evicted or replaced mid-transfer); never hand it a channel of the
			// old one.
			if ch := l.session().Channel; ch != nil {
				_ = ch.Close()
			}
			continue
		}
		l.release()
	}
	for _, c := range owned {
		_ = c.Close()
	}
	tc.budget.release(tokens)
}

// ---- small hooks into the session pool ----
//
// These read sessionPool/pooledServer fields directly so sessionpool.go stays
// untouched; they belong there as methods if the pool grows equivalent API.

// touchConn refreshes the idle timer of serverName's pooled connection if it is
// still backed by client.
func (sp *sessionPool) touchConn(serverName string, client *ssh.Client) {
	if sp == nil || client == nil {
		return
	}
	sp.mu.Lock()
	if entry, ok := sp.entries[serverName]; ok && entry.root != nil && entry.root.Client == client {
		entry.lastUsed = time.Now()
	}
	sp.mu.Unlock()
}

// isCurrentConn reports whether serverName's pooled connection is backed by
// client.
func (sp *sessionPool) isCurrentConn(serverName string, client *ssh.Client) bool {
	if sp == nil || client == nil {
		return false
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	entry, ok := sp.entries[serverName]
	return ok && entry.root != nil && entry.root.Client == client
}

// evictConn drops serverName's pooled connection only if it is still backed by
// client.
func (sp *sessionPool) evictConn(serverName string, client *ssh.Client) {
	if sp == nil || client == nil {
		return
	}
	sp.mu.Lock()
	entry, ok := sp.entries[serverName]
	if ok && entry.root != nil && entry.root.Client == client {
		delete(sp.entries, serverName)
	} else {
		ok = false
	}
	sp.mu.Unlock()
	if ok {
		entry.closeAll()
	}
}
