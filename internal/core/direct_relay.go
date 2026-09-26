// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
)

// Direct-mode relay through the daemon.
//
// Every `fleet` invocation is a new process with an empty connection pool, so a
// one-shot direct-mode command paid a full connection setup — TCP, SSH version
// exchange, key exchange, public-key auth, channel open, hello — plus a
// private-key read and a TOML rewrite of the server record, before its single
// RPC: about ten round trips, ~550 ms at 50 ms RTT. A running `fleet daemon`
// already holds warm pooled connections (its metrics poller keeps them alive),
// so when one is running a direct-mode call is handed to it over the loopback
// control socket ("call.direct") and rides its pool: one round trip to the
// server. The relay is only used with a daemon that has proved, per
// connection, that it holds the control token (see reverse_control.go): an
// impostor listening on the control address gets a nonce and nothing else.
//
// What does not change: every authorisation decision — RBAC token scope,
// cmd-policy, approvals, secret resolution, redaction, audit — happens in the
// CLI before callRPCContext is reached, exactly as before; the daemon only
// moves the already-authorised envelope. The daemon connects with the same
// config directory's key and known_hosts pin (it refuses when its view of the
// server's address, port, user, key or known_hosts differs from the caller's,
// and it redials if either file changed since its connection was made, so a
// rotated key or re-pinned host key is never ridden on an old connection).
// Holding the control token already implies read access to that config
// directory (and so to the private key), so the relay grants nothing new.
//
// Fallback is automatic and happens before anything but a nonce has left the
// process: no daemon (its control token is removed when it stops), an older
// daemon or anything else that cannot prove it holds the token, a busy daemon,
// or a target mismatch all make the CLI dial the server itself.
// FLEET_NO_DAEMON_RELAY=1 disables the relay for a process.

// controlDirectTarget is the connection identity the caller resolved for a
// direct-mode server; the daemon relays only if it resolves the same.
type controlDirectTarget struct {
	Address        string `json:"address"`
	Port           int    `json:"port"`
	User           string `json:"user"`
	KeyPath        string `json:"key_path"`
	KnownHostsPath string `json:"known_hosts_path"`
}

func (a *App) directTargetFor(server ServerRecord) controlDirectTarget {
	return controlDirectTarget{
		Address:        server.Address,
		Port:           server.Port,
		User:           server.User,
		KeyPath:        a.serverPrivateKeyPath(server),
		KnownHostsPath: a.Config.Crypto.KnownHostsPath,
	}
}

// directRelayMismatchError means the daemon will not relay this call because
// its view of the target differs from the caller's; the caller dials itself.
type directRelayMismatchError struct{ reason string }

func (e *directRelayMismatchError) Error() string {
	return "daemon will not relay direct call: " + e.reason
}

// daemonApps records the Apps currently running RunDaemon. Their own direct
// calls must never be relayed back to their own control socket.
var daemonApps sync.Map // *App → struct{}

// directRelayRetryAfter is how long a process stops trying the daemon after it
// could not be reached, so a CLI or web UI running without a daemon does not
// pay a refused loopback connect on every call.
const directRelayRetryAfter = 3 * time.Second

var directRelayUnreachable sync.Map // control address → time.Time (retry after)

// relayDirectCall runs on the daemon: it serves a "call.direct" request over
// the daemon's own pooled connection to the server.
func (a *App) relayDirectCall(ctx context.Context, name string, want *controlDirectTarget, env proto.Envelope) (proto.Envelope, error) {
	if want == nil {
		return proto.Envelope{}, &directRelayMismatchError{reason: "request did not identify its target"}
	}
	// The name becomes a path under servers/; refuse anything but a plain name.
	if err := validateSafeName(name); err != nil {
		return proto.Envelope{}, &directRelayMismatchError{reason: err.Error()}
	}
	server, err := a.GetServer(name)
	if err != nil {
		return proto.Envelope{}, &directRelayMismatchError{reason: err.Error()}
	}
	if server.Mode != transport.ModeDirect {
		return proto.Envelope{}, &directRelayMismatchError{reason: fmt.Sprintf("server %q is not in direct mode", name)}
	}
	if got := a.directTargetFor(server); got != *want {
		return proto.Envelope{}, &directRelayMismatchError{reason: fmt.Sprintf("server %q resolves differently in the daemon", name)}
	}
	a.sessions.evictIfCredentialsChanged(name, want.KeyPath, want.KnownHostsPath)
	return a.callDirectPooledContext(ctx, server, env)
}

// tryDaemonDirectRelay hands a direct-mode call to a running daemon. handled is
// false when the call was not relayed (and nothing was sent to the server), in
// which case the caller must make the call itself.
func (a *App) tryDaemonDirectRelay(ctx context.Context, server ServerRecord, env proto.Envelope) (out proto.Envelope, handled bool, err error) {
	if !a.directRelayEnabled(server) {
		return proto.Envelope{}, false, nil
	}
	address := a.Config.Runtime.ControlAddress
	if until, ok := directRelayUnreachable.Load(address); ok && time.Now().Before(until.(time.Time)) {
		return proto.Envelope{}, false, nil
	}
	if _, err := a.controlPeer().cachedToken(); err != nil {
		return proto.Envelope{}, false, nil // no daemon is running here
	}
	// controlCall only sends the envelope after whatever is listening on the
	// control address has proved it holds the control token, and only if it
	// advertises direct-call; an older daemon (or anything else) never sees
	// more than a nonce.
	target := a.directTargetFor(server)
	out, err = a.controlCall(ctx, controlTypeCallDirect, server.Name, env, &target)
	if err == nil {
		return out, true, nil
	}
	var notSent *controlNotSentError
	var respErr *controlResponseError
	switch {
	case errors.As(err, &notSent):
		if ctx.Err() != nil {
			return proto.Envelope{}, true, ctx.Err()
		}
		// Unreachable, not authenticated, busy, or lacking the capability:
		// nothing left this process. Dial the server ourselves, and do not
		// knock again for a few seconds.
		directRelayUnreachable.Store(address, time.Now().Add(directRelayRetryAfter))
		return proto.Envelope{}, false, nil
	case errors.Is(err, errControlUnsupported):
		return proto.Envelope{}, false, nil
	case errors.As(err, &respErr):
		switch respErr.Code {
		case "direct_target_mismatch", "control_busy", "unauthorized", "unsupported_action":
			// The daemon refused before doing anything: dial it ourselves.
			return proto.Envelope{}, false, nil
		case "direct_call_failed":
			// Surface the error exactly as a call made here would have.
			switch respErr.Message {
			case context.DeadlineExceeded.Error():
				return proto.Envelope{}, true, context.DeadlineExceeded
			case context.Canceled.Error():
				return proto.Envelope{}, true, context.Canceled
			}
			return proto.Envelope{}, true, errors.New(respErr.Message)
		}
	}
	return proto.Envelope{}, true, err
}

// directRelayEnabled reports whether this process should try the daemon.
func (a *App) directRelayEnabled(server ServerRecord) bool {
	if server.Mode != transport.ModeDirect || os.Getenv("FLEET_NO_DAEMON_RELAY") != "" {
		return false
	}
	// A custom dialer (tests, proxies) must not be bypassed, and the daemon
	// itself serves these calls from its own pool.
	if a.NetworkDialContext != nil {
		return false
	}
	if _, isDaemon := daemonApps.Load(a); isDaemon {
		return false
	}
	// A long-running process that already holds a warm connection of its own
	// keeps using it; the relay exists for processes that would have to dial.
	return !a.sessions.has(server.Name)
}
