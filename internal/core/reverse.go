// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	fleetcrypto "github.com/cenvero/fleet/internal/crypto"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/internal/version"
	"github.com/cenvero/fleet/pkg/proto"
	"golang.org/x/crypto/ssh"
)

type ReverseSessionInfo struct {
	Server             string             `json:"server"`
	Connected          bool               `json:"connected"`
	ConnectedAt        time.Time          `json:"connected_at,omitempty"`
	HostKeyFingerprint string             `json:"host_key_fingerprint,omitempty"`
	Hello              proto.HelloPayload `json:"hello,omitempty"`
	ReplayedMetrics    int                `json:"replayed_metrics,omitempty"`
}

// maxReverseExtraChannels bounds how many additional fleet-rpc channels the
// controller will open back to one reverse agent. It must stay at or below the
// agent's own inbound cap; transfers use far fewer.
const maxReverseExtraChannels = 16

// Reverse connections are probed with SSH keepalives so an agent that vanished
// without a FIN is noticed (and its session cleared) within about a minute, and
// opening an extra channel back to an agent is bounded. Variables so tests can
// shorten them.
var (
	reverseKeepaliveInterval  = transport.DefaultKeepaliveInterval
	reverseKeepaliveMaxMissed = transport.DefaultKeepaliveMaxMissed
	reverseChannelOpenTimeout = transport.DefaultChannelOpenTimeout
)

type reverseSession struct {
	session *transport.Session // the channel the agent opened; always usable
	conn    ssh.Conn           // opens additional channels back to the agent
	info    ReverseSessionInfo

	// multiplex is set when the agent advertised CapabilityReverseMultiplex.
	// Without it the controller never attempts an extra channel and every call
	// serialises on the agent-opened one, exactly as before.
	multiplex bool

	// lanes bounds concurrent calls on a multiplexed session to the channels
	// it can offer: every extra channel plus the primary. A caller beyond that
	// waits here — honouring its context — instead of piling onto the primary
	// channel, where it used to queue invisibly behind whoever held it, past
	// its own deadline, while pinning a daemon control slot.
	lanes chan struct{}
	// done is closed when the session is retired so lane waiters give up at
	// once instead of waiting out their deadline on a dead connection.
	done      chan struct{}
	closeOnce sync.Once

	mu     sync.Mutex
	idle   []*transport.Session // extra channels free for reuse
	opened int                  // extra channels created so far
}

func newReverseSession(session *transport.Session, conn ssh.Conn, info ReverseSessionInfo) *reverseSession {
	rs := &reverseSession{
		session: session,
		conn:    conn,
		info:    info,
		multiplex: slices.Contains(info.Hello.Capabilities, proto.CapabilityReverseMultiplex) &&
			conn != nil,
		done: make(chan struct{}),
	}
	if rs.multiplex {
		rs.lanes = make(chan struct{}, maxReverseExtraChannels+1)
	}
	return rs
}

// markDone releases anyone waiting for a lane on this session.
func (rs *reverseSession) markDone() {
	if rs == nil || rs.done == nil {
		return
	}
	rs.closeOnce.Do(func() { close(rs.done) })
}

// acquireLane waits for a free channel lane on a multiplexed session. Sessions
// without multiplexing have a single channel whose own lock serialises callers,
// so they need no lane.
func (rs *reverseSession) acquireLane(ctx context.Context) (release func(), err error) {
	if rs.lanes == nil {
		return func() {}, nil
	}
	select {
	case rs.lanes <- struct{}{}:
		return func() { <-rs.lanes }, nil
	case <-rs.done:
		return nil, fmt.Errorf("reverse session for %q closed while waiting for a free channel", rs.info.Server)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// leaseChannel borrows a channel for one RPC. It prefers an idle extra channel,
// then opens a new one, and finally falls back to the agent-opened channel. The
// bool reports whether the caller should return it via releaseChannel; the
// primary channel is shared and must not be pooled. An error means the agent
// connection could not even open a channel in time and should be retired.
func (rs *reverseSession) leaseChannel(ctx context.Context) (*transport.Session, bool, error) {
	if !rs.multiplex || rs.conn == nil {
		return rs.session, false, nil
	}
	rs.mu.Lock()
	if n := len(rs.idle); n > 0 {
		sess := rs.idle[n-1]
		rs.idle = rs.idle[:n-1]
		rs.mu.Unlock()
		return sess, true, nil
	}
	if rs.opened >= maxReverseExtraChannels {
		rs.mu.Unlock()
		// At the cap. The lane this caller holds guarantees the primary channel
		// is the one left free (unless extra opens were refused, in which case
		// callers share it exactly as before).
		return rs.session, false, nil
	}
	rs.opened++
	rs.mu.Unlock()

	// Opening waits for the agent's confirmation, so it happens outside rs.mu
	// and is bounded: without a timeout a half-dead connection held the caller
	// until TCP gave up.
	channel, requests, err := transport.OpenChannelTimeout(ctx, rs.conn, transport.RPCChannelType, nil, reverseChannelOpenTimeout)
	if err != nil {
		rs.mu.Lock()
		if rs.opened > 0 {
			rs.opened--
		}
		rs.mu.Unlock()
		switch {
		case errors.Is(err, transport.ErrChannelOpenTimeout):
			return nil, false, err
		case ctx.Err() != nil:
			return nil, false, ctx.Err()
		}
		// An agent that rejects inbound channels (or a connection on its way
		// out) drops us back to the single-channel path rather than failing.
		return rs.session, false, nil
	}
	go ssh.DiscardRequests(requests)
	sess := &transport.Session{
		Mode:               transport.ModeReverse,
		LocalAddr:          rs.session.LocalAddr,
		RemoteAddr:         rs.session.RemoteAddr,
		HostKeyFingerprint: rs.session.HostKeyFingerprint,
		Channel:            channel,
	}
	sess.SetCapabilities(rs.info.Hello.Capabilities)
	return sess, true, nil
}

func (rs *reverseSession) releaseChannel(sess *transport.Session, healthy bool) {
	if sess == nil {
		return
	}
	if !healthy {
		rs.mu.Lock()
		// closeExtraChannels may have reset the counter while this call was in
		// flight, so floor it rather than letting it drift negative.
		if rs.opened > 0 {
			rs.opened--
		}
		rs.mu.Unlock()
		_ = sess.Close()
		return
	}
	rs.mu.Lock()
	rs.idle = append(rs.idle, sess)
	rs.mu.Unlock()
}

// closeExtraChannels tears down the pooled channels. The primary channel and the
// underlying connection are owned by the caller.
func (rs *reverseSession) closeExtraChannels() {
	rs.markDone()
	rs.mu.Lock()
	idle := rs.idle
	rs.idle = nil
	rs.opened = 0
	rs.mu.Unlock()
	for _, sess := range idle {
		_ = sess.Close()
	}
}

type ReverseHub struct {
	app          *App
	mu           sync.RWMutex
	sessions     map[string]*reverseSession
	controlToken string
	// enrollMu serializes the read-pin → verify-token → write-pin → clear-token
	// enrollment sequence in authorizeAgent. Without it, two connections racing
	// the FIRST enrollment with the same one-time token can both read "no pin",
	// both pass the ConstantTimeCompare, and both write a key — last-writer-wins
	// the pin (and a stolen one-time token could be consumed twice). Holding this
	// across the whole read→verify→write→clear makes token consumption atomic so
	// exactly one connection can enroll. Enrollment is rare and the section is
	// cheap, so a single hub-wide lock is sufficient.
	enrollMu sync.Mutex

	activeMu       sync.Mutex
	activeTotal    int
	activeByServer map[string]int
	controlSlots   chan struct{}
	// controlWaiters bounds how many control connections may be queued waiting
	// for a handler slot; beyond it a connection is refused outright.
	controlWaiters chan struct{}
	// controlCalls bounds authenticated control calls in progress.
	controlCalls chan struct{}

	// bg tracks the goroutines a registered session leaves behind (clearing
	// it when its connection ends, replaying its metrics backlog). They write
	// the server record and the audit log, so Close waits for them: before,
	// they could still be writing after the hub and the App were closed and
	// the config directory was gone. bgClosed, under bgMu, stops new ones
	// from starting once Close has begun.
	bgMu     sync.Mutex
	bgClosed bool
	bg       sync.WaitGroup
}

// reverseControlRequest is the JSON wrapper a CLI process sends over the local
// control socket so the daemon can relay it to a connected reverse agent.
//
// EnvelopeBinary exists because proto.Envelope.Binary is tagged `json:"-"`: the
// wire codec carries a binary frame itself, outside the JSON. That is right for
// the SSH hop but means a plain json.Marshal of the envelope silently discards a
// file chunk. This hop is a local loopback socket, not the SSH transport, so the
// attachment is carried here as an explicit field. Without it a reverse-mode
// transfer against a binary-frame-capable agent ships chunks with no bytes.
//
// That field is base64 inside JSON, which costs ~30 ms and ~17 MB of garbage
// per 2 MiB chunk on each side. On a mutually authenticated connection, a
// daemon that advertises controlCapBinaryFrame in its auth challenge also
// accepts the "call.framed" request type, where BinaryLength announces that
// the attachment follows the JSON line as raw bytes; and a caller that lists
// controlCapBinaryFrame in Accept gets the response's attachment the same way.
// Legacy (raw-token) requests keep the original base64 encoding both ways.
type reverseControlRequest struct {
	// ClientProof replaces Token on a mutually authenticated connection (see
	// reverse_control.go). It is the first member so the daemon can check it
	// before reading the rest of the request.
	ClientProof string `json:"client_proof,omitempty"`
	// Token is the legacy credential: a caller from before mutual
	// authentication sends it as the first member.
	Token          string         `json:"token,omitempty"`
	Type           string         `json:"type"`
	Server         string         `json:"server"`
	Envelope       proto.Envelope `json:"envelope,omitempty"`
	EnvelopeBinary []byte         `json:"envelope_binary,omitempty"`
	Accept         []string       `json:"accept,omitempty"`
	BinaryLength   *int           `json:"binary_length,omitempty"`
	// Direct identifies the server record the caller resolved for a
	// "call.direct" request, so the daemon only relays over its own pooled
	// connection when both processes agree on where and how to connect.
	Direct *controlDirectTarget `json:"direct,omitempty"`
}

func newReverseControlRequest(token, kind, server string, env proto.Envelope) reverseControlRequest {
	return reverseControlRequest{
		Token:          token,
		Type:           kind,
		Server:         server,
		Envelope:       env,
		EnvelopeBinary: env.Binary,
	}
}

// envelope reattaches the binary frame carried alongside the JSON.
func (r reverseControlRequest) envelope() proto.Envelope {
	env := r.Envelope
	if r.EnvelopeBinary != nil {
		env.Binary = r.EnvelopeBinary
	}
	return env
}

type reverseControlResponse struct {
	Response       *proto.Envelope     `json:"response,omitempty"`
	ResponseBinary []byte              `json:"response_binary,omitempty"`
	BinaryLength   *int                `json:"binary_length,omitempty"`
	Status         *ReverseSessionInfo `json:"status,omitempty"`
	Error          *proto.Error        `json:"error,omitempty"`
	// Capabilities answers a "hello": the control-protocol features this daemon
	// understands.
	Capabilities []string `json:"capabilities,omitempty"`
}

// setResponse stores an envelope and lifts its binary frame into a field that
// survives JSON encoding — see the note on reverseControlRequest.
func (r *reverseControlResponse) setResponse(env proto.Envelope) {
	r.Response = &env
	r.ResponseBinary = env.Binary
}

// responseEnvelope returns the stored envelope with its binary frame reattached.
func (r reverseControlResponse) responseEnvelope() (proto.Envelope, bool) {
	if r.Response == nil {
		return proto.Envelope{}, false
	}
	env := *r.Response
	if r.ResponseBinary != nil {
		env.Binary = r.ResponseBinary
	}
	return env, true
}

func NewReverseHub(app *App, controlToken string) *ReverseHub {
	return &ReverseHub{
		app:            app,
		sessions:       make(map[string]*reverseSession),
		controlToken:   controlToken,
		activeByServer: make(map[string]int),
		controlSlots:   make(chan struct{}, maxConcurrentControlConnections),
		controlWaiters: make(chan struct{}, maxQueuedControlConnections),
		controlCalls:   make(chan struct{}, maxInFlightControlCalls),
	}
}

func (h *ReverseHub) Serve(ctx context.Context, listener net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
		h.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept reverse transport connection: %w", err)
		}
		// Bound concurrent pre-auth handshakes: drop (don't queue) excess
		// connections so half-open floods can't exhaust goroutines/fds.
		select {
		case reverseHandshakeSlots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		go func() {
			var once sync.Once
			releaseHandshake := func() { once.Do(func() { <-reverseHandshakeSlots }) }
			defer releaseHandshake()
			defer guardPanic("reverse agent connection")
			_ = h.serveConnAfterAuth(conn, releaseHandshake)
		}()
	}
}

// reverseHandshakeTimeout bounds how long a reverse agent's SSH handshake may
// take before the controller drops the connection.
const reverseHandshakeTimeout = 20 * time.Second

// maxConcurrentReverseHandshakes caps how many inbound reverse connections may be
// in the pre-auth SSH handshake at once. Without it an attacker who can reach the
// listener could open thousands of half-open connections, each pinning a
// goroutine + fd (and a private-key disk read) for up to reverseHandshakeTimeout.
const maxConcurrentReverseHandshakes = 256

// reverseHandshakeSlots is a counting semaphore buffered to the cap above.
var reverseHandshakeSlots = make(chan struct{}, maxConcurrentReverseHandshakes)

// reversePostAuthTimeout bounds the synchronous post-auth Hello so a peer that
// completes auth but never answers can't pin the connection goroutine
// indefinitely. (The queued-metrics replay now runs after registration, with a
// timeout per page: see metrics_replay.go.)
const reversePostAuthTimeout = 30 * time.Second

const (
	reverseFirstChannelTimeout     = 15 * time.Second
	maxActiveReverseConnections    = 256
	maxReverseConnectionsPerServer = 2
)

func (h *ReverseHub) acquireAuthenticated(server string) bool {
	h.activeMu.Lock()
	defer h.activeMu.Unlock()
	if server == "" || h.activeTotal >= maxActiveReverseConnections || h.activeByServer[server] >= maxReverseConnectionsPerServer {
		return false
	}
	h.activeTotal++
	h.activeByServer[server]++
	return true
}

func (h *ReverseHub) releaseAuthenticated(server string) {
	h.activeMu.Lock()
	defer h.activeMu.Unlock()
	if h.activeByServer[server] <= 0 {
		return
	}
	h.activeTotal--
	h.activeByServer[server]--
	if h.activeByServer[server] == 0 {
		delete(h.activeByServer, server)
	}
}

func (h *ReverseHub) ServeConn(rawConn net.Conn) error {
	return h.serveConnAfterAuth(rawConn, nil)
}

func (h *ReverseHub) serveConnAfterAuth(rawConn net.Conn, authenticated func()) error {
	defer rawConn.Close()

	signer, err := fleetcrypto.LoadPrivateKeySigner(h.app.controllerPrivateKeyPath(), nil)
	if err != nil {
		return err
	}

	config := &ssh.ServerConfig{
		Config: ssh.Config{
			Ciphers:      transport.SupportedCiphers(),
			KeyExchanges: transport.SupportedKEX(),
			MACs:         transport.SupportedMACs(),
		},
		PublicKeyCallback: h.authorizeAgent,
	}
	config.AddHostKey(signer)

	// Bound the SSH handshake so a stalled or half-open agent can't pin a
	// goroutine and fd open indefinitely (DoS). Cleared once the handshake
	// completes — the established session manages its own lifetime.
	_ = rawConn.SetDeadline(time.Now().Add(reverseHandshakeTimeout))
	conn, chans, reqs, err := ssh.NewServerConn(rawConn, config)
	if err != nil {
		return err
	}
	if authenticated != nil {
		authenticated()
	}
	serverName := conn.Permissions.Extensions["server"]
	if !h.acquireAuthenticated(serverName) {
		_ = conn.Close()
		return fmt.Errorf("too many authenticated reverse connections for %q", serverName)
	}
	defer h.releaseAuthenticated(serverName)
	_ = rawConn.SetDeadline(time.Now().Add(reverseFirstChannelTimeout))
	defer conn.Close()
	go ssh.DiscardRequests(reqs)

	registered := false
	for newChannel := range chans {
		if newChannel.ChannelType() != transport.RPCChannelType {
			_ = newChannel.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}
		if registered {
			_ = newChannel.Reject(ssh.ResourceShortage, "reverse session channel already registered")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		registered = true
		_ = rawConn.SetDeadline(time.Time{})
		go ssh.DiscardRequests(requests)

		// authorizeAgent stored the authenticated, colon-stripped server name in
		// permissions; never use the raw SSH username carrying enrollment data.
		session := &transport.Session{
			Mode:               transport.ModeReverse,
			LocalAddr:          conn.LocalAddr(),
			RemoteAddr:         conn.RemoteAddr(),
			HostKeyFingerprint: conn.Permissions.Extensions["fingerprint"],
			Channel:            channel,
			Closer:             conn,
		}

		helloCtx, cancelHello := context.WithTimeout(context.Background(), reversePostAuthTimeout)
		hello, err := session.Hello(helloCtx, h.app.Config.InstanceID)
		cancelHello()
		if err != nil {
			_ = session.Close()
			return err
		}

		info := ReverseSessionInfo{
			Server:             serverName,
			Connected:          true,
			ConnectedAt:        time.Now().UTC(),
			HostKeyFingerprint: conn.Permissions.Extensions["fingerprint"],
			Hello:              hello,
		}
		// Register first, replay second. Replaying a long offline backlog used
		// to run before the session existed, so for the whole replay the server
		// looked disconnected and every call to it failed.
		h.setSession(serverName, session, info, conn)

		// Probe the connection so an agent that disappears without closing it
		// (power loss, a NAT dropping state) is cleared within about a minute.
		// Closing the connection ends Wait below, which clears the session.
		stopKeepalive := transport.StartKeepalive(conn, reverseKeepaliveInterval, reverseKeepaliveMaxMissed, nil)
		name, sshConn, capabilities := serverName, conn, hello.Capabilities
		if !h.goTracked(func() {
			_ = sshConn.Wait()
			stopKeepalive()
			h.clearSession(name, "", session)
		}) {
			// The hub closed while this connection was being set up: drop it
			// here rather than leave a session nobody will clean up.
			stopKeepalive()
			h.clearSession(name, "hub closed", session)
			return nil
		}
		h.goTracked(func() { h.replayAfterConnect(name, session, capabilities) })
	}
	return nil
}

// replayAfterConnect drains the agent's offline metrics queue once the
// session is registered and records the outcome.
func (h *ReverseHub) replayAfterConnect(serverName string, session *transport.Session, capabilities []string) {
	defer guardPanic("reverse metrics replay")
	replayed, err := h.replayQueuedMetrics(serverName, session, capabilities)
	if replayed > 0 {
		h.noteReplayedMetrics(serverName, session, replayed)
	}
	if err != nil {
		_ = h.app.AuditLog.Append(logs.AuditEntry{
			Action:   "metrics.replay.failed",
			Target:   serverName,
			Operator: h.app.operator(),
			Details:  err.Error(),
		})
	}
}

// noteReplayedMetrics records on the live session how many queued snapshots
// its connection replayed, if that session is still the registered one.
func (h *ReverseHub) noteReplayedMetrics(serverName string, session *transport.Session, replayed int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if current := h.sessions[serverName]; current != nil && current.session == session {
		current.info.ReplayedMetrics = replayed
	}
}

// callOn issues one RPC for serverName over the given agent connection. While
// that connection is the registered session the call goes through CallContext,
// so it takes a channel lane like any other caller instead of monopolising the
// agent-opened channel; once the session has been replaced or cleared it fails
// rather than wandering onto a newer connection.
func (h *ReverseHub) callOn(ctx context.Context, serverName string, session *transport.Session, env proto.Envelope) (proto.Envelope, error) {
	h.mu.RLock()
	current := h.sessions[serverName]
	h.mu.RUnlock()
	if current != nil && current.session == session {
		return h.CallContext(ctx, serverName, env)
	}
	if current != nil || session == nil {
		return proto.Envelope{}, fmt.Errorf("reverse session for %q was replaced", serverName)
	}
	// Not registered at all (e.g. called directly by a test): use the channel.
	return session.Call(ctx, env)
}

func (h *ReverseHub) Call(server string, env proto.Envelope) (proto.Envelope, error) {
	return h.CallContext(context.Background(), server, env)
}

func (h *ReverseHub) CallContext(ctx context.Context, server string, env proto.Envelope) (proto.Envelope, error) {
	h.mu.RLock()
	current := h.sessions[server]
	h.mu.RUnlock()
	if current == nil || current.session == nil {
		return proto.Envelope{}, fmt.Errorf("reverse session for %q is not connected", server)
	}
	// Borrow a channel so concurrent callers do not queue behind one another.
	// Against an agent without CapabilityReverseMultiplex this returns the
	// single agent-opened channel and behaves exactly as it always did.
	releaseLane, err := current.acquireLane(ctx)
	if err != nil {
		return proto.Envelope{}, err
	}
	defer releaseLane()
	sess, pooled, err := current.leaseChannel(ctx)
	if err != nil {
		if errors.Is(err, transport.ErrChannelOpenTimeout) {
			// The connection could not answer a channel open: it is dead.
			h.clearSession(server, err.Error(), current.session)
		}
		return proto.Envelope{}, err
	}
	response, err := sess.Call(ctx, env)
	if err != nil {
		if transport.SessionUsableAfterError(err) {
			if pooled {
				current.releaseChannel(sess, true)
			}
			return proto.Envelope{}, err
		}
		if pooled {
			current.releaseChannel(sess, false)
			// The caller giving up only cost this extra channel (Call closed
			// it); the connection and its other channels are fine.
			if ctx.Err() != nil {
				return proto.Envelope{}, err
			}
		}
		// A framing or transport failure makes the whole connection suspect.
		h.clearSession(server, err.Error(), current.session)
		return proto.Envelope{}, err
	}
	if pooled {
		current.releaseChannel(sess, true)
	}
	return response, nil
}

func (h *ReverseHub) Status(server string) (ReverseSessionInfo, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	current := h.sessions[server]
	if current == nil {
		return ReverseSessionInfo{}, fmt.Errorf("reverse session for %q is not connected", server)
	}
	return current.info, nil
}

func (h *ReverseHub) Disconnect(server string) error {
	h.mu.RLock()
	current := h.sessions[server]
	h.mu.RUnlock()
	if current == nil || current.session == nil {
		return fmt.Errorf("reverse session for %q is not connected", server)
	}
	return current.session.Close()
}

// Close disconnects every agent and returns once the goroutines those
// sessions started have finished, so nothing writes to the App afterwards.
func (h *ReverseHub) Close() {
	h.bgMu.Lock()
	h.bgClosed = true
	h.bgMu.Unlock()

	h.mu.Lock()
	for name, session := range h.sessions {
		session.closeExtraChannels()
		if session.session != nil {
			_ = session.session.Close()
		}
		delete(h.sessions, name)
	}
	h.mu.Unlock()
	// Closing each connection ends its Wait, so this does not block on a
	// live agent; it waits only for writes already under way.
	h.bg.Wait()
}

// goTracked runs fn on its own goroutine and makes Close wait for it. Once
// Close has begun it runs nothing and reports false.
func (h *ReverseHub) goTracked(fn func()) bool {
	h.bgMu.Lock()
	defer h.bgMu.Unlock()
	if h.bgClosed {
		return false
	}
	h.bg.Add(1)
	go func() {
		defer h.bg.Done()
		fn()
	}()
	return true
}

func (h *ReverseHub) authorizeAgent(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	// The SSH username is "<serverName>" or, during first-time enrollment,
	// "<serverName>:<enroll-token>". Server names never contain a colon, so split
	// on the first one. The token rides the already-encrypted SSH auth phase.
	raw := conn.User()
	serverName, secret := raw, ""
	if i := strings.IndexByte(raw, ':'); i >= 0 {
		serverName, secret = raw[:i], raw[i+1:]
	}
	if serverName == "" {
		return nil, fmt.Errorf("reverse agent must provide a server name as the SSH username")
	}
	// The (attacker-controlled) SSH username becomes a filesystem path
	// (keys/agents/<name>.pub) and a server lookup, so reject path separators /
	// traversal up front — defense in depth beyond the GetServer lookup below.
	if err := validateSafeName(serverName); err != nil {
		return nil, fmt.Errorf("invalid reverse server name: %w", err)
	}
	server, err := h.app.GetServer(serverName)
	if err != nil {
		return nil, fmt.Errorf("reverse server %q is not registered: %w", serverName, err)
	}
	if server.Mode != transport.ModeReverse && server.Mode != transport.ModePerNode {
		return nil, fmt.Errorf("server %q is not configured for reverse mode", serverName)
	}

	path := filepath.Join(h.app.ConfigDir, "keys", "agents", serverName+".pub")

	// Serialize the whole read-pin → verify-token → write-pin → clear-token
	// sequence so two connections racing the SAME one-time enrollment token can't
	// both observe "no pin", both pass the token compare, and both write a key
	// (last-writer-wins). The lock makes token consumption atomic: exactly one
	// connection enrolls; the loser sees the freshly written pin (key match) or a
	// consumed token (mismatch). Re-read the server record under the lock so the
	// EnrollSecret seen here can't be stale relative to a concurrent enrollment.
	h.enrollMu.Lock()
	defer h.enrollMu.Unlock()

	if fresh, ferr := h.app.GetServer(serverName); ferr == nil {
		server = fresh
	}

	data, readErr := os.ReadFile(path) // #nosec G304 -- path is the controller-configured public key file
	switch {
	case readErr == nil:
		// Already enrolled: the presented key MUST match the pinned key.
		existing, _, _, _, parseErr := ssh.ParseAuthorizedKey(data)
		if parseErr != nil {
			return nil, fmt.Errorf("parse pinned reverse key for %s: %w", serverName, parseErr)
		}
		if string(existing.Marshal()) != string(key.Marshal()) {
			return nil, fmt.Errorf("reverse agent key mismatch for %s", serverName)
		}
	case os.IsNotExist(readErr):
		// First contact: require a valid one-time enrollment token BEFORE pinning,
		// so a rogue agent that merely knows the server name and can reach the
		// listener cannot win the key-pin race. These checks (and the directory
		// creation) run only after a name passes the cheap GetServer/mode gate,
		// so an unknown or ineligible name does no enrollment disk work.
		if server.EnrollSecret == "" {
			return nil, fmt.Errorf("server %q has no pending enrollment token — mint one with 'fleet server enroll-token %s' and start the agent with --enroll-token", serverName, serverName)
		}
		if subtle.ConstantTimeCompare([]byte(secret), []byte(server.EnrollSecret)) != 1 {
			return nil, fmt.Errorf("invalid enrollment token for %s", serverName)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create reverse agent key directory: %w", err)
		}
		if err := os.WriteFile(path, ssh.MarshalAuthorizedKey(key), 0o644); err != nil { // #nosec G306 -- public key is intentionally world-readable
			return nil, fmt.Errorf("pin reverse agent key for %s: %w", serverName, err)
		}
		// Consume the one-time token so a stolen token can't be replayed.
		server.EnrollSecret = ""
		if err := h.app.SaveServer(server); err != nil {
			return nil, fmt.Errorf("clear enrollment token for %s: %w", serverName, err)
		}
	default:
		return nil, fmt.Errorf("read pinned reverse key for %s: %w", serverName, readErr)
	}

	return &ssh.Permissions{
		Extensions: map[string]string{
			"server":      serverName,
			"fingerprint": ssh.FingerprintSHA256(key),
		},
	}, nil
}

// GenerateEnrollSecret mints a random reverse-mode enrollment token.
func GenerateEnrollSecret() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate enrollment secret: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func (h *ReverseHub) setSession(serverName string, session *transport.Session, info ReverseSessionInfo, conn ssh.Conn) {
	if session != nil {
		session.SetCapabilities(info.Hello.Capabilities)
	}
	h.mu.Lock()
	if existing := h.sessions[serverName]; existing != nil {
		existing.closeExtraChannels()
		if existing.session != nil {
			_ = existing.session.Close()
		}
	}
	h.sessions[serverName] = newReverseSession(session, conn, info)
	h.mu.Unlock()

	server, err := h.app.GetServer(serverName)
	if err != nil {
		return
	}
	server.Capabilities = info.Hello.Capabilities
	server.Observed = ServerObservation{
		Reachable:          true,
		LastSeen:           time.Now().UTC(),
		LastError:          "",
		NodeName:           info.Hello.NodeName,
		AgentVersion:       info.Hello.AgentVersion,
		OS:                 info.Hello.OS,
		Arch:               info.Hello.Arch,
		Transport:          info.Hello.Transport,
		FileRoot:           info.Hello.FileRoot,
		HostKeyFingerprint: info.HostKeyFingerprint,
	}
	_ = h.app.SaveServer(server)
	_ = h.app.AuditLog.Append(logs.AuditEntry{
		Action:   "server.reverse.connect",
		Target:   serverName,
		Operator: h.app.operator(),
		Details:  fmt.Sprintf("fingerprint=%s capabilities=%d", info.HostKeyFingerprint, len(info.Hello.Capabilities)),
	})

	// Auto-update the agent only when the policy permits it.
	if info.Hello.AgentVersion != "" && version.Canonical(info.Hello.AgentVersion) != version.Canonical(version.Version) &&
		agentSupportsUnattendedUpdateActivation(info.Hello.OS) &&
		h.app.Config.Updates.Policy == update.PolicyAutoUpdate {
		go func() {
			defer guardPanic("agent auto-update")
			_ = h.app.AuditLog.Append(logs.AuditEntry{
				Action:   "agent.auto-update",
				Target:   serverName,
				Operator: "system",
				Details:  fmt.Sprintf("agent=%s controller=%s", info.Hello.AgentVersion, version.Version),
			})
			h.app.applyAgentUpdate(context.Background(), server)
		}()
	}
}

func (h *ReverseHub) clearSession(serverName, lastError string, expected *transport.Session) {
	h.mu.Lock()
	current := h.sessions[serverName]
	if current == nil || current.session != expected {
		h.mu.Unlock()
		return
	}
	delete(h.sessions, serverName)
	h.mu.Unlock()
	current.closeExtraChannels()
	if current.session != nil {
		_ = current.session.Close()
	}

	server, err := h.app.GetServer(serverName)
	if err != nil {
		return
	}
	server.Observed.Reachable = false
	server.Observed.LastSeen = time.Now().UTC()
	server.Observed.LastError = lastError
	_ = h.app.SaveServer(server)
}

func (a *App) reverseStatus(serverName string) (ReverseSessionInfo, error) {
	if a.ReverseStatusLookup != nil {
		return a.ReverseStatusLookup(serverName)
	}
	return a.callReverseStatus(serverName)
}

func (a *App) reverseDisconnect(serverName string) error {
	if a.ReverseDisconnect != nil {
		return a.ReverseDisconnect(serverName)
	}
	return a.callReverseDisconnect(serverName)
}

func (a *App) generateControlToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate control token: %w", err)
	}
	// The prefix tells callers that this daemon performs mutual
	// authentication, so they never fall back to presenting the token in the
	// clear to whatever answers on the control address. Callers from before
	// that treat the whole string as an opaque token, as they always have.
	token := controlTokenMutualAuthPrefix + hex.EncodeToString(raw)
	tokenPath := a.controlTokenPath()
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o750); err != nil {
		return "", fmt.Errorf("create data dir for control token: %w", err)
	}
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("write control token: %w", err)
	}
	return token, nil
}

func validateLoopbackControlAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("parse control address %q: %w", address, err)
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("reverse control address %q must bind to a loopback IP", address)
	}
	return nil
}

func requireLoopbackListener(listener net.Listener) error {
	tcp, ok := listener.Addr().(*net.TCPAddr)
	if !ok || tcp.IP == nil || !tcp.IP.IsLoopback() {
		return fmt.Errorf("reverse control listener must be loopback-only (got %s)", listener.Addr())
	}
	return nil
}

func (a *App) RunDaemon(ctx context.Context) error {
	if err := validateLoopbackControlAddress(a.Config.Runtime.ControlAddress); err != nil {
		return err
	}
	reverseListener, err := net.Listen("tcp", a.Config.Runtime.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen for reverse agents on %s: %w", a.Config.Runtime.ListenAddress, err)
	}
	defer reverseListener.Close()

	controlListener, err := net.Listen("tcp", a.Config.Runtime.ControlAddress)
	if err != nil {
		return fmt.Errorf("listen for local control on %s: %w", a.Config.Runtime.ControlAddress, err)
	}
	defer controlListener.Close()

	// Shut down cleanly on SIGTERM (service managers) as well as the
	// caller's own cancellation, so the token file below is removed.
	ctx, stopSignals := signal.NotifyContext(ctx, syscall.SIGTERM)
	defer stopSignals()

	controlToken, err := a.generateControlToken()
	if err != nil {
		return err
	}
	// Remove the token when the daemon stops. A token left behind is what a
	// process later listening on the control address would be handed by
	// callers that still speak the legacy protocol.
	defer a.removeControlTokenIfOwned(controlToken)

	hub := NewReverseHub(a, controlToken)
	a.useHubInProcess(hub)
	daemonApps.Store(a, struct{}{})
	defer daemonApps.Delete(a)
	errCh := make(chan error, 2)
	go func() { errCh <- hub.Serve(ctx, reverseListener) }()
	go func() { errCh <- hub.ServeControl(ctx, controlListener) }()
	go a.runMetricsPoller(ctx)
	go a.runUpdateChecker(ctx)
	go a.runJobLogPruner(ctx)

	select {
	case <-ctx.Done():
		hub.Close()
		return nil
	case err := <-errCh:
		hub.Close()
		return err
	}
}

// useHubInProcess points the daemon's own reverse-mode calls straight at its
// hub. Without this the daemon's metrics poller (and anything else it runs)
// reached its reverse agents the way a separate CLI process does: a TCP
// connection to its own control socket per call, a re-read of control.token
// from disk, JSON (and base64 for any attachment) both ways, and one of the
// socket's limited handler slots — which it then competed for with real CLI and
// web UI callers. Hooks a caller has already installed (tests) are kept.
// Call before starting anything that issues RPCs.
func (a *App) useHubInProcess(hub *ReverseHub) {
	if a.ReverseRPCContext == nil && a.ReverseRPC == nil {
		a.ReverseRPCContext = hub.CallContext
	}
	if a.ReverseStatusLookup == nil {
		a.ReverseStatusLookup = hub.Status
	}
	if a.ReverseDisconnect == nil {
		a.ReverseDisconnect = hub.Disconnect
	}
}

func (a *App) controllerPrivateKeyPath() string {
	return filepath.Join(a.ConfigDir, "keys", a.Config.Crypto.PrimaryKey)
}

// ControllerHostKeyFingerprint returns the fingerprint agents must verify before
// creating their first controller pin.
func (a *App) ControllerHostKeyFingerprint() (string, error) {
	signer, err := fleetcrypto.LoadPrivateKeySigner(a.controllerPrivateKeyPath(), nil)
	if err != nil {
		return "", err
	}
	return ssh.FingerprintSHA256(signer.PublicKey()), nil
}
