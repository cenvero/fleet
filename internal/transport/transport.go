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

func SupportedCiphers() []string {
	return []string{
		"chacha20-poly1305@openssh.com",
		"aes256-gcm@openssh.com",
	}
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
