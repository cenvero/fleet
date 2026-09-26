// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

// The daemon's control socket is how every other process on this machine — a
// CLI invocation, the web UI, the TUI — reaches the reverse agents connected
// to the daemon. It listens on loopback only and every request carries the
// daemon's control token (data/control.token, 0600).
//
// Protocol: one request per connection. The caller writes a JSON
// reverseControlRequest and a newline; the daemon writes a JSON
// reverseControlResponse and a newline, then closes. The caller closing its end
// early cancels the call.
//
// Binary framing (negotiated): a daemon that lists controlCapBinaryFrame in its
// "hello" reply accepts request type "call.framed", where BinaryLength says the
// envelope's attachment follows the request's newline as exactly that many raw
// bytes. Independently, a caller that lists controlCapBinaryFrame in Accept
// gets a response attachment the same way (BinaryLength + raw bytes after the
// response's newline) instead of base64 in ResponseBinary. A daemon that does
// not know about framing ignores Accept, and rejects "call.framed" before it
// does anything, so a newer CLI falls back to base64 against an older daemon
// and an older CLI (which never asks) keeps getting base64 from a newer one.

const (
	maxControlRequestBytes = 32 << 20
	controlIOTimeout       = 30 * time.Second
	// maxConcurrentControlConnections bounds how many control requests the
	// daemon reads and serves at once. Pre-authentication work per connection
	// is bounded by maxControlRequestBytes and controlIOTimeout, so this is what
	// bounds a local, token-less process's memory impact.
	maxConcurrentControlConnections = 32
	// maxQueuedControlConnections bounds how many further connections may wait
	// for a handler slot. A waiting connection has not been read yet, so it
	// costs a goroutine and a file descriptor, nothing more.
	maxQueuedControlConnections = 256
	// maxInFlightControlCalls bounds authenticated calls in progress. A handler
	// slot only covers reading and authenticating a request; once that is done
	// the slot is handed back and the call runs under this separate, larger
	// bound. Previously a slot was held for the whole call, so 32 slow calls (a
	// batch of `exec -- sleep`) starved every other control request, even a
	// status lookup, and later callers were dropped.
	maxInFlightControlCalls = 128
)

// controlSlotWait is how long a queued connection waits for a handler slot
// before the daemon answers "busy". Excess connections used to be closed on the
// spot, which reached the caller as a bare EOF — indistinguishable from the
// server being offline. A variable so tests can shorten it.
var controlSlotWait = 15 * time.Second

// Control-protocol capabilities a daemon advertises in reply to "hello".
const (
	controlCapBinaryFrame = "binary-frame"
)

// Control request types.
const (
	controlTypeCall       = "call"
	controlTypeCallFramed = "call.framed"
	controlTypeStatus     = "status"
	controlTypeDisconnect = "disconnect"
	controlTypeHello      = "hello"
)

// controlCapabilities lists what this daemon's control socket understands.
func (h *ReverseHub) controlCapabilities() []string {
	return []string{controlCapBinaryFrame}
}

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
			go h.serveControlSlot(conn)
			continue
		default:
		}
		// Every handler is busy: queue the connection (up to a bound) until a
		// slot frees instead of dropping it.
		select {
		case h.controlWaiters <- struct{}{}:
			go h.waitForControlSlot(ctx, conn)
		default:
			_ = conn.Close()
		}
	}
}

// serveControlSlot handles one connection that holds a handler slot. The slot
// is released as soon as the request has been read and authenticated.
func (h *ReverseHub) serveControlSlot(conn net.Conn) {
	var once sync.Once
	release := func() { once.Do(func() { <-h.controlSlots }) }
	defer release()
	defer guardPanic("control connection")
	h.handleControl(conn, release)
}

// waitForControlSlot parks a connection until a handler slot frees, for at
// most controlSlotWait.
func (h *ReverseHub) waitForControlSlot(ctx context.Context, conn net.Conn) {
	timer := time.NewTimer(controlSlotWait)
	defer timer.Stop()
	select {
	case h.controlSlots <- struct{}{}:
		<-h.controlWaiters
		h.serveControlSlot(conn)
	case <-timer.C:
		<-h.controlWaiters
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		_ = writeControlResponse(conn, reverseControlResponse{Error: &proto.Error{
			Code: "control_busy", Message: "fleet daemon control socket is busy; try again", Retry: true,
		}}, nil)
		_ = conn.Close()
	case <-ctx.Done():
		<-h.controlWaiters
		_ = conn.Close()
	}
}

func (h *ReverseHub) handleControlConn(conn net.Conn) {
	h.handleControl(conn, nil)
}

// acquireCallSlot bounds authenticated calls in progress. With wait set a
// caller waits up to controlSlotWait for a slot; otherwise it is refused at
// once.
func (h *ReverseHub) acquireCallSlot(wait bool) (release func(), ok bool) {
	if h.controlCalls == nil {
		return func() {}, true
	}
	select {
	case h.controlCalls <- struct{}{}:
		return func() { <-h.controlCalls }, true
	default:
	}
	if !wait {
		return nil, false
	}
	timer := time.NewTimer(controlSlotWait)
	defer timer.Stop()
	select {
	case h.controlCalls <- struct{}{}:
		return func() { <-h.controlCalls }, true
	case <-timer.C:
		return nil, false
	}
}

// handleControl serves one control connection. releaseReadSlot, when set, is
// called once the request has been read and authenticated.
func (h *ReverseHub) handleControl(conn net.Conn, releaseReadSlot func()) {
	defer conn.Close()
	if releaseReadSlot == nil {
		releaseReadSlot = func() {}
	}
	_ = conn.SetDeadline(time.Now().Add(controlIOTimeout))

	limited := &io.LimitedReader{R: conn, N: maxControlRequestBytes + 1}
	decoder := json.NewDecoder(limited)
	var req reverseControlRequest
	if err := decoder.Decode(&req); err != nil {
		_ = writeControlError(conn, "decode_error", "invalid control request")
		return
	}
	buffered := decoder.Buffered()
	if req.BinaryLength == nil {
		trailing, _ := io.ReadAll(buffered)
		if limited.N <= 0 || len(strings.TrimSpace(string(trailing))) != 0 {
			_ = writeControlError(conn, "request_too_large", "control request exceeds limit or contains trailing data")
			return
		}
	} else if limited.N <= 0 {
		_ = writeControlError(conn, "request_too_large", "control request exceeds limit or contains trailing data")
		return
	}

	// Authenticate before reading any attachment, so a caller without the
	// token cannot make the daemon buffer one.
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(h.controlToken)) != 1 {
		_ = writeControlError(conn, "unauthorized", "invalid control token")
		return
	}

	var attachment []byte
	if req.BinaryLength != nil {
		if req.Type != controlTypeCallFramed {
			_ = writeControlError(conn, "decode_error", fmt.Sprintf("control request %q cannot carry an attachment", req.Type))
			return
		}
		blob, trailing, err := readControlAttachment(buffered, limited, *req.BinaryLength)
		if err != nil || limited.N <= 0 || len(strings.TrimSpace(string(trailing))) != 0 {
			_ = writeControlError(conn, "request_too_large", "control request attachment is malformed, exceeds limit or has trailing data")
			return
		}
		attachment = blob
	}
	// The request is fully read and authenticated: hand the read slot back so
	// a slow call does not hold up other callers' requests.
	releaseReadSlot()
	framedResponse := slices.Contains(req.Accept, controlCapBinaryFrame)

	var resp reverseControlResponse
	var respAttachment []byte
	switch req.Type {
	case controlTypeCall, controlTypeCallFramed:
		releaseCall, ok := h.acquireCallSlot(true)
		if !ok {
			resp.Error = &proto.Error{Code: "control_busy", Message: "fleet daemon has too many calls in progress; try again", Retry: true}
			break
		}
		env := req.envelope()
		if attachment != nil {
			env.Binary = attachment
		}
		out, err := runControlCall(conn, req.Envelope.DeadlineUnixMilli, true, func(ctx context.Context) (proto.Envelope, error) {
			return h.CallContext(ctx, req.Server, env)
		})
		releaseCall()
		if err != nil {
			resp.Error = &proto.Error{Code: "reverse_call_failed", Message: err.Error()}
			break
		}
		respAttachment = resp.setResponseFramed(out, framedResponse)
	case controlTypeStatus:
		info, err := h.Status(req.Server)
		if err != nil {
			resp.Error = &proto.Error{Code: "reverse_status_failed", Message: err.Error()}
			break
		}
		resp.Status = &info
	case controlTypeDisconnect:
		if err := h.Disconnect(req.Server); err != nil {
			resp.Error = &proto.Error{Code: "reverse_disconnect_failed", Message: err.Error()}
			break
		}
	case controlTypeHello:
		resp.Capabilities = h.controlCapabilities()
	default:
		resp.Error = &proto.Error{Code: "unsupported_action", Message: fmt.Sprintf("control request %q is not supported", req.Type)}
	}

	_ = writeControlResponse(conn, resp, respAttachment)
}

// runControlCall runs fn under the caller's deadline — capped at
// controlIOTimeout when capped is set — and cancels it if the caller hangs up.
func runControlCall(conn net.Conn, deadlineUnixMilli int64, capped bool, fn func(context.Context) (proto.Envelope, error)) (proto.Envelope, error) {
	callCtx, cancel := context.WithCancel(context.Background())
	if capped {
		cancel()
		callCtx, cancel = context.WithTimeout(context.Background(), controlIOTimeout)
	}
	if deadlineUnixMilli > 0 {
		requested := time.UnixMilli(deadlineUnixMilli)
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
	out, err := fn(callCtx)
	cancel()
	return out, err
}

// setResponseFramed stores env, carrying its attachment raw after the JSON
// when the caller accepts that (returning the bytes to send) or as base64 in
// ResponseBinary otherwise.
func (r *reverseControlResponse) setResponseFramed(env proto.Envelope, framed bool) []byte {
	if !framed || env.Binary == nil {
		r.setResponse(env)
		return nil
	}
	blob := env.Binary
	env.Binary = nil
	r.Response = &env
	n := len(blob)
	r.BinaryLength = &n
	return blob
}

func writeControlError(conn net.Conn, code, message string) error {
	return writeControlResponse(conn, reverseControlResponse{Error: &proto.Error{Code: code, Message: message}}, nil)
}

// writeControlResponse writes the JSON line and, when present, the raw
// attachment after it — in a single writev where the platform allows.
func writeControlResponse(conn net.Conn, resp reverseControlResponse, attachment []byte) error {
	return writeControlMessage(conn, resp, attachment)
}

func writeControlMessage(w io.Writer, message any, attachment []byte) error {
	header, err := json.Marshal(message)
	if err != nil {
		return err
	}
	header = append(header, '\n')
	if attachment == nil {
		_, err = w.Write(header)
		return err
	}
	buffers := net.Buffers{header, attachment}
	_, err = buffers.WriteTo(w)
	return err
}

// readControlAttachment reads the newline that ends a JSON line and the n raw
// attachment bytes after it. buffered is what the JSON decoder read ahead;
// rest is the connection. It also returns whatever the decoder had buffered
// beyond the attachment, so the caller can reject trailing data.
func readControlAttachment(buffered, rest io.Reader, n int) ([]byte, []byte, error) {
	if n < 0 || n > proto.MaxBinaryFrameBytes {
		return nil, nil, fmt.Errorf("control attachment length %d is out of range", n)
	}
	r := io.MultiReader(buffered, rest)
	var sep [1]byte
	if _, err := io.ReadFull(r, sep[:]); err != nil {
		return nil, nil, fmt.Errorf("read control attachment separator: %w", err)
	}
	if sep[0] != '\n' {
		return nil, nil, fmt.Errorf("control attachment is not preceded by a newline")
	}
	blob := make([]byte, n)
	if _, err := io.ReadFull(r, blob); err != nil {
		return nil, nil, fmt.Errorf("read control attachment: %w", err)
	}
	trailing, _ := io.ReadAll(buffered)
	return blob, trailing, nil
}

// --- caller side -------------------------------------------------------------

// controlPeer caches what this process knows about the daemon behind one
// control address: its control token and its control-protocol capabilities.
// Both used to be rediscovered on every call — each RPC re-read the token file
// from disk — and the capabilities are what let a newer CLI use binary framing
// without breaking against an older daemon.
type controlPeer struct {
	mu        sync.Mutex
	tokenPath string
	token     string
	haveToken bool
	caps      []string
	haveCaps  bool
}

var controlPeers sync.Map // control address + "\x00" + token path → *controlPeer

func (a *App) controlPeer() *controlPeer {
	tokenPath := a.controlTokenPath()
	key := a.Config.Runtime.ControlAddress + "\x00" + tokenPath
	if peer, ok := controlPeers.Load(key); ok {
		return peer.(*controlPeer)
	}
	peer, _ := controlPeers.LoadOrStore(key, &controlPeer{tokenPath: tokenPath})
	return peer.(*controlPeer)
}

// cachedToken returns the control token, reading it from disk only once.
func (p *controlPeer) cachedToken() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.haveToken {
		return p.token, nil
	}
	token, err := readControlTokenFile(p.tokenPath)
	if err != nil {
		return "", err
	}
	p.token, p.haveToken = token, true
	return token, nil
}

// refreshToken re-reads the token after the daemon rejected the cached one
// (it mints a new token every time it starts) and reports whether it changed,
// i.e. whether retrying can help.
func (p *controlPeer) refreshToken(rejected string) bool {
	token, err := readControlTokenFile(p.tokenPath)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.haveToken = false
		return false
	}
	p.token, p.haveToken = token, true
	return token != rejected
}

func (p *controlPeer) capabilities() ([]string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.caps, p.haveCaps
}

func (p *controlPeer) setCapabilities(caps []string) {
	p.mu.Lock()
	p.caps, p.haveCaps = caps, true
	p.mu.Unlock()
}

// forgetCapabilities drops what was learned about the daemon, e.g. because it
// could not be reached (it may come back as a different version).
func (p *controlPeer) forgetCapabilities() {
	p.mu.Lock()
	p.caps, p.haveCaps = nil, false
	p.mu.Unlock()
}

// controlCapabilitiesFor asks the daemon what it supports, once per process.
// An older daemon answers "hello" with unsupported_action, which is recorded
// as "no optional capabilities".
func (a *App) controlCapabilitiesFor(ctx context.Context, peer *controlPeer) []string {
	if caps, ok := peer.capabilities(); ok {
		return caps
	}
	token, err := peer.cachedToken()
	if err != nil {
		return nil
	}
	resp, _, err := a.controlRoundTrip(ctx, peer, reverseControlRequest{Token: token, Type: controlTypeHello}, nil)
	if err != nil {
		return nil
	}
	if resp.Error != nil {
		if resp.Error.Code == "unauthorized" || resp.Error.Code == "control_busy" {
			return nil // says nothing about the daemon's version
		}
		peer.setCapabilities([]string{})
		return nil
	}
	caps := resp.Capabilities
	if caps == nil {
		caps = []string{}
	}
	peer.setCapabilities(caps)
	return caps
}

// controlRoundTrip performs one request/response exchange on a fresh
// connection to the daemon's control socket.
func (a *App) controlRoundTrip(ctx context.Context, peer *controlPeer, req reverseControlRequest, attachment []byte) (reverseControlResponse, []byte, error) {
	address := a.Config.Runtime.ControlAddress
	if err := validateLoopbackControlAddress(address); err != nil {
		return reverseControlResponse{}, nil, err
	}
	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		peer.forgetCapabilities()
		if ctx.Err() != nil {
			return reverseControlResponse{}, nil, ctx.Err()
		}
		return reverseControlResponse{}, nil, &controlDialError{err: fmt.Errorf("connect to local reverse control at %s: %w", address, err)}
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	requested, hasDeadline := ctx.Deadline()
	if hasDeadline && isControlCallType(req.Type) {
		req.Envelope.DeadlineUnixMilli = requested.UnixMilli()
	}
	// The caller's context closes the connection the moment it ends (the
	// AfterFunc above), which is what reports context.DeadlineExceeded. The
	// socket deadline trails it slightly so it never races that and surfaces a
	// bare "i/o timeout" instead; it only matters if the daemon stops answering.
	const socketGrace = 2 * time.Second
	deadline := time.Now().Add(controlIOTimeout)
	if hasDeadline && requested.Before(deadline) {
		deadline = requested.Add(socketGrace)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return reverseControlResponse{}, nil, fmt.Errorf("set reverse control deadline: %w", err)
	}

	if err := writeControlMessage(conn, req, attachment); err != nil {
		if ctx.Err() != nil {
			return reverseControlResponse{}, nil, ctx.Err()
		}
		return reverseControlResponse{}, nil, err
	}
	decoder := json.NewDecoder(conn)
	var resp reverseControlResponse
	if err := decoder.Decode(&resp); err != nil {
		if ctx.Err() != nil {
			return reverseControlResponse{}, nil, ctx.Err()
		}
		return reverseControlResponse{}, nil, err
	}
	var blob []byte
	if resp.BinaryLength != nil {
		blob, _, err = readControlAttachment(decoder.Buffered(), conn, *resp.BinaryLength)
		if err != nil {
			if ctx.Err() != nil {
				return reverseControlResponse{}, nil, ctx.Err()
			}
			return reverseControlResponse{}, nil, err
		}
	}
	return resp, blob, nil
}

func isControlCallType(kind string) bool {
	return kind == controlTypeCall || kind == controlTypeCallFramed
}

// errControlUnsupported reports that the daemon does not implement a request
// type (it is older than this CLI).
var errControlUnsupported = errors.New("fleet daemon does not support this control request")

// controlDialError means the daemon's control socket could not be reached, so
// nothing was sent.
type controlDialError struct{ err error }

func (e *controlDialError) Error() string { return e.err.Error() }
func (e *controlDialError) Unwrap() error { return e.err }

// controlResponseError is an error the daemon reported. Its text is the
// "<code>: <message>" form callers have always seen.
type controlResponseError struct {
	Code    string
	Message string
}

func (e *controlResponseError) Error() string { return e.Code + ": " + e.Message }

// controlCall sends one RPC envelope through the daemon (kind "call" relays
// it to a reverse agent). It negotiates binary framing for attachments, retries once
// with a fresh token if the daemon restarted, and falls back to base64 if an
// older daemon has taken over the socket.
func (a *App) controlCall(ctx context.Context, kind, serverName string, env proto.Envelope) (proto.Envelope, error) {
	peer := a.controlPeer()
	framed := false
	if env.Binary != nil {
		framed = slices.Contains(a.controlCapabilitiesFor(ctx, peer), controlCapBinaryFrame)
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		token, err := peer.cachedToken()
		if err != nil {
			return proto.Envelope{}, err
		}
		req := reverseControlRequest{
			Token:    token,
			Type:     kind,
			Server:   serverName,
			Envelope: env,
			Accept:   []string{controlCapBinaryFrame},
		}
		var attachment []byte
		if env.Binary != nil {
			if framed {
				if kind == controlTypeCall {
					req.Type = controlTypeCallFramed
				}
				n := len(env.Binary)
				req.BinaryLength = &n
				attachment = env.Binary
			} else {
				req.EnvelopeBinary = env.Binary
			}
		}
		resp, blob, err := a.controlRoundTrip(ctx, peer, req, attachment)
		if err != nil {
			return proto.Envelope{}, err
		}
		if resp.Error != nil {
			lastErr = &controlResponseError{Code: resp.Error.Code, Message: resp.Error.Message}
			switch code := resp.Error.Code; {
			case code == "unauthorized" && peer.refreshToken(token):
				continue // the daemon restarted and minted a new token
			case framed && (code == "unsupported_action" || code == "decode_error" || code == "request_too_large"):
				// An older daemon rejected the framed request before acting
				// on it. Remember that and resend the attachment as base64.
				peer.setCapabilities([]string{})
				framed = false
				continue
			case code == "unsupported_action" && kind != controlTypeCall:
				return proto.Envelope{}, fmt.Errorf("%w: %s", errControlUnsupported, resp.Error.Message)
			}
			return proto.Envelope{}, lastErr
		}
		out, ok := resp.responseEnvelope()
		if !ok {
			return proto.Envelope{}, fmt.Errorf("reverse control did not return a response envelope")
		}
		if resp.BinaryLength != nil {
			out.Binary = blob
		}
		return out, nil
	}
	return proto.Envelope{}, lastErr
}

// controlSimple sends a request that carries no envelope (status, disconnect)
// and retries once if the daemon restarted with a new token.
func (a *App) controlSimple(kind, serverName string) (reverseControlResponse, error) {
	peer := a.controlPeer()
	for attempt := 0; ; attempt++ {
		token, err := peer.cachedToken()
		if err != nil {
			return reverseControlResponse{}, err
		}
		resp, _, err := a.controlRoundTrip(context.Background(), peer, reverseControlRequest{Token: token, Type: kind, Server: serverName}, nil)
		if err != nil {
			return reverseControlResponse{}, err
		}
		if resp.Error != nil && resp.Error.Code == "unauthorized" && attempt == 0 && peer.refreshToken(token) {
			continue
		}
		return resp, nil
	}
}

func (a *App) callReverseControlContext(ctx context.Context, serverName string, env proto.Envelope) (proto.Envelope, error) {
	return a.controlCall(ctx, controlTypeCall, serverName, env)
}

func (a *App) callReverseStatus(serverName string) (ReverseSessionInfo, error) {
	resp, err := a.controlSimple(controlTypeStatus, serverName)
	if err != nil {
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
	resp, err := a.controlSimple(controlTypeDisconnect, serverName)
	if err != nil {
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

func readControlTokenFile(path string) (string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is the controller's own data/control.token
	if err != nil {
		return "", fmt.Errorf("read control token: %w", err)
	}
	// Trim surrounding whitespace: the token is compared byte-for-byte with a
	// constant-time compare, so a trailing newline introduced by an editor or a
	// manual rewrite of the file would fail authentication with no useful
	// diagnostic. generateControlToken writes no newline, so this is
	// forward-compatible robustness, not a behavior change today.
	return strings.TrimSpace(string(data)), nil
}
