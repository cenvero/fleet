// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/cpu"
)

type Mode string

const (
	ModeReverse Mode = "reverse"
	ModeDirect  Mode = "direct"
	ModePerNode Mode = "per-server"
)

func (m Mode) String() string {
	return string(m)
}

func ParseMode(v string) (Mode, error) {
	switch strings.TrimSpace(strings.ToLower(v)) {
	case string(ModeReverse):
		return ModeReverse, nil
	case string(ModeDirect):
		return ModeDirect, nil
	case string(ModePerNode), "none", "decide":
		return ModePerNode, nil
	default:
		return "", fmt.Errorf("invalid transport mode %q", v)
	}
}

// SupportedCiphers returns the curated AEAD ciphers pinned on every fleet SSH
// config, ordered by what is fastest on THIS machine.
//
// The set is fixed and deliberately small — both entries are modern AEADs, and
// narrowing it is what stops a downgrade attacker steering onto something weak.
// Only the order is dynamic, and order is purely a performance decision: SSH
// negotiation walks the client's list and takes the first entry the server also
// offers, and every fleet peer offers both, so the choice never affects whether
// a handshake succeeds or how strong it is.
//
// The order matters a great deal for throughput. On a CPU with AES instructions
// — anything x86 since AES-NI, and ARM64 with the crypto extensions — AES-GCM
// runs several times faster than ChaCha20-Poly1305 (measured on an Apple M4:
// 6.9 GB/s vs 1.4 GB/s). Without those instructions the ranking inverts, which
// is exactly why ChaCha20 exists. Preferring ChaCha20 unconditionally, as this
// did, left most of the transport's headroom unused on typical server hardware.
func SupportedCiphers() []string {
	if hasHardwareAES() {
		return []string{
			"aes256-gcm@openssh.com",
			"chacha20-poly1305@openssh.com",
		}
	}
	return []string{
		"chacha20-poly1305@openssh.com",
		"aes256-gcm@openssh.com",
	}
}

// hasHardwareAES reports whether this CPU implements AES in hardware.
//
// This gate is a security requirement as much as a performance one. Hardware AES
// is constant-time; a software AES implementation is the one that has to worry
// about cache-timing side channels, and ChaCha20 is constant-time everywhere by
// construction. So preferring AES-GCM only when the CPU can do it in hardware
// gets the faster cipher exactly where it is also the safe one, and falls back
// to ChaCha20 everywhere else.
//
// The fields are defined on every platform (zero on architectures they don't
// apply to), so this needs no build tags.
func hasHardwareAES() bool {
	return cpu.X86.HasAES || cpu.ARM64.HasAES || cpu.ARM.HasAES || cpu.S390X.HasAESGCM
}

type ServerTarget struct {
	Name    string
	Address string
	Port    int
	Mode    Mode
	User    string
}

type Session struct {
	Mode               Mode
	LocalAddr          net.Addr
	RemoteAddr         net.Addr
	HostKeyFingerprint string
	Client             *ssh.Client
	Channel            ssh.Channel
	Closer             io.Closer
	// childOfClient marks a session that borrows a parent's *ssh.Client (created
	// via OpenChannelSession for parallel transfers). Its Close() must release
	// only its own channel — closing the shared client would kill every sibling
	// channel. The parent session owns the client lifecycle.
	childOfClient bool
	mu            sync.Mutex
}

type Connector struct {
	Mode               Mode
	Username           string
	PrivateKeyPath     string
	KnownHostsPath     string
	AcceptNewHostKey   bool
	PrivateKeyPassphr  []byte
	NetworkDialContext func(context.Context, string, string) (net.Conn, error)
}

func CapabilitySupported(caps []string, capability string) bool {
	for _, candidate := range caps {
		if candidate == capability {
			return true
		}
	}
	return false
}

func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	if s.Channel != nil {
		_ = s.Channel.Close()
	}
	// A child session shares its parent's client — never close it here.
	if s.childOfClient {
		return nil
	}
	if s.Client != nil {
		return s.Client.Close()
	}
	if s.Closer != nil {
		return s.Closer.Close()
	}
	return nil
}

// OpenChannelSession opens an additional fleet-rpc channel on this session's
// existing SSH client and wraps it in its own Session. The agent accepts many
// channels per connection (each served by its own goroutine), so N channels on
// one client give N parallel in-flight RPCs without a fresh SSH handshake.
//
// The returned child has its own Call mutex (so calls run concurrently with the
// parent and other children) and closes only its channel — the caller must keep
// the parent session alive for the lifetime of every child and close the parent
// last.
func (s *Session) OpenChannelSession() (*Session, error) {
	if s == nil || s.Client == nil {
		return nil, fmt.Errorf("transport session has no ssh client")
	}
	channel, requests, err := s.Client.OpenChannel(RPCChannelType, nil)
	if err != nil {
		return nil, fmt.Errorf("open extra %s channel: %w", RPCChannelType, err)
	}
	go ssh.DiscardRequests(requests)
	return &Session{
		Mode:               s.Mode,
		LocalAddr:          s.LocalAddr,
		RemoteAddr:         s.RemoteAddr,
		HostKeyFingerprint: s.HostKeyFingerprint,
		Client:             s.Client,
		Channel:            channel,
		childOfClient:      true,
	}, nil
}
