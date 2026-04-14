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
	for {
		if c.buf.Len() > 0 {
			return c.buf.Read(p)
		}
		select {
		case payload, ok := <-c.in:
			if !ok {
				return 0, io.EOF
			}
			_, _ = c.buf.Write(payload)
		case <-c.pipe.done:
			if c.buf.Len() == 0 {
				return 0, io.EOF
			}
		}
	}
}

func (c *memConn) Write(p []byte) (n int, err error) {
	payload := make([]byte, len(p))
	copy(payload, p)

	defer func() {
		if recover() != nil {
			// Sending on a channel that was just closed should behave like a closed socket,
			// not crash the test process.
			n = 0
			err = io.ErrClosedPipe
		}
	}()

	select {
	case <-c.pipe.done:
		return 0, io.ErrClosedPipe
	case c.out <- payload:
		return len(payload), nil
	}
}

func (c *memConn) Close() error {
	c.pipe.once.Do(func() {
		close(c.pipe.done)
		close(c.pipe.a2b)
		close(c.pipe.b2a)
	})
	return nil
}

func (c *memConn) LocalAddr() net.Addr  { return c.local }
func (c *memConn) RemoteAddr() net.Addr { return c.remote }

func (c *memConn) SetDeadline(time.Time) error      { return nil }
func (c *memConn) SetReadDeadline(time.Time) error  { return nil }
func (c *memConn) SetWriteDeadline(time.Time) error { return nil }
