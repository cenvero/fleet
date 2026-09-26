// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package transport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// KeepaliveRequest is the global request used to probe a connection. Every SSH
// implementation answers unknown global requests that want a reply (fleet peers
// reply false via ssh.DiscardRequests or the client's built-in handler), and
// any answer at all proves the peer and the path to it are alive. Using the
// OpenSSH name keeps the probe unremarkable to anything in between.
const KeepaliveRequest = "keepalive@openssh.com"

const (
	// DefaultKeepaliveInterval is how often a long-lived fleet connection is
	// probed. It is short enough to keep NAT/conntrack state fresh and to notice
	// a vanished peer within about a minute, and long enough to be free.
	DefaultKeepaliveInterval = 15 * time.Second
	// DefaultKeepaliveMaxMissed is how many consecutive intervals a probe may go
	// unanswered before the connection is declared dead and closed.
	DefaultKeepaliveMaxMissed = 3
	// DefaultChannelOpenTimeout bounds how long opening a fleet-rpc channel on
	// an established connection may take. A healthy peer answers a channel open
	// in one round trip (or rejects it at once when it is at capacity); a
	// connection that cannot answer within this is treated as dead rather than
	// being allowed to block its caller for minutes.
	DefaultChannelOpenTimeout = 15 * time.Second
)

// ErrChannelOpenTimeout reports that the peer did not answer a channel open in
// time. The connection it was attempted on should be considered dead.
var ErrChannelOpenTimeout = errors.New("timed out opening ssh channel")

// StartKeepalive probes conn every interval and closes it once maxMissed
// consecutive probes have gone unanswered, or as soon as a probe fails because
// the connection is already gone. onDead, when non-nil, runs once after the
// connection has been closed for either reason, so an owner (a connection pool,
// a session registry) can retire it immediately instead of discovering it on
// the next call. The returned stop function ends probing without touching the
// connection; it is safe to call more than once.
//
// Without this a pooled connection whose peer vanished without a FIN (a NAT or
// firewall that dropped state, a host that lost power) looked healthy until a
// caller tried to use it, and then that caller could block for minutes waiting
// on TCP to give up.
func StartKeepalive(conn ssh.Conn, interval time.Duration, maxMissed int, onDead func()) (stop func()) {
	if conn == nil {
		return func() {}
	}
	if interval <= 0 {
		interval = DefaultKeepaliveInterval
	}
	if maxMissed <= 0 {
		maxMissed = DefaultKeepaliveMaxMissed
	}
	done := make(chan struct{})
	var stopOnce sync.Once
	stop = func() { stopOnce.Do(func() { close(done) }) }

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		// replies is buffered so an outstanding probe can always finish and exit,
		// even after this loop has returned.
		replies := make(chan error, 1)
		outstanding := false
		missed := 0
		dead := func() {
			_ = conn.Close()
			if onDead != nil {
				onDead()
			}
		}
		for {
			select {
			case <-done:
				return
			case err := <-replies:
				outstanding = false
				if err != nil {
					// The connection is closed or broken; SendRequest only fails
					// once the transport has gone away.
					dead()
					return
				}
				missed = 0
			case <-ticker.C:
				if outstanding {
					missed++
					if missed >= maxMissed {
						// Closing the connection also unblocks the outstanding
						// SendRequest, whose goroutine then exits.
						dead()
						return
					}
					continue
				}
				outstanding = true
				go func() {
					_, _, err := conn.SendRequest(KeepaliveRequest, true, nil)
					replies <- err
				}()
			}
		}
	}()
	return stop
}

// OpenChannelTimeout opens a channel of chanType on conn, giving up after
// timeout or when ctx ends, whichever comes first. ssh.Conn.OpenChannel itself
// has no deadline: on a half-dead connection it waits for a confirmation that
// never comes. On timeout the caller gets ErrChannelOpenTimeout and should
// retire the connection; on cancellation it gets ctx.Err(). Either way a
// confirmation that arrives late is closed rather than leaked.
func OpenChannelTimeout(ctx context.Context, conn ssh.Conn, chanType string, extra []byte, timeout time.Duration) (ssh.Channel, <-chan *ssh.Request, error) {
	if conn == nil {
		return nil, nil, fmt.Errorf("open %s channel: no connection", chanType)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = DefaultChannelOpenTimeout
	}
	type result struct {
		channel  ssh.Channel
		requests <-chan *ssh.Request
		err      error
	}
	results := make(chan result, 1)
	go func() {
		channel, requests, err := conn.OpenChannel(chanType, extra)
		results <- result{channel, requests, err}
	}()
	abandon := func() {
		go func() {
			if late := <-results; late.err == nil {
				go ssh.DiscardRequests(late.requests)
				_ = late.channel.Close()
			}
		}()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-results:
		return r.channel, r.requests, r.err
	case <-timer.C:
		abandon()
		return nil, nil, fmt.Errorf("open %s channel: %w after %s", chanType, ErrChannelOpenTimeout, timeout)
	case <-ctx.Done():
		abandon()
		return nil, nil, ctx.Err()
	}
}

// ChannelOpenRejected reports whether err is the peer explicitly refusing a
// channel (for example because it is at its per-connection channel cap). A
// rejection proves the connection is alive, so it must not be retired for it.
func ChannelOpenRejected(err error) bool {
	var rejected *ssh.OpenChannelError
	return errors.As(err, &rejected)
}
