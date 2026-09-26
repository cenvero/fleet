// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/cenvero/fleet/internal/transport"
)

// pipeRPCChannel is an in-memory ssh.Channel for driving serveRPC directly.
type pipeRPCChannel struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (c *pipeRPCChannel) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *pipeRPCChannel) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c *pipeRPCChannel) Close() error {
	_ = c.r.Close()
	return c.w.Close()
}
func (c *pipeRPCChannel) CloseWrite() error                              { return c.w.Close() }
func (c *pipeRPCChannel) SendRequest(string, bool, []byte) (bool, error) { return false, nil }
func (c *pipeRPCChannel) Stderr() io.ReadWriter                          { return &bytes.Buffer{} }

func newPipeRPCChannelPair() (*pipeRPCChannel, *pipeRPCChannel) {
	ar, aw := io.Pipe()
	br, bw := io.Pipe()
	return &pipeRPCChannel{r: ar, w: bw}, &pipeRPCChannel{r: br, w: aw}
}

func TestControllerIDFromPayloadDecodesWirePayloads(t *testing.T) {
	cases := []struct {
		name    string
		payload any
		want    string
	}{
		// What every hello off the wire looks like since Envelope keeps payloads raw.
		{"raw json", json.RawMessage(`{"controller_id":"ctl-123"}`), "ctl-123"},
		// In-process callers may still hand over a live map.
		{"map", map[string]any{"controller_id": "ctl-456"}, "ctl-456"},
		{"nil", nil, ""},
		{"missing", json.RawMessage(`{}`), ""},
		{"wrong type", json.RawMessage(`{"controller_id":42}`), ""},
		{"garbage", json.RawMessage(`not json`), ""},
	}
	for _, tc := range cases {
		if got := controllerIDFromPayload(tc.payload); got != tc.want {
			t.Errorf("%s: controllerIDFromPayload() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestHelloEchoesControllerID is the end-to-end regression: a controller's
// hello crosses the codec (so its payload arrives as json.RawMessage) and the
// agent must still echo the controller ID back.
func TestHelloEchoesControllerID(t *testing.T) {
	controllerSide, agentSide := newPipeRPCChannelPair()
	done := make(chan struct{})
	go func() {
		defer close(done)
		Server{Mode: transport.ModeDirect}.serveRPC(agentSide)
	}()
	session := &transport.Session{Mode: transport.ModeDirect, Channel: controllerSide}
	hello, err := session.Hello(t.Context(), "controller-instance-7")
	if err != nil {
		t.Fatalf("Hello() error = %v", err)
	}
	if hello.ControllerID != "controller-instance-7" {
		t.Fatalf("hello ControllerID = %q, want the controller's instance ID echoed", hello.ControllerID)
	}
	_ = controllerSide.Close()
	<-done
}
