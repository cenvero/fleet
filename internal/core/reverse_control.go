// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
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
	"syscall"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

// The daemon's control socket is how every other process on this machine — a
// CLI invocation, the web UI, the TUI — reaches the reverse agents connected
// to the daemon, and (see direct_relay.go) the daemon's warm connections to
// direct-mode servers. It listens on loopback only. Both ends know the
// daemon's control token (data/control.token, 0600); nobody else does.
//
// One request per connection. The caller closing its end early cancels the
// call.
//
// Mutual authentication. Anyone on the machine can connect to — or, while the
// daemon is down, listen on — a loopback port, so each end must prove it holds
// the token before the other sends anything of value:
//
//  1. caller → daemon: {"auth":"hmac-sha256-v1","client_nonce":N_c}   (no token)
//  2. daemon → caller: {"auth":…,"server_nonce":N_s,"proof":P_d,"capabilities":…}
//     P_d = HMAC-SHA256(token, "fleet-control-daemon" ‖ N_c ‖ N_s)
//  3. the caller checks P_d in constant time and gives up unless it matches;
//     only then does it send its request, carrying
//     "client_proof": HMAC-SHA256(token, "fleet-control-client" ‖ N_s ‖ N_c)
//     in place of the raw token.
//  4. the daemon checks the client proof, then reads and serves the request.
//
// Fresh nonces from both sides make every proof single-use. Before a caller
// has proved itself the daemon reads at most maxControlPreAuthBytes and waits
// at most controlAuthTimeout.
//
// Legacy mode. A CLI from before mutual authentication sends its request with
// the raw token as the first member; the daemon checks that token before
// reading the rest and serves the original request types (call, status,
// disconnect) exactly as before. Every current daemon mints its token with
// controlTokenMutualAuthPrefix; a caller holding such a token only ever uses
// the handshake and never downgrades, so an impostor cannot talk it into
// presenting the token. A token without the prefix was written by a daemon
// from before mutual authentication, which can only be reached with the raw
// token: a current CLI then sends the legacy request for reverse-mode calls
// (the only route they have), exactly as an older CLI does, and does not try
// the direct-mode relay at all.
//
// Binary framing (authenticated mode only): a daemon that lists
// controlCapBinaryFrame in its capabilities accepts "call.framed", where
// BinaryLength says the envelope's attachment follows the request's newline as
// exactly that many raw bytes. A caller that lists controlCapBinaryFrame in
// Accept gets a response attachment the same way instead of base64 in
// ResponseBinary; older callers never ask and keep getting base64.

const (
	maxControlRequestBytes = 32 << 20
	controlIOTimeout       = 30 * time.Second
	// maxControlPreAuthBytes bounds what the daemon reads from a connection
	// before its peer has proved it holds the token: the auth hello, then the
	// first member of the request (its client proof), or — for a legacy
	// request — the first member carrying the raw token. Each is well under
	// 200 bytes.
	maxControlPreAuthBytes = 4 << 10
	// controlAuthTimeout bounds the unauthenticated phase of a connection.
	controlAuthTimeout = 5 * time.Second
	// maxControlChallengeBytes bounds the daemon's reply to an auth hello as
	// read by a caller (an impostor must not be able to make it buffer much).
	maxControlChallengeBytes = 64 << 10
	// maxConcurrentControlConnections bounds how many control connections the
	// daemon reads and authenticates at once.
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

const (
	controlAuthVersion = "hmac-sha256-v1"
	controlNonceBytes  = 32
	// controlTokenMutualAuthPrefix marks a control token written by a daemon
	// that performs mutual authentication. A caller holding such a token never
	// falls back to sending it in the clear.
	controlTokenMutualAuthPrefix = "ma1-"
)

// controlSlotWait is how long a queued connection waits for a handler slot
// before the daemon answers "busy". Excess connections used to be closed on the
// spot, which reached the caller as a bare EOF — indistinguishable from the
// server being offline. A variable so tests can shorten it.
var controlSlotWait = 15 * time.Second

// Control-protocol capabilities a daemon advertises in its auth challenge.
const (
	controlCapBinaryFrame = "binary-frame"
	controlCapDirectCall  = "direct-call"
)

// Control request types.
const (
	controlTypeCall       = "call"
	controlTypeCallFramed = "call.framed"
	controlTypeCallDirect = "call.direct"
	controlTypeStatus     = "status"
	controlTypeDisconnect = "disconnect"
)

// controlCapabilities lists what this daemon's control socket understands.
func (h *ReverseHub) controlCapabilities() []string {
	return []string{controlCapBinaryFrame, controlCapDirectCall}
}

// controlAuthHello opens a mutually authenticated exchange. "auth" must stay
// the first member: the daemon tells the modes apart by the first member.
type controlAuthHello struct {
	Auth        string `json:"auth"`
	ClientNonce string `json:"client_nonce"`
}

// controlAuthChallenge is the daemon's answer to an auth hello, or an error.
type controlAuthChallenge struct {
	Auth         string       `json:"auth,omitempty"`
	ServerNonce  string       `json:"server_nonce,omitempty"`
	Proof        string       `json:"proof,omitempty"`
	Capabilities []string     `json:"capabilities,omitempty"`
	Error        *proto.Error `json:"error,omitempty"`
}

func controlMAC(token, label string, first, second []byte) []byte {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(label))
	mac.Write(first)
	mac.Write(second)
	return mac.Sum(nil)
}

// controlDaemonProof is what the daemon sends to prove it holds token.
func controlDaemonProof(token string, clientNonce, serverNonce []byte) []byte {
	return controlMAC(token, "fleet-control-daemon", clientNonce, serverNonce)
}

// controlClientProof is what a caller sends to prove it holds token.
func controlClientProof(token string, serverNonce, clientNonce []byte) []byte {
	return controlMAC(token, "fleet-control-client", serverNonce, clientNonce)
}

func newControlNonce() ([]byte, error) {
	nonce := make([]byte, controlNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate control nonce: %w", err)
	}
	return nonce, nil
}

func decodeControlNonce(s string) ([]byte, bool) {
	b, err := hex.DecodeString(s)
	return b, err == nil && len(b) == controlNonceBytes
}

// tokenRequiresMutualAuth reports whether token was minted by a daemon that
// performs mutual authentication, so it must never be sent in the clear.
func tokenRequiresMutualAuth(token string) bool {
	return strings.HasPrefix(token, controlTokenMutualAuthPrefix)
}

// --- daemon side --------------------------------------------------------------

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
			// Even the queue is full. Say so rather than closing silently: a
			// caller that sees "busy" knows nothing was done and can fall back
			// (a direct-mode call dials the server itself). The reply fits in
			// an empty socket buffer, so this never blocks the accept loop.
			rejectControlBusy(conn, "fleet daemon control socket is overloaded; try again")
		}
	}
}

// rejectControlBusy answers a connection that will not be served and closes it.
func rejectControlBusy(conn net.Conn, message string) {
	_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	_ = writeControlResponse(conn, reverseControlResponse{Error: &proto.Error{
		Code: "control_busy", Message: message, Retry: true,
	}}, nil)
	_ = conn.Close()
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
		rejectControlBusy(conn, "fleet daemon control socket is busy; try again")
	case <-ctx.Done():
		<-h.controlWaiters
		_ = conn.Close()
	}
}

func (h *ReverseHub) handleControlConn(conn net.Conn) {
	h.handleControl(conn, nil)
}

// acquireCallSlot bounds authenticated calls in progress. A direct relay does
// not wait (the caller can simply dial the server itself); a reverse call
// waits up to controlSlotWait, since the daemon is its only route.
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

// firstJSONMember reads the opening of a JSON object from r — its first key
// and that key's value — reading no more than max bytes. consumed is every
// byte read from r, so the whole object can be parsed again from the start.
func firstJSONMember(r io.Reader, max int) (key string, value json.RawMessage, consumed []byte, err error) {
	var record bytes.Buffer
	dec := json.NewDecoder(io.TeeReader(io.LimitReader(r, int64(max)), &record))
	tok, err := dec.Token()
	if err != nil {
		return "", nil, record.Bytes(), err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return "", nil, record.Bytes(), fmt.Errorf("control message is not a JSON object")
	}
	tok, err = dec.Token()
	if err != nil {
		return "", nil, record.Bytes(), err
	}
	key, ok := tok.(string)
	if !ok {
		return "", nil, record.Bytes(), fmt.Errorf("control message is empty")
	}
	if err := dec.Decode(&value); err != nil {
		return "", nil, record.Bytes(), err
	}
	return key, value, record.Bytes(), nil
}

type controlAuthMode int

const (
	controlAuthLegacy controlAuthMode = iota + 1
	controlAuthMutual
)

// readControlRequest authenticates the connection and reads the request. On
// failure it has already answered the caller. pending holds the bytes already
// read past the request's JSON value; the stream then continues with limited,
// the size-capped connection reader.
func (h *ReverseHub) readControlRequest(conn net.Conn) (req reverseControlRequest, mode controlAuthMode, pending io.Reader, limited *io.LimitedReader, ok bool) {
	fail := func(code, message string) (reverseControlRequest, controlAuthMode, io.Reader, *io.LimitedReader, bool) {
		_ = writeControlError(conn, code, message)
		return reverseControlRequest{}, 0, nil, nil, false
	}
	key, value, consumed, err := firstJSONMember(conn, maxControlPreAuthBytes)
	if err != nil {
		return fail("decode_error", "invalid control request")
	}
	var stream io.Reader = conn
	switch key {
	case "token":
		// A caller from before mutual authentication. Every such CLI encodes
		// the token as the first member, so it is checked before the rest of
		// the request (up to maxControlRequestBytes) is read.
		var token string
		if json.Unmarshal(value, &token) != nil || subtle.ConstantTimeCompare([]byte(token), []byte(h.controlToken)) != 1 {
			return fail("unauthorized", "invalid control token")
		}
		mode = controlAuthLegacy
	case "auth":
		var hello controlAuthHello
		helloSrc := io.MultiReader(bytes.NewReader(consumed), io.LimitReader(conn, int64(maxControlPreAuthBytes)))
		helloDec := json.NewDecoder(helloSrc)
		if err := helloDec.Decode(&hello); err != nil {
			return fail("decode_error", "invalid control auth hello")
		}
		clientNonce, valid := decodeControlNonce(hello.ClientNonce)
		if hello.Auth != controlAuthVersion || !valid {
			return fail("unauthorized", "unsupported control authentication")
		}
		serverNonce, err := newControlNonce()
		if err != nil {
			return fail("internal_error", "could not generate a nonce")
		}
		if err := writeControlMessage(conn, controlAuthChallenge{
			Auth:         controlAuthVersion,
			ServerNonce:  hex.EncodeToString(serverNonce),
			Proof:        hex.EncodeToString(controlDaemonProof(h.controlToken, clientNonce, serverNonce)),
			Capabilities: h.controlCapabilities(),
		}, nil); err != nil {
			return reverseControlRequest{}, 0, nil, nil, false
		}
		// The request follows on the same connection; its first member must
		// be the caller's proof. (The stream continues with whatever the hello
		// decoder read ahead, then whatever of its input it has not read yet.)
		stream = io.MultiReader(helloDec.Buffered(), helloSrc, conn)
		key, value, consumed, err = firstJSONMember(stream, maxControlPreAuthBytes)
		if err != nil || key != "client_proof" {
			return fail("unauthorized", "control request did not prove the caller holds the token")
		}
		var proofHex string
		proof, decodeErr := []byte(nil), json.Unmarshal(value, &proofHex)
		if decodeErr == nil {
			proof, decodeErr = hex.DecodeString(proofHex)
		}
		if decodeErr != nil || !hmac.Equal(proof, controlClientProof(h.controlToken, serverNonce, clientNonce)) {
			return fail("unauthorized", "invalid control proof")
		}
		mode = controlAuthMutual
	default:
		return fail("unauthorized", "control request must authenticate first")
	}

	// Authenticated: the rest of the request may be up to the full size.
	_ = conn.SetDeadline(time.Now().Add(controlIOTimeout))
	limited = &io.LimitedReader{R: stream, N: maxControlRequestBytes + 1 - int64(len(consumed))}
	recorded := bytes.NewReader(consumed)
	decoder := json.NewDecoder(io.MultiReader(recorded, limited))
	if err := decoder.Decode(&req); err != nil {
		return fail("decode_error", "invalid control request")
	}
	if limited.N <= 0 {
		return fail("request_too_large", "control request exceeds limit or contains trailing data")
	}
	// pending is what has already been read off the connection past the
	// request: the decoder's read-ahead, then whatever firstJSONMember
	// recorded that the decoder has not reached. The stream continues with
	// limited.
	return req, mode, io.MultiReader(decoder.Buffered(), recorded), limited, true
}

// handleControl serves one control connection. releaseReadSlot, when set, is
// called once the request has been read and authenticated.
func (h *ReverseHub) handleControl(conn net.Conn, releaseReadSlot func()) {
	defer conn.Close()
	if releaseReadSlot == nil {
		releaseReadSlot = func() {}
	}
	_ = conn.SetDeadline(time.Now().Add(controlAuthTimeout))

	req, mode, buffered, limited, ok := h.readControlRequest(conn)
	if !ok {
		return
	}
	if mode == controlAuthLegacy {
		// Legacy callers only ever used these; everything newer requires the
		// mutually authenticated handshake.
		switch req.Type {
		case controlTypeCall, controlTypeStatus, controlTypeDisconnect:
		default:
			_ = writeControlError(conn, "unsupported_action", fmt.Sprintf("control request %q is not supported", req.Type))
			return
		}
		if req.BinaryLength != nil {
			_ = writeControlError(conn, "decode_error", "legacy control requests cannot carry a raw attachment")
			return
		}
	}

	var attachment []byte
	if req.BinaryLength == nil {
		trailing, _ := io.ReadAll(buffered)
		if len(strings.TrimSpace(string(trailing))) != 0 {
			_ = writeControlError(conn, "request_too_large", "control request exceeds limit or contains trailing data")
			return
		}
	} else {
		if req.Type != controlTypeCallFramed && req.Type != controlTypeCallDirect {
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
	framedResponse := mode == controlAuthMutual && slices.Contains(req.Accept, controlCapBinaryFrame)

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
	case controlTypeCallDirect:
		releaseCall, ok := h.acquireCallSlot(false)
		if !ok {
			resp.Error = &proto.Error{Code: "control_busy", Message: "fleet daemon has too many calls in progress", Retry: true}
			break
		}
		env := req.envelope()
		if attachment != nil {
			env.Binary = attachment
		}
		// A direct call keeps the semantics it has without the daemon: bounded
		// by the caller's own deadline (if any) and by the caller hanging up,
		// not by the control socket's I/O timeout.
		if deadline := req.Envelope.DeadlineUnixMilli; deadline > 0 {
			_ = conn.SetDeadline(time.UnixMilli(deadline).Add(5 * time.Second))
		} else {
			_ = conn.SetDeadline(time.Time{})
		}
		out, err := runControlCall(conn, req.Envelope.DeadlineUnixMilli, false, func(ctx context.Context) (proto.Envelope, error) {
			return h.app.relayDirectCall(ctx, req.Server, req.Direct, env)
		})
		releaseCall()
		_ = conn.SetWriteDeadline(time.Now().Add(controlIOTimeout))
		if err != nil {
			var mismatch *directRelayMismatchError
			if errors.As(err, &mismatch) {
				resp.Error = &proto.Error{Code: "direct_target_mismatch", Message: err.Error()}
				break
			}
			resp.Error = &proto.Error{Code: "direct_call_failed", Message: err.Error()}
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

// controlPeer caches the control token of the daemon behind one control
// address. It used to be re-read from disk on every call.
type controlPeer struct {
	mu        sync.Mutex
	tokenPath string
	token     string
	haveToken bool
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

// refreshToken re-reads the token after the daemon could not use the cached
// one (it mints a new token every time it starts) and reports whether it
// changed, i.e. whether retrying can help.
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

var (
	// errControlNotAuthenticated: the daemon answered the auth hello with an
	// error — it predates mutual authentication (or pretends to).
	errControlNotAuthenticated = errors.New("fleet daemon does not support mutual authentication")
	// errControlImpostor: whatever answered could not prove it holds the
	// control token. Nothing but the auth hello was sent to it.
	errControlImpostor = errors.New("the process on the control address could not prove it is the fleet daemon")
	// errControlUnsupported reports that the daemon does not implement a
	// request type (it is older than this CLI).
	errControlUnsupported = errors.New("fleet daemon does not support this control request")
)

// controlDialError means the daemon's control socket could not be reached, so
// nothing was sent.
type controlDialError struct{ err error }

func (e *controlDialError) Error() string { return e.err.Error() }
func (e *controlDialError) Unwrap() error { return e.err }

// controlNotSentError wraps a failure that happened before the request left
// this process: dialling, the authentication handshake, or the daemon lacking
// what the request needs. The caller may safely do the work another way.
type controlNotSentError struct{ err error }

func (e *controlNotSentError) Error() string { return e.err.Error() }
func (e *controlNotSentError) Unwrap() error { return e.err }

// controlResponseError is an error the daemon reported. Its text is the
// "<code>: <message>" form callers have always seen.
type controlResponseError struct {
	Code    string
	Message string
}

func (e *controlResponseError) Error() string { return e.Code + ": " + e.Message }

// controlSession is a connection on which the daemon has proved itself and
// which is ready for one request.
type controlSession struct {
	reader io.Reader // what the handshake decoder read ahead, then the connection
	proof  string    // this caller's proof, to send with the request
	caps   []string
}

// controlHandshake runs the caller's side of mutual authentication on conn.
// It sends only a nonce, and returns errControlImpostor unless the peer proves
// it holds token.
func controlHandshake(conn net.Conn, token string) (*controlSession, error) {
	clientNonce, err := newControlNonce()
	if err != nil {
		return nil, err
	}
	if err := writeControlMessage(conn, controlAuthHello{Auth: controlAuthVersion, ClientNonce: hex.EncodeToString(clientNonce)}, nil); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(io.LimitReader(conn, maxControlChallengeBytes))
	var challenge controlAuthChallenge
	if err := decoder.Decode(&challenge); err != nil {
		return nil, err
	}
	if challenge.Error != nil {
		if challenge.Error.Code == "control_busy" {
			return nil, &controlResponseError{Code: challenge.Error.Code, Message: challenge.Error.Message}
		}
		return nil, fmt.Errorf("%w (%s: %s)", errControlNotAuthenticated, challenge.Error.Code, challenge.Error.Message)
	}
	serverNonce, valid := decodeControlNonce(challenge.ServerNonce)
	proof, proofErr := hex.DecodeString(challenge.Proof)
	if challenge.Auth != controlAuthVersion || !valid || proofErr != nil ||
		!hmac.Equal(proof, controlDaemonProof(token, clientNonce, serverNonce)) {
		return nil, errControlImpostor
	}
	return &controlSession{
		reader: io.MultiReader(decoder.Buffered(), conn),
		proof:  hex.EncodeToString(controlClientProof(token, serverNonce, clientNonce)),
		caps:   challenge.Capabilities,
	}, nil
}

// controlRequestSpec describes one control exchange.
type controlRequestSpec struct {
	kind   string
	server string
	env    *proto.Envelope // nil for status/disconnect
	direct *controlDirectTarget
	// requireCapability, when set, is a capability the daemon must advertise;
	// without it the request is not sent.
	requireCapability string
	// requireMutualAuth refuses the legacy raw-token fallback.
	requireMutualAuth bool
}

// build assembles the request for a daemon with caps (nil in legacy mode).
func (s controlRequestSpec) build(caps []string, legacy bool) (reverseControlRequest, []byte) {
	req := reverseControlRequest{Type: s.kind, Server: s.server, Direct: s.direct}
	if s.env == nil {
		return req, nil
	}
	req.Envelope = *s.env
	if legacy {
		// Exactly what a CLI from before mutual authentication sends.
		req.EnvelopeBinary = s.env.Binary
		return req, nil
	}
	req.Accept = []string{controlCapBinaryFrame}
	if s.env.Binary == nil {
		return req, nil
	}
	if !slices.Contains(caps, controlCapBinaryFrame) {
		req.EnvelopeBinary = s.env.Binary
		return req, nil
	}
	if req.Type == controlTypeCall {
		req.Type = controlTypeCallFramed
	}
	n := len(s.env.Binary)
	req.BinaryLength = &n
	return req, s.env.Binary
}

// controlDial connects to the daemon's control socket and arms the deadlines
// appropriate for kind. The returned stop releases the context hook.
func (a *App) controlDial(ctx context.Context, kind string) (net.Conn, func(), error) {
	address := a.Config.Runtime.ControlAddress
	if err := validateLoopbackControlAddress(address); err != nil {
		return nil, nil, &controlNotSentError{err: err}
	}
	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, &controlNotSentError{err: ctx.Err()}
		}
		dialErr := fmt.Errorf("connect to local reverse control at %s: %w", address, err)
		if errors.Is(err, syscall.ECONNREFUSED) {
			dialErr = fmt.Errorf("%w (the fleet daemon is not running; start it with `fleet start`, or run `fleet daemon`)", dialErr)
		}
		return nil, nil, &controlNotSentError{err: &controlDialError{err: dialErr}}
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	requested, hasDeadline := ctx.Deadline()
	// The caller's context closes the connection the moment it ends (the
	// AfterFunc above), which is what reports context.DeadlineExceeded. The
	// socket deadline trails it slightly so it never races that and surfaces a
	// bare "i/o timeout" instead; it only matters if the daemon stops answering.
	const socketGrace = 2 * time.Second
	var deadlineErr error
	if kind == controlTypeCallDirect {
		// A relayed direct call is bounded by the caller's own deadline only,
		// exactly as the call would be if this process made it itself.
		deadline := time.Time{}
		if hasDeadline {
			deadline = requested.Add(socketGrace)
		}
		deadlineErr = errors.Join(conn.SetReadDeadline(deadline), conn.SetWriteDeadline(time.Now().Add(controlIOTimeout)))
	} else {
		deadline := time.Now().Add(controlIOTimeout)
		if hasDeadline && requested.Before(deadline) {
			deadline = requested.Add(socketGrace)
		}
		deadlineErr = conn.SetDeadline(deadline)
	}
	if deadlineErr != nil {
		stopCancel()
		_ = conn.Close()
		return nil, nil, &controlNotSentError{err: fmt.Errorf("set reverse control deadline: %w", deadlineErr)}
	}
	return conn, func() { stopCancel() }, nil
}

// controlExchange performs one control request: mutually authenticated when
// the daemon supports it, or — only for callers that allow it and only with a
// daemon that predates mutual authentication — the legacy raw-token request.
func (a *App) controlExchange(ctx context.Context, spec controlRequestSpec) (reverseControlResponse, []byte, error) {
	peer := a.controlPeer()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		token, err := peer.cachedToken()
		if err != nil {
			return reverseControlResponse{}, nil, &controlNotSentError{err: err}
		}
		resp, blob, err := a.controlExchangeOnce(ctx, spec, token)
		switch {
		case (errors.Is(err, errControlImpostor) || errors.Is(err, errControlNotAuthenticated)) && peer.refreshToken(token):
			// Most likely the daemon restarted (possibly as another version)
			// with a new token; nothing was sent, so try once more with the
			// token now on disk.
			lastErr = err
			continue
		case err == nil && resp.Error != nil && resp.Error.Code == "unauthorized" && peer.refreshToken(token):
			// A legacy daemon rejected a stale token before acting on it.
			lastErr = &controlResponseError{Code: resp.Error.Code, Message: resp.Error.Message}
			continue
		}
		return resp, blob, err
	}
	return reverseControlResponse{}, nil, lastErr
}

func (a *App) controlExchangeOnce(ctx context.Context, spec controlRequestSpec, token string) (reverseControlResponse, []byte, error) {
	if !tokenRequiresMutualAuth(token) {
		// The token was written by a daemon from before mutual authentication
		// (every current daemon marks its token), so whatever answers can
		// only be reached with the raw token, which is all it ever had —
		// exactly the request an older CLI sends. Nothing that needs a
		// daemon which proved itself (the direct-mode relay) is attempted.
		if spec.requireMutualAuth {
			return reverseControlResponse{}, nil, &controlNotSentError{err: errControlNotAuthenticated}
		}
		return a.controlLegacyExchange(ctx, spec, token)
	}
	conn, stop, err := a.controlDial(ctx, spec.kind)
	if err != nil {
		return reverseControlResponse{}, nil, err
	}
	session, err := controlHandshake(conn, token)
	if err != nil {
		stop()
		_ = conn.Close()
		if ctx.Err() != nil {
			return reverseControlResponse{}, nil, &controlNotSentError{err: ctx.Err()}
		}
		// Never downgrade: the daemon that minted this token authenticates
		// itself, so anything that does not is not that daemon.
		return reverseControlResponse{}, nil, &controlNotSentError{err: err}
	}
	defer conn.Close()
	defer stop()
	if spec.requireCapability != "" && !slices.Contains(session.caps, spec.requireCapability) {
		return reverseControlResponse{}, nil, &controlNotSentError{err: fmt.Errorf("%w: %s", errControlUnsupported, spec.requireCapability)}
	}
	req, attachment := spec.build(session.caps, false)
	req.ClientProof = session.proof
	return readControlReply(ctx, conn, session.reader, req, attachment)
}

// controlLegacyExchange sends the request the way a CLI from before mutual
// authentication does.
func (a *App) controlLegacyExchange(ctx context.Context, spec controlRequestSpec, token string) (reverseControlResponse, []byte, error) {
	conn, stop, err := a.controlDial(ctx, spec.kind)
	if err != nil {
		return reverseControlResponse{}, nil, err
	}
	defer conn.Close()
	defer stop()
	req, _ := spec.build(nil, true)
	req.Token = token
	return readControlReply(ctx, conn, conn, req, nil)
}

// readControlReply writes req (and its attachment) and reads the response.
func readControlReply(ctx context.Context, conn net.Conn, reader io.Reader, req reverseControlRequest, attachment []byte) (reverseControlResponse, []byte, error) {
	if err := writeControlMessage(conn, req, attachment); err != nil {
		if ctx.Err() != nil {
			return reverseControlResponse{}, nil, ctx.Err()
		}
		return reverseControlResponse{}, nil, err
	}
	decoder := json.NewDecoder(reader)
	var resp reverseControlResponse
	if err := decoder.Decode(&resp); err != nil {
		if ctx.Err() != nil {
			return reverseControlResponse{}, nil, ctx.Err()
		}
		return reverseControlResponse{}, nil, err
	}
	var blob []byte
	if resp.BinaryLength != nil {
		var err error
		blob, _, err = readControlAttachment(decoder.Buffered(), reader, *resp.BinaryLength)
		if err != nil {
			if ctx.Err() != nil {
				return reverseControlResponse{}, nil, ctx.Err()
			}
			return reverseControlResponse{}, nil, err
		}
	}
	return resp, blob, nil
}

// controlCall sends one RPC envelope through the daemon: kind is "call" for a
// reverse agent or "call.direct" for a direct-mode server relayed over the
// daemon's pool (which requires a daemon that proved itself and supports it).
func (a *App) controlCall(ctx context.Context, kind, serverName string, env proto.Envelope, direct *controlDirectTarget) (proto.Envelope, error) {
	spec := controlRequestSpec{kind: kind, server: serverName, env: &env, direct: direct}
	if kind == controlTypeCallDirect {
		spec.requireMutualAuth = true
		spec.requireCapability = controlCapDirectCall
	}
	resp, blob, err := a.controlExchange(ctx, spec)
	if err != nil {
		return proto.Envelope{}, err
	}
	if resp.Error != nil {
		if resp.Error.Code == "unsupported_action" && kind != controlTypeCall {
			return proto.Envelope{}, fmt.Errorf("%w: %s", errControlUnsupported, resp.Error.Message)
		}
		return proto.Envelope{}, &controlResponseError{Code: resp.Error.Code, Message: resp.Error.Message}
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

func (a *App) callReverseControlContext(ctx context.Context, serverName string, env proto.Envelope) (proto.Envelope, error) {
	return a.controlCall(ctx, controlTypeCall, serverName, env, nil)
}

func (a *App) callReverseStatus(serverName string) (ReverseSessionInfo, error) {
	resp, _, err := a.controlExchange(context.Background(), controlRequestSpec{kind: controlTypeStatus, server: serverName})
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
	resp, _, err := a.controlExchange(context.Background(), controlRequestSpec{kind: controlTypeDisconnect, server: serverName})
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
		if errors.Is(err, os.ErrNotExist) {
			// The daemon writes this file when it starts and removes it when it
			// stops, so a missing file means no daemon is running.
			return "", fmt.Errorf("the fleet daemon is not running (reverse-mode servers are reached through it); start it with `fleet start`, or run `fleet daemon`: %w", err)
		}
		return "", fmt.Errorf("read control token (is `fleet daemon` running?): %w", err)
	}
	// Trim surrounding whitespace: the token is compared byte-for-byte with a
	// constant-time compare, so a trailing newline introduced by an editor or a
	// manual rewrite of the file would fail authentication with no useful
	// diagnostic. generateControlToken writes no newline, so this is
	// forward-compatible robustness, not a behavior change today.
	return strings.TrimSpace(string(data)), nil
}

// removeControlTokenIfOwned deletes data/control.token on daemon shutdown if
// it still holds this daemon's token, so no process keeps presenting a token
// to whatever might listen on the control address next. A token written by a
// daemon started since is left alone.
func (a *App) removeControlTokenIfOwned(token string) {
	path := a.controlTokenPath()
	if current, err := readControlTokenFile(path); err == nil && subtle.ConstantTimeCompare([]byte(current), []byte(token)) == 1 {
		_ = os.Remove(path)
	}
}
