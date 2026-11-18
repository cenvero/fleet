// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package transport

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
	"golang.org/x/crypto/ssh"
)

// fakeChannel is an ssh.Channel whose writes and reads can be made to fail.
type fakeChannel struct {
	writeErr error
	written  int
}

func (c *fakeChannel) Read([]byte) (int, error) { return 0, io.EOF }
func (c *fakeChannel) Write(p []byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	c.written += len(p)
	return len(p), nil
}
func (c *fakeChannel) Close() error                                   { return nil }
func (c *fakeChannel) CloseWrite() error                              { return nil }
func (c *fakeChannel) SendRequest(string, bool, []byte) (bool, error) { return false, nil }
func (c *fakeChannel) Stderr() io.ReadWriter                          { return nil }

var _ ssh.Channel = (*fakeChannel)(nil)

// A call that fails before its request is written certainly did not run and
// may be retried; one that fails after the request went out (the connection
// dropped while waiting for the reply) may have run and must not be.
func TestRequestNotSentClassifiesCallFailures(t *testing.T) {
	env := proto.Envelope{Action: "shell.exec", Payload: proto.ExecPayload{Command: "true"}}

	unsent := &Session{Channel: &fakeChannel{writeErr: errors.New("broken pipe")}}
	if _, err := unsent.Call(context.Background(), env); err == nil || !RequestNotSent(err) {
		t.Fatalf("a failed write: err = %v, RequestNotSent = %v; want true", err, RequestNotSent(err))
	}

	sentCh := &fakeChannel{}
	sent := &Session{Channel: sentCh}
	if _, err := sent.Call(context.Background(), env); err == nil || RequestNotSent(err) {
		t.Fatalf("EOF after the request was written: err = %v, RequestNotSent = %v; want false", err, RequestNotSent(err))
	}
	if sentCh.written == 0 {
		t.Fatal("the request was not written")
	}

	if RequestNotSent(errors.New("plain")) || !RequestNotSent(NotSent(errors.New("dial failed"))) || NotSent(nil) != nil {
		t.Fatal("NotSent / RequestNotSent do not round-trip")
	}
	var closed *Session
	if _, err := closed.Call(context.Background(), env); !RequestNotSent(err) {
		t.Fatalf("a call on no session: %v, want not sent", err)
	}
}
