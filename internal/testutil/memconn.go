// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package testutil

import (
	"bytes"
	"io"
	"net"
	"sync"
	"time"
)

type addr string

func (a addr) Network() string { return "tcp" }
func (a addr) String() string  { return string(a) }

type memPipe struct {
	once sync.Once
	done chan struct{}
	a2b  chan []byte
	b2a  chan []byte
}

type memConn struct {
	pipe   *memPipe
	in     chan []byte
	out    chan []byte
	local  net.Addr
	remote net.Addr
	mu     sync.Mutex
	buf    bytes.Buffer
}

func NewBufferedConnPair(localA, localB string) (net.Conn, net.Conn) {
	pipe := &memPipe{
		done: make(chan struct{}),
		a2b:  make(chan []byte, 32),
		b2a:  make(chan []byte, 32),
	}

	return &memConn{
			pipe:   pipe,
			in:     pipe.b2a,
			out:    pipe.a2b,
			local:  addr(localA),
			remote: addr(localB),
		}, &memConn{
			pipe:   pipe,
			in:     pipe.a2b,
			out:    pipe.b2a,
			local:  addr(localB),
			remote: addr(localA),
		}
}

func (c *memConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if c.buf.Len() > 0 {
		n, err := c.buf.Read(p)
		c.mu.Unlock()
		return n, err
	}
	c.mu.Unlock()

	select {
	case payload := <-c.in:
		c.mu.Lock()
		_, _ = c.buf.Write(payload)
		n, err := c.buf.Read(p)
		c.mu.Unlock()
		return n, err
	case <-c.pipe.done:
		// Drain any buffered data before returning EOF.
		select {
		case payload := <-c.in:
			c.mu.Lock()
			_, _ = c.buf.Write(payload)
			n, err := c.buf.Read(p)
			c.mu.Unlock()
			return n, err
		default:
			return 0, io.EOF
		}
	}
}

func (c *memConn) Write(p []byte) (n int, err error) {
	payload := make([]byte, len(p))
	copy(payload, p)

	select {
	case <-c.pipe.done:
		return 0, io.ErrClosedPipe
	case c.out <- payload:
		return len(payload), nil
	}
}

// Close signals shutdown via the done channel only. Data channels are never
// closed because closing a channel while another goroutine is mid-send on it
// is a data race; the done signal is sufficient to unblock all waiters.
func (c *memConn) Close() error {
	c.pipe.once.Do(func() {
		close(c.pipe.done)
	})
	return nil
}

func (c *memConn) LocalAddr() net.Addr  { return c.local }
func (c *memConn) RemoteAddr() net.Addr { return c.remote }

func (c *memConn) SetDeadline(time.Time) error      { return nil }
func (c *memConn) SetReadDeadline(time.Time) error  { return nil }
func (c *memConn) SetWriteDeadline(time.Time) error { return nil }
