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
	mu                 sync.Mutex
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
	if s.Client != nil {
		return s.Client.Close()
	}
	if s.Closer != nil {
		return s.Closer.Close()
	}
	return nil
}
