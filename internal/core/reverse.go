// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

type reverseSession struct {
	session *transport.Session // the channel the agent opened; always usable
	conn    ssh.Conn           // opens additional channels back to the agent
	info    ReverseSessionInfo

	// multiplex is set when the agent advertised CapabilityReverseMultiplex.
	// Without it the controller never attempts an extra channel and every call
	// serialises on the agent-opened one, exactly as before.
	multiplex bool

	mu     sync.Mutex
	idle   []*transport.Session // extra channels free for reuse
	opened int                  // extra channels created so far
}

// leaseChannel borrows a channel for one RPC. It prefers an idle extra channel,
// then opens a new one, and finally falls back to the agent-opened channel. The
// bool reports whether the caller should return it via releaseChannel; the
// primary channel is shared and must not be pooled.
func (rs *reverseSession) leaseChannel() (*transport.Session, bool) {
	if !rs.multiplex || rs.conn == nil {
		return rs.session, false
	}
	rs.mu.Lock()
	if n := len(rs.idle); n > 0 {
		sess := rs.idle[n-1]
		rs.idle = rs.idle[:n-1]
		rs.mu.Unlock()
		return sess, true
	}
	if rs.opened >= maxReverseExtraChannels {
		rs.mu.Unlock()
		return rs.session, false // at the cap: share the primary channel
	}
	rs.opened++
	rs.mu.Unlock()

	channel, requests, err := rs.conn.OpenChannel(transport.RPCChannelType, nil)
	if err != nil {
		// An agent that rejects inbound channels (or a connection on its way
		// out) drops us back to the single-channel path rather than failing.
		rs.mu.Lock()
		if rs.opened > 0 {
			rs.opened--
		}
		rs.mu.Unlock()
		return rs.session, false
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
	return sess, true
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
type reverseControlRequest struct {
	Token          string         `json:"token"`
	Type           string         `json:"type"`
	Server         string         `json:"server"`
	Envelope       proto.Envelope `json:"envelope,omitempty"`
	EnvelopeBinary []byte         `json:"envelope_binary,omitempty"`
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
	Status         *ReverseSessionInfo `json:"status,omitempty"`
	Error          *proto.Error        `json:"error,omitempty"`
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

// reversePostAuthTimeout bounds the synchronous post-auth RPCs (Hello + queued
// metric replay) so a peer that completes auth but never answers can't pin the
// connection goroutine indefinitely.
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
		replayed, replayErr := h.replayQueuedMetrics(serverName, session, hello.Capabilities)
		if replayErr != nil {
			_ = h.app.AuditLog.Append(logs.AuditEntry{
				Action:   "metrics.replay.failed",
				Target:   serverName,
				Operator: h.app.operator(),
				Details:  replayErr.Error(),
			})
		} else {
			info.ReplayedMetrics = replayed
		}
		h.setSession(serverName, session, info, conn)

		go func(name string, session *transport.Session, sshConn *ssh.ServerConn) {
			_ = sshConn.Wait()
			h.clearSession(name, "", session)
		}(serverName, session, conn)
	}
	return nil
}

func (h *ReverseHub) replayQueuedMetrics(serverName string, session *transport.Session, capabilities []string) (int, error) {
	// Older agents implemented metrics.flush_queue destructively. Never probe it:
	// absence of the explicit peek/ack capability means queued data stays on the
	// agent until it is upgraded.
	if !slices.Contains(capabilities, proto.CapabilityMetricsPeekAck) {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), reversePostAuthTimeout)
	defer cancel()
	response, err := session.Call(ctx, proto.Envelope{
		Action: proto.ActionMetricsPeekQueue,
		Payload: proto.MetricsPayload{
			Server: serverName,
		},
	})
	if err != nil {
		return 0, err
	}
	if response.Error != nil {
		return 0, fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}

	replay, err := proto.DecodePayload[proto.MetricsReplayResult](response.Payload)
	if err != nil {
		return 0, err
	}
	if len(replay.Snapshots) == 0 {
		return 0, nil
	}
	if replay.BatchID == "" {
		return 0, fmt.Errorf("peek/ack capable agent returned metrics without a batch id")
	}
	acknowledge := func() error {
		ackResponse, err := session.Call(ctx, proto.Envelope{
			Action:  proto.ActionMetricsAckQueue,
			Payload: proto.MetricsReplayAck{BatchID: replay.BatchID},
		})
		if err != nil {
			return err
		}
		if ackResponse.Error != nil {
			return fmt.Errorf("%s: %s", ackResponse.Error.Code, ackResponse.Error.Message)
		}
		return nil
	}

	server, err := h.app.GetServer(serverName)
	if err != nil {
		return 0, err
	}

	latest := server.Metrics
	for _, snapshot := range replay.Snapshots {
		if err := h.app.persistMetricsSnapshot(serverName, snapshot); err != nil {
			return 0, err
		}
		if latest.Timestamp.IsZero() || snapshot.Timestamp.After(latest.Timestamp) {
			latest = snapshot
		}
	}
	server.Metrics = latest
	server.Observed.LastSeen = time.Now().UTC()
	server.Observed.LastError = ""
	if err := h.app.SaveServer(server); err != nil {
		return 0, err
	}
	if err := h.app.evaluateMetricAlerts(serverName, latest); err != nil {
		return 0, err
	}
	if err := h.app.clearCollectionFailureAlert(serverName); err != nil {
		return 0, err
	}
	if err := h.app.AuditLog.Append(logs.AuditEntry{
		Action:   "metrics.replay",
		Target:   serverName,
		Operator: h.app.operator(),
		Details:  fmt.Sprintf("snapshots=%d", len(replay.Snapshots)),
	}); err != nil {
		return 0, err
	}
	if err := acknowledge(); err != nil {
		return 0, fmt.Errorf("acknowledge replayed metrics: %w", err)
	}
	return len(replay.Snapshots), nil
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
	sess, pooled := current.leaseChannel()
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

func (h *ReverseHub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, session := range h.sessions {
		session.closeExtraChannels()
		if session.session != nil {
			_ = session.session.Close()
		}
		delete(h.sessions, name)
	}
}

const (
	maxControlRequestBytes          = 32 << 20
	controlIOTimeout                = 30 * time.Second
	maxConcurrentControlConnections = 32
)

func (h *ReverseHub) ServeControl(ctx context.Context, listener net.Listener) error {
	if err := requireLoopbackListener(listener); err != nil {
		_ = listener.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept control connection: %w", err)
		}
		select {
		case h.controlSlots <- struct{}{}:
			go func() {
				defer func() { <-h.controlSlots }()
				h.handleControlConn(conn)
			}()
		default:
			_ = conn.Close()
		}
	}
}

func (h *ReverseHub) handleControlConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(controlIOTimeout))

	limited := &io.LimitedReader{R: conn, N: maxControlRequestBytes + 1}
	decoder := json.NewDecoder(limited)
	var req reverseControlRequest
	if err := decoder.Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(reverseControlResponse{
			Error: &proto.Error{Code: "decode_error", Message: "invalid control request"},
		})
		return
	}
	buffered, _ := io.ReadAll(decoder.Buffered())
	if limited.N <= 0 || len(strings.TrimSpace(string(buffered))) != 0 {
		_ = json.NewEncoder(conn).Encode(reverseControlResponse{
			Error: &proto.Error{Code: "request_too_large", Message: "control request exceeds limit or contains trailing data"},
		})
		return
	}

	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(h.controlToken)) != 1 {
		_ = json.NewEncoder(conn).Encode(reverseControlResponse{
			Error: &proto.Error{Code: "unauthorized", Message: "invalid control token"},
		})
		return
	}

	var resp reverseControlResponse
	switch req.Type {
	case "call":
		callCtx, cancel := context.WithTimeout(context.Background(), controlIOTimeout)
		if deadline := req.Envelope.DeadlineUnixMilli; deadline > 0 {
			requested := time.UnixMilli(deadline)
			if current, ok := callCtx.Deadline(); !ok || requested.Before(current) {
				cancel()
				callCtx, cancel = context.WithDeadline(context.Background(), requested)
			}
		}
		// The CLI closes this loopback connection when its context is cancelled.
		// Continue reading after the one complete request solely to observe that
		// close and cancel the daemon-side call; otherwise a deadline-free Ctrl-C
		// would leave the remote process running until controlIOTimeout.
		go func() {
			_, _ = io.Copy(io.Discard, conn)
			cancel()
		}()
		out, err := h.CallContext(callCtx, req.Server, req.envelope())
		cancel()
		if err != nil {
			resp.Error = &proto.Error{Code: "reverse_call_failed", Message: err.Error()}
			break
		}
		resp.setResponse(out)
	case "status":
		info, err := h.Status(req.Server)
		if err != nil {
			resp.Error = &proto.Error{Code: "reverse_status_failed", Message: err.Error()}
			break
		}
		resp.Status = &info
	case "disconnect":
		if err := h.Disconnect(req.Server); err != nil {
			resp.Error = &proto.Error{Code: "reverse_disconnect_failed", Message: err.Error()}
			break
		}
	default:
		resp.Error = &proto.Error{Code: "unsupported_action", Message: fmt.Sprintf("control request %q is not supported", req.Type)}
	}

	_ = json.NewEncoder(conn).Encode(resp)
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
	h.sessions[serverName] = &reverseSession{
		session: session,
		conn:    conn,
		info:    info,
		multiplex: slices.Contains(info.Hello.Capabilities, proto.CapabilityReverseMultiplex) &&
			conn != nil,
	}
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
		h.app.Config.Updates.Policy == update.PolicyAutoUpdate {
		go func() {
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

func (a *App) callReverseControlContext(ctx context.Context, serverName string, env proto.Envelope) (proto.Envelope, error) {
	if err := validateLoopbackControlAddress(a.Config.Runtime.ControlAddress); err != nil {
		return proto.Envelope{}, err
	}
	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", a.Config.Runtime.ControlAddress)
	if err != nil {
		return proto.Envelope{}, fmt.Errorf("connect to local reverse control at %s: %w", a.Config.Runtime.ControlAddress, err)
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	deadline := time.Now().Add(controlIOTimeout)
	if requested, ok := ctx.Deadline(); ok && requested.Before(deadline) {
		deadline = requested
		env.DeadlineUnixMilli = requested.UnixMilli()
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return proto.Envelope{}, fmt.Errorf("set reverse control deadline: %w", err)
	}

	token, err := a.readControlToken()
	if err != nil {
		return proto.Envelope{}, err
	}
	if err := json.NewEncoder(conn).Encode(
		newReverseControlRequest(token, "call", serverName, env),
	); err != nil {
		if ctx.Err() != nil {
			return proto.Envelope{}, ctx.Err()
		}
		return proto.Envelope{}, err
	}

	var resp reverseControlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		if ctx.Err() != nil {
			return proto.Envelope{}, ctx.Err()
		}
		return proto.Envelope{}, err
	}
	if resp.Error != nil {
		return proto.Envelope{}, fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	out, ok := resp.responseEnvelope()
	if !ok {
		return proto.Envelope{}, fmt.Errorf("reverse control did not return a response envelope")
	}
	return out, nil
}

func (a *App) callReverseStatus(serverName string) (ReverseSessionInfo, error) {
	if err := validateLoopbackControlAddress(a.Config.Runtime.ControlAddress); err != nil {
		return ReverseSessionInfo{}, err
	}
	conn, err := net.DialTimeout("tcp", a.Config.Runtime.ControlAddress, 2*time.Second)
	if err != nil {
		return ReverseSessionInfo{}, fmt.Errorf("connect to local reverse control at %s: %w", a.Config.Runtime.ControlAddress, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(controlIOTimeout)); err != nil {
		return ReverseSessionInfo{}, err
	}

	token, err := a.readControlToken()
	if err != nil {
		return ReverseSessionInfo{}, err
	}
	if err := json.NewEncoder(conn).Encode(reverseControlRequest{
		Token:  token,
		Type:   "status",
		Server: serverName,
	}); err != nil {
		return ReverseSessionInfo{}, err
	}

	var resp reverseControlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return ReverseSessionInfo{}, err
	}
	if resp.Error != nil {
		return ReverseSessionInfo{}, fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	if resp.Status == nil {
		return ReverseSessionInfo{}, fmt.Errorf("reverse control did not return session status")
	}
	return *resp.Status, nil
}

func (a *App) callReverseDisconnect(serverName string) error {
	if err := validateLoopbackControlAddress(a.Config.Runtime.ControlAddress); err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", a.Config.Runtime.ControlAddress, 2*time.Second)
	if err != nil {
		return fmt.Errorf("connect to local reverse control at %s: %w", a.Config.Runtime.ControlAddress, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(controlIOTimeout)); err != nil {
		return err
	}

	token, err := a.readControlToken()
	if err != nil {
		return err
	}
	if err := json.NewEncoder(conn).Encode(reverseControlRequest{
		Token:  token,
		Type:   "disconnect",
		Server: serverName,
	}); err != nil {
		return err
	}

	var resp reverseControlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	return nil
}

func (a *App) controlTokenPath() string {
	return filepath.Join(a.ConfigDir, "data", "control.token")
}

func (a *App) generateControlToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate control token: %w", err)
	}
	token := hex.EncodeToString(raw)
	tokenPath := a.controlTokenPath()
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o750); err != nil {
		return "", fmt.Errorf("create data dir for control token: %w", err)
	}
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("write control token: %w", err)
	}
	return token, nil
}

func (a *App) readControlToken() (string, error) {
	data, err := os.ReadFile(a.controlTokenPath())
	if err != nil {
		return "", fmt.Errorf("read control token: %w", err)
	}
	return string(data), nil
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

	controlToken, err := a.generateControlToken()
	if err != nil {
		return err
	}

	hub := NewReverseHub(a, controlToken)
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
