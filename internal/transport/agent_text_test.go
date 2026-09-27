// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package transport

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

// pipeChannel is an ssh.Channel over one end of an in-memory pipe.
type pipeChannel struct{ net.Conn }

func (pipeChannel) CloseWrite() error                              { return nil }
func (pipeChannel) SendRequest(string, bool, []byte) (bool, error) { return false, nil }
func (pipeChannel) Stderr() io.ReadWriter                          { return nil }

// hostileAgent answers every request on conn with reply(request).
func hostileAgent(t *testing.T, conn net.Conn, reply func(proto.Envelope) proto.Envelope) {
	t.Helper()
	go func() {
		for {
			req, err := proto.Decode(conn)
			if err != nil {
				return
			}
			resp := reply(req)
			resp.Type = proto.EnvelopeTypeResponse
			resp.RequestID = req.RequestID
			resp.Action = req.Action
			if err := proto.Encode(conn, resp); err != nil {
				return
			}
		}
	}()
}

func hasControl(s string) bool {
	for _, r := range s {
		if (r < 0x20 && r != '\t' && r != '\n') || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x202e {
			return true
		}
	}
	return false
}

// TestCallNeutralisesAgentErrorText: an agent's error code and message end up
// on the operator's terminal (every failing command prints them), in alerts,
// notifications and the audit log. Escape sequences in them must arrive as
// visible text and an absurd message must be cut down.
func TestCallNeutralisesAgentErrorText(t *testing.T) {
	ctrl, agentEnd := net.Pipe()
	defer ctrl.Close()
	defer agentEnd.Close()
	hostileAgent(t, agentEnd, func(proto.Envelope) proto.Envelope {
		return proto.Envelope{Error: &proto.Error{
			Code:    "bad\x1b]0;pwned\x07code",
			Message: "denied\r\x1b[2Kall good\u202e " + strings.Repeat("A", 1<<20),
		}}
	})
	s := &Session{Channel: pipeChannel{ctrl}}
	resp, err := s.Call(context.Background(), proto.Envelope{Action: "file.delete"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("agent error was dropped")
	}
	if hasControl(resp.Error.Code) || hasControl(resp.Error.Message) {
		t.Fatalf("agent error reaches the controller with control characters: %q / %q", resp.Error.Code, resp.Error.Message[:80])
	}
	if !strings.HasPrefix(resp.Error.Message, `denied\x0d\x1b[2Kall good\u202e AAA`) {
		t.Fatalf("agent error text not escaped visibly: %q", resp.Error.Message[:80])
	}
	if len(resp.Error.Message) > 4*maxAgentErrorMessage || len(resp.Error.Code) > 4*maxAgentErrorCode {
		t.Fatalf("agent error not bounded: code %d bytes, message %d bytes", len(resp.Error.Code), len(resp.Error.Message))
	}
}

// TestHelloNeutralisesAgentSelfDescription: hello fields are stored in the
// server record and shown by `server list`, inventory, the dashboard and the
// web UI; a hostile agent must not plant escape sequences or megabytes there.
func TestHelloNeutralisesAgentSelfDescription(t *testing.T) {
	ctrl, agentEnd := net.Pipe()
	defer ctrl.Close()
	defer agentEnd.Close()
	caps := make([]string, 0, 10_000)
	for i := 0; i < 10_000; i++ {
		caps = append(caps, "cap\x1b[31m")
	}
	caps[0] = proto.CapabilityBinaryFrames
	hostileAgent(t, agentEnd, func(proto.Envelope) proto.Envelope {
		return proto.Envelope{Payload: proto.HelloPayload{
			NodeName:     "web\x1b[2J01",
			AgentVersion: "v2.4.3\r\x1b[2Kv9.9.9",
			OS:           "linux",
			Arch:         "amd64\x07",
			Transport:    "direct",
			FileRoot:     "/srv/" + strings.Repeat("d", 1<<20),
			Capabilities: caps,
		}}
	})
	s := &Session{Channel: pipeChannel{ctrl}}
	hello, err := s.Hello(context.Background(), "controller")
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	for _, field := range []string{hello.NodeName, hello.AgentVersion, hello.OS, hello.Arch, hello.Transport, hello.FileRoot} {
		if hasControl(field) {
			t.Fatalf("hello field keeps control characters: %q", field)
		}
	}
	if hello.NodeName != `web\x1b[2J01` || hello.OS != "linux" || hello.Transport != "direct" {
		t.Fatalf("hello fields not escaped visibly / changed: %+v", hello)
	}
	if len(hello.FileRoot) > 4*maxHelloField {
		t.Fatalf("hello file root not bounded: %d bytes", len(hello.FileRoot))
	}
	if len(hello.Capabilities) > maxHelloCapabilities || hello.Capabilities[0] != proto.CapabilityBinaryFrames {
		t.Fatalf("hello capabilities not bounded or reordered: %d entries, first %q", len(hello.Capabilities), hello.Capabilities[0])
	}
	for _, c := range hello.Capabilities {
		if hasControl(c) {
			t.Fatalf("capability keeps control characters: %q", c)
		}
	}
	if !s.SupportsCapability(proto.CapabilityBinaryFrames) {
		t.Fatal("a genuine capability was lost")
	}
}
