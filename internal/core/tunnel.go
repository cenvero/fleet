// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"net"

	"github.com/cenvero/fleet/internal/transport"
)

// TunnelDialer is a live connection to a server's agent that can open TCP
// connections FROM the server to an arbitrary host:port (SSH direct-tcpip).
//
// It is produced by App.OpenTunnelDialer and MUST be closed by the caller when
// the tunnel is torn down: Close releases the underlying *ssh.Client (and the
// agent RPC channel) created for the tunnel.
type TunnelDialer struct {
	session *transport.Session
}

// Dial opens a TCP connection from the server to addr (host:port). The bytes
// travel: caller -> controller -> [SSH] -> server -> target. This is the SSH
// "direct-tcpip" forwarding primitive: the server, not the controller, makes
// the outbound connection, so it reaches hosts that are only routable from the
// server's network (e.g. a private database at 10.0.0.2:5432).
func (d *TunnelDialer) Dial(network, addr string) (net.Conn, error) {
	if d == nil || d.session == nil || d.session.Client == nil {
		return nil, fmt.Errorf("tunnel dialer is not open")
	}
	return d.session.Client.Dial(network, addr)
}

// Close tears down the SSH client backing the tunnel.
func (d *TunnelDialer) Close() error {
	if d == nil || d.session == nil {
		return nil
	}
	return d.session.Close()
}

// OpenTunnelDialer opens a dedicated direct-mode SSH session to serverName and
// returns a TunnelDialer that forwards new TCP connections from the server.
//
// Only direct mode is supported: the controller holds a live *ssh.Client to the
// agent and can use SSH direct-tcpip. Reverse-mode agents dial the controller
// and are multiplexed through the reverse hub's control socket, which does not
// hand a raw *ssh.Client to the CLI process, so direct-tcpip is not available
// there.
func (a *App) OpenTunnelDialer(serverName string) (*TunnelDialer, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return nil, err
	}
	if server.Mode != transport.ModeDirect {
		return nil, fmt.Errorf("tunnel requires a direct-mode server; %q is in %s mode", serverName, server.Mode)
	}
	session, _, err := a.openDirectSession(server, false)
	if err != nil {
		return nil, err
	}
	return &TunnelDialer{session: session}, nil
}
