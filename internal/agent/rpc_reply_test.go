// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
	"golang.org/x/crypto/ssh"
)

// countingChannel counts the bytes the controller side reads, so a test can
// report how large the agent's reply frame was.
type countingChannel struct {
	ssh.Channel
	read atomic.Int64
}

func (c *countingChannel) Read(p []byte) (int, error) {
	n, err := c.Channel.Read(p)
	c.read.Add(int64(n))
	return n, err
}

// startRPCAgent serves s over an in-memory channel and returns a controller
// session on the other end. The session reads replies with the real
// proto.Decode, so a reply over proto.MaxEnvelopeSize fails to decode.
func startRPCAgent(t *testing.T, s Server) (*transport.Session, *countingChannel) {
	t.Helper()
	controllerSide, agentSide := newPipeRPCChannelPair()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.serveRPC(agentSide)
	}()
	t.Cleanup(func() {
		_ = controllerSide.Close()
		<-done
	})
	counted := &countingChannel{Channel: controllerSide}
	return &transport.Session{Mode: transport.ModeDirect, Channel: counted}, counted
}

// callAgent makes one call with a bound, so a reply that never comes fails the
// test instead of hanging it.
func callAgent(t *testing.T, session *transport.Session, env proto.Envelope, timeout time.Duration) proto.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := session.Call(ctx, env)
	if err != nil {
		t.Fatalf("%s: no reply from the agent: %v", env.Action, err)
	}
	return resp
}

// hugeListing returns one directory entry whose name alone is larger than the
// protocol's envelope limit.
type hugeListing struct{ FileManager }

func (hugeListing) List(context.Context, proto.FileListPayload) (proto.FileListResult, error) {
	return proto.FileListResult{Path: "/big", Entries: []proto.FileEntry{{Name: strings.Repeat("x", proto.MaxEnvelopeSize+1)}}}, nil
}

// nanMetrics returns a snapshot encoding/json refuses to marshal.
type nanMetrics struct{}

func (nanMetrics) Collect(context.Context) (proto.MetricsSnapshot, error) {
	return proto.MetricsSnapshot{CPUPercent: math.NaN()}, nil
}

// TestReplyTooLargeIsReportedNotDropped: a reply the codec refuses to send
// because it is over the envelope limit (here a file.list, which has no entry
// cap) must come back as a small error envelope. It used to be dropped, and the
// controller waited for its own timeout.
func TestReplyTooLargeIsReportedNotDropped(t *testing.T) {
	session, counted := startRPCAgent(t, Server{FileManager: hugeListing{}})
	resp := callAgent(t, session, proto.Envelope{
		RequestID: "list-1",
		Action:    proto.ActionFileList,
		Payload:   proto.FileListPayload{Path: "/big"},
	}, 15*time.Second)
	if resp.Error == nil || resp.Error.Code != "response_too_large" {
		t.Fatalf("reply = %+v, want a response_too_large error", resp)
	}
	if resp.RequestID != "list-1" || resp.Action != proto.ActionFileList {
		t.Fatalf("error reply echoes request %q / %q, want list-1 / %s", resp.RequestID, resp.Action, proto.ActionFileList)
	}
	if !strings.Contains(resp.Error.Message, proto.ActionFileList) {
		t.Fatalf("error message %q does not name the action", resp.Error.Message)
	}
	if n := counted.read.Load(); n > 4096 {
		t.Fatalf("fallback reply was %d bytes, want a small envelope", n)
	}

	// The channel is still usable afterwards.
	if hello := callAgent(t, session, proto.Envelope{Action: "hello"}, 5*time.Second); hello.Error != nil {
		t.Fatalf("hello after the fallback = %+v", hello.Error)
	}
}

// TestReplyThatCannotMarshalIsReportedNotDropped: a reply encoding/json refuses
// (a NaN in a metrics snapshot) is reported rather than dropped.
func TestReplyThatCannotMarshalIsReportedNotDropped(t *testing.T) {
	session, _ := startRPCAgent(t, Server{MetricsCollector: nanMetrics{}})
	resp := callAgent(t, session, proto.Envelope{RequestID: "m-1", Action: "metrics.collect"}, 15*time.Second)
	if resp.Error == nil || resp.Error.Code != "response_encode_failed" {
		t.Fatalf("reply = %+v, want a response_encode_failed error", resp)
	}
	if resp.RequestID != "m-1" {
		t.Fatalf("error reply request id = %q, want m-1", resp.RequestID)
	}
}
