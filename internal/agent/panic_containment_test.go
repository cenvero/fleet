// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
	"golang.org/x/crypto/ssh"
)

// panicMetrics stands in for a handler bug reachable from a request.
type panicMetrics struct{}

func (panicMetrics) Collect(context.Context) (proto.MetricsSnapshot, error) {
	panic("metrics handler bug")
}

// fakeController is the controller end of a reverse connection: the SSH
// server the agent dials.
type fakeController struct {
	conn  *ssh.ServerConn
	chans <-chan ssh.NewChannel
}

// startFakeController completes the SSH handshake on conn as the controller.
// The channel yields nil if the handshake fails.
func startFakeController(conn net.Conn, hostKey ssh.Signer) <-chan *fakeController {
	out := make(chan *fakeController, 1)
	go func() {
		cfg := &ssh.ServerConfig{
			Config: ssh.Config{
				Ciphers:      transport.SupportedCiphers(),
				KeyExchanges: transport.SupportedKEX(),
				MACs:         transport.SupportedMACs(),
			},
			PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
				return &ssh.Permissions{}, nil
			},
		}
		cfg.AddHostKey(hostKey)
		sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
		if err != nil {
			out <- nil
			return
		}
		go ssh.DiscardRequests(reqs)
		out <- &fakeController{conn: sshConn, chans: chans}
	}()
	return out
}

func callOn(ch ssh.Channel, action string) (proto.Envelope, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return (&transport.Session{Mode: transport.ModeReverse, Channel: ch}).Call(ctx, proto.Envelope{Action: action})
}

// TestReverseInboundChannelPanicIsContained: a handler panic on a fleet-rpc
// channel the controller opened over a reverse connection must end only that
// channel, not the agent. It used to terminate the process.
func TestReverseInboundChannelPanicIsContained(t *testing.T) {
	agentConn, controllerConn := testutil.NewBufferedConnPair("127.0.0.1:55555", "127.0.0.1:9443")
	controllerReady := startFakeController(controllerConn, newTestHostSigner(t))

	clientConn, chans, reqs, err := ssh.NewClientConn(agentConn, "127.0.0.1:9443", &ssh.ClientConfig{
		Config: ssh.Config{
			Ciphers:      transport.SupportedCiphers(),
			KeyExchanges: transport.SupportedKEX(),
			MACs:         transport.SupportedMACs(),
		},
		User:            "rev",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(newTestHostSigner(t))},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // #nosec G106 -- in-memory test connection
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := ssh.NewClient(clientConn, chans, reqs)
	defer client.Close()
	controller := <-controllerReady
	if controller == nil {
		t.Fatal("controller handshake failed")
	}
	defer controller.conn.Close()
	go serveReverseInboundChannels(client.HandleChannelOpen(transport.RPCChannelType),
		Server{Mode: transport.ModeReverse, MetricsCollector: panicMetrics{}})

	call := func(action string) (proto.Envelope, error) {
		ch, chReqs, err := controller.conn.OpenChannel(transport.RPCChannelType, nil)
		if err != nil {
			t.Fatalf("open channel: %v", err)
		}
		go ssh.DiscardRequests(chReqs)
		defer ch.Close()
		return callOn(ch, action)
	}
	if _, err := call("metrics.collect"); err == nil {
		t.Fatal("a panicking handler produced a reply; want its channel closed")
	}
	// The agent is still serving: a new channel works.
	hello, err := call("hello")
	if err != nil || hello.Error != nil {
		t.Fatalf("hello after the contained panic = %+v, %v", hello.Error, err)
	}
}

// TestReverseSessionPanicIsContained: a handler panic on the reverse agent's
// own fleet-rpc channel ends that session, which the reconnect loop replaces,
// instead of terminating the agent. runReverseSession must return, so the loop
// can reconnect, and report the session as failed.
func TestReverseSessionPanicIsContained(t *testing.T) {
	dir := t.TempDir()
	hostKey := newTestHostSigner(t)
	agentConn, controllerConn := testutil.NewBufferedConnPair("127.0.0.1:55555", "127.0.0.1:9443")
	controllerReady := startFakeController(controllerConn, hostKey)
	opts := ReverseOptions{
		ControllerAddress:     "127.0.0.1:9443",
		ControllerFingerprint: ssh.FingerprintSHA256(hostKey.PublicKey()),
		ServerName:            "rev",
		KnownHostsPath:        filepath.Join(dir, "known_hosts"),
		NetworkDialContext: func(context.Context, string, string) (net.Conn, error) {
			return agentConn, nil
		},
	}
	server := Server{Mode: transport.ModeReverse, HostKeyPath: filepath.Join(dir, "agent_key"), MetricsCollector: panicMetrics{}}

	type sessionResult struct {
		authenticated bool
		err           error
	}
	ended := make(chan sessionResult, 1)
	go func() {
		authenticated, err := runReverseSession(context.Background(), opts, server, nil, nil)
		ended <- sessionResult{authenticated, err}
	}()

	controller := <-controllerReady
	if controller == nil {
		t.Fatal("controller handshake failed")
	}
	defer controller.conn.Close()
	var newChannel ssh.NewChannel
	select {
	case newChannel = <-controller.chans:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not open its fleet-rpc channel")
	}
	ch, chReqs, err := newChannel.Accept()
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(chReqs)
	if _, err := callOn(ch, "metrics.collect"); err == nil {
		t.Fatal("a panicking handler produced a reply; want the channel closed")
	}

	select {
	case res := <-ended:
		if !res.authenticated || res.err == nil {
			t.Fatalf("runReverseSession = %v, %v; want an authenticated session that ended with an error", res.authenticated, res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runReverseSession did not return after the contained panic")
	}
}

// TestShellExecPanicIsAnswered: a panic in the shell.exec worker goroutine
// must not terminate the agent, and the waiting controller must get an error
// reply rather than silence. The channel stays usable.
func TestShellExecPanicIsAnswered(t *testing.T) {
	session, _ := startRPCAgent(t, Server{shellExec: func(context.Context, proto.ExecPayload) (proto.ExecResult, error) {
		panic("exec worker bug")
	}})
	resp := callAgent(t, session, proto.Envelope{RequestID: "exec-p", Action: "shell.exec", Payload: proto.ExecPayload{Command: "true"}}, 5*time.Second)
	if resp.Error == nil || resp.Error.Code != "internal_error" || resp.RequestID != "exec-p" {
		t.Fatalf("reply = %+v, want an internal_error for exec-p", resp)
	}
	if hello := callAgent(t, session, proto.Envelope{Action: "hello"}, 5*time.Second); hello.Error != nil {
		t.Fatalf("hello after the contained panic = %+v", hello.Error)
	}
}
