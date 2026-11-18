// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
)

// Whether a failed call may have reached the server decides whether `fleet
// exec --retry` may try again. That answer has to survive the daemon in
// between: a direct call the daemon could not connect for, and a call for a
// reverse agent that is not connected, both certainly never ran.
func TestNotSentSurvivesTheDaemon(t *testing.T) {
	f := newRelayFleet(t)

	// The server stops answering: the daemon's dial fails.
	f.daemon.DisconnectPooledSessions()
	f.cli.DisconnectPooledSessions()
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := dead.Addr().(*net.TCPAddr).Port
	_ = dead.Close()
	for _, app := range []*App{f.cli, f.daemon} {
		if err := app.updateExistingServer("web", func(s *ServerRecord) bool { s.Port = deadPort; return true }); err != nil {
			t.Fatal(err)
		}
	}
	record, err := f.cli.GetServer("web")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = f.cli.callRPCContext(ctx, record, proto.Envelope{Action: "service.restart"})
	if err == nil {
		t.Fatal("a call to a closed port succeeded")
	}
	if !transport.RequestNotSent(err) {
		t.Fatalf("relayed dial failure %v: RequestNotSent = false, want true so --retry may retry it", err)
	}
	if f.cli.sessions.has("web") {
		t.Fatal("the CLI dialled the server itself; the test did not go through the daemon")
	}

	// A reverse agent that is not connected to the daemon's hub.
	if err := f.cli.AddServer(ServerRecord{Name: "edge", Address: "unknown", Mode: transport.ModeReverse}); err != nil {
		t.Fatal(err)
	}
	edge, err := f.cli.GetServer("edge")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.cli.callRPCContext(ctx, edge, proto.Envelope{Action: "shell.exec", Payload: proto.ExecPayload{Command: "true"}})
	if err == nil || !transport.RequestNotSent(err) {
		t.Fatalf("call for a disconnected reverse agent: %v, RequestNotSent = %v; want true", err, transport.RequestNotSent(err))
	}
}
