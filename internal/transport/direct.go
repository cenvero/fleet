// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package transport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/crypto"
	"github.com/cenvero/fleet/pkg/proto"
	"golang.org/x/crypto/ssh"
)

// SupportedKEX returns the curated key-exchange algorithms pinned on every
// fleet SSH config (client and server). Pinning these alongside the AEAD cipher
// list means a downgrade attacker cannot steer the handshake onto a weak/legacy
// KEX (e.g. SHA-1 or 1024-bit DH groups) that the Go default set still offers
// for interop. All fleet peers run golang.org/x/crypto/ssh, which supports this
// modern set, so the pin never breaks a fleet-to-fleet handshake.
func SupportedKEX() []string {
	return []string{
		"curve25519-sha256",
		"curve25519-sha256@libssh.org",
		"ecdh-sha2-nistp256",
		"ecdh-sha2-nistp384",
		"ecdh-sha2-nistp521",
		"diffie-hellman-group16-sha512",
		"diffie-hellman-group14-sha256",
	}
}

// SupportedMACs returns the curated MAC algorithms pinned on every fleet SSH
// config. The pinned ciphers are AEAD (chacha20-poly1305 / aes256-gcm) and
// supply their own integrity, so a MAC is not even negotiated with them; this
// list exists purely as defense in depth so that if the cipher set is ever
// widened, integrity cannot be downgraded to SHA-1. Encrypt-then-MAC variants
// are preferred over the plain (encrypt-and-MAC) constructions.
func SupportedMACs() []string {
	return []string{
		"hmac-sha2-256-etm@openssh.com",
		"hmac-sha2-512-etm@openssh.com",
		"hmac-sha2-256",
		"hmac-sha2-512",
	}
}

// SupportedHostKeyAlgos returns the host-key signature algorithms a fleet client
// will accept from a fleet peer. Every fleet host key is Ed25519 (see
// EnsureEd25519Signer / GenerateKeySet), so constraining this prevents a MITM
// from negotiating a weaker host-key type. RSA SHA-2 variants are included only
// for the optional RSA-4096 controller key offered for legacy interop.
func SupportedHostKeyAlgos() []string {
	return []string{
		ssh.KeyAlgoED25519,
		ssh.KeyAlgoRSASHA512,
		ssh.KeyAlgoRSASHA256,
	}
}

const RPCChannelType = "fleet-rpc"

// ShellChannelType is the SSH channel type used for interactive shell sessions
// through the fleet agent. Only fleet clients know this type — port scanners
// using standard SSH tooling cannot open a shell even if they somehow have a
// valid key.
const ShellChannelType = "fleet-shell"

func (c Connector) DialContext(ctx context.Context, target ServerTarget) (*Session, error) {
	if target.Port == 0 {
		target.Port = 22
	}
	if target.Port < 1 || target.Port > 65535 {
		return nil, fmt.Errorf("invalid port %d: must be between 1 and 65535", target.Port)
	}
	username := c.Username
	if target.User != "" {
		username = target.User
	}
	if username == "" {
		username = "cenvero-agent"
	}

	signer, err := crypto.LoadPrivateKeySigner(c.PrivateKeyPath, c.PrivateKeyPassphr)
	if err != nil {
		return nil, err
	}

	hostKeyState := &HostKeyState{}
	hostKeyCallback, err := NewTOFUHostKeyCallback(c.KnownHostsPath, c.AcceptNewHostKey, hostKeyState)
	if err != nil {
		return nil, err
	}

	address := net.JoinHostPort(target.Address, strconv.Itoa(target.Port))
	config := &ssh.ClientConfig{
		Config: ssh.Config{
			Ciphers:      SupportedCiphers(),
			KeyExchanges: SupportedKEX(),
			MACs:         SupportedMACs(),
		},
		User:              username,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback:   hostKeyCallback,
		HostKeyAlgorithms: SupportedHostKeyAlgos(),
		Timeout:           10 * time.Second,
	}

	var rawConn net.Conn
	if c.NetworkDialContext != nil {
		rawConn, err = c.NetworkDialContext(ctx, "tcp", address)
	} else {
		rawConn, err = (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", address, err)
	}

	// ssh.ClientConfig.Timeout is not honored by NewClientConn when callers
	// provide an already-dialled net.Conn. Bound the version/KEX/auth exchange on
	// the socket itself, respecting an earlier caller deadline.
	handshakeDeadline := time.Now().Add(config.Timeout)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(handshakeDeadline) {
		handshakeDeadline = deadline
	}
	if err := rawConn.SetDeadline(handshakeDeadline); err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("set ssh handshake deadline for %s: %w", address, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(rawConn, address, config)
	if err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("establish ssh connection to %s: %w", address, err)
	}
	if err := rawConn.SetDeadline(time.Time{}); err != nil {
		_ = sshConn.Close()
		return nil, fmt.Errorf("clear ssh handshake deadline for %s: %w", address, err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	channel, requests, err := client.OpenChannel(RPCChannelType, nil)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("open %s channel: %w", RPCChannelType, err)
	}
	go ssh.DiscardRequests(requests)

	return &Session{
		Mode:               target.Mode,
		LocalAddr:          sshConn.LocalAddr(),
		RemoteAddr:         sshConn.RemoteAddr(),
		HostKeyFingerprint: hostKeyState.Fingerprint,
		Client:             client,
		Channel:            channel,
	}, nil
}

func (s *Session) Call(ctx context.Context, env proto.Envelope) (proto.Envelope, error) {
	if s == nil || s.Channel == nil {
		return proto.Envelope{}, fmt.Errorf("transport session is not open")
	}
	s.callMu.Lock()
	defer s.callMu.Unlock()
	if err := ctx.Err(); err != nil {
		return proto.Envelope{}, err
	}
	if env.RequestID == "" {
		requestID, err := newRequestID()
		if err != nil {
			return proto.Envelope{}, err
		}
		env.RequestID = requestID
	}
	env.Type = proto.EnvelopeTypeRequest
	env.ProtocolVersion = proto.CurrentProtocolVersion
	_, hasDeadline := ctx.Deadline()
	if deadline, ok := ctx.Deadline(); ok {
		env.DeadlineUnixMilli = deadline.UnixMilli()
	}

	canCancel := s.SupportsCapability(proto.CapabilityRequestCancel)
	if strings.EqualFold(env.Action, "shell.exec") && ctx.Done() != nil && !hasDeadline && !canCancel {
		return proto.Envelope{}, fmt.Errorf("agent does not advertise %s; refusing deadline-free cancellable execution until it is upgraded", proto.CapabilityRequestCancel)
	}

	if err := s.encode(env); err != nil {
		return proto.Envelope{}, fmt.Errorf("encode rpc request: %w", err)
	}

	decode := func() (proto.Envelope, error) {
		resp, err := proto.Decode(s.Channel)
		if err != nil {
			return proto.Envelope{}, fmt.Errorf("decode rpc response: %w", err)
		}
		if resp.RequestID != "" && resp.RequestID != env.RequestID {
			return proto.Envelope{}, fmt.Errorf("rpc response request id %q does not match %q", resp.RequestID, env.RequestID)
		}
		return resp, nil
	}

	// Background/TODO cannot be cancelled, so avoid a goroutine on transfer hot
	// paths. Cancellable calls use a read goroutine while this method retains the
	// logical-call lock; request.cancel is the only frame allowed to bypass it.
	if ctx.Done() == nil {
		return decode()
	}

	type result struct {
		response proto.Envelope
		err      error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := decode()
		done <- result{response: resp, err: err}
	}()

	select {
	case out := <-done:
		return out.response, out.err
	case <-ctx.Done():
		if !canCancel {
			_ = s.Channel.Close()
			go func() { <-done }()
			return proto.Envelope{}, &callContextError{err: ctx.Err()}
		}
		cancelEnv := proto.Envelope{
			Type:            proto.EnvelopeTypeEvent,
			ProtocolVersion: proto.CurrentProtocolVersion,
			Action:          proto.ActionRequestCancel,
			Payload:         proto.CancelRequestPayload{RequestID: env.RequestID},
		}
		if err := s.encode(cancelEnv); err != nil {
			_ = s.Channel.Close()
			go func() { <-done }()
			return proto.Envelope{}, &callContextError{err: ctx.Err()}
		}

		// Do not return while the original response is still on the wire: the next
		// pooled caller could otherwise consume it. A capable agent kills the
		// process group and replies promptly. If it lies about the capability,
		// close only this RPC channel and mark it unusable.
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case out := <-done:
			if out.err != nil {
				return proto.Envelope{}, &callContextError{err: ctx.Err()}
			}
			return proto.Envelope{}, &callContextError{err: ctx.Err(), sessionUsable: true}
		case <-timer.C:
			_ = s.Channel.Close()
			go func() { <-done }()
			return proto.Envelope{}, &callContextError{err: ctx.Err()}
		}
	}
}

func (s *Session) encode(env proto.Envelope) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return proto.Encode(s.Channel, env)
}

func (s *Session) Hello(ctx context.Context, controllerID string) (proto.HelloPayload, error) {
	response, err := s.Call(ctx, proto.Envelope{
		Action: "hello",
		Payload: map[string]any{
			"controller_id": controllerID,
		},
	})
	if err != nil {
		return proto.HelloPayload{}, err
	}
	if response.Error != nil {
		return proto.HelloPayload{}, fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}
	payload, err := proto.DecodeHelloPayload(response.Payload)
	if err != nil {
		return proto.HelloPayload{}, err
	}
	s.SetCapabilities(payload.Capabilities)
	return payload, nil
}

func newRequestID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate request id: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}
