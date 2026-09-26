// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
)

// impostorBehaviour is how a fake "daemon" answers the caller's first line.
type impostorBehaviour int

const (
	// impostorLegacy answers like a daemon from before mutual authentication
	// (or anything pretending to be one): "unauthorized".
	impostorLegacy impostorBehaviour = iota
	// impostorForgedChallenge answers with a well-formed challenge whose proof
	// it cannot actually compute, then forges a response to anything else.
	impostorForgedChallenge
	// impostorHangUp closes the connection without a word.
	impostorHangUp
)

// impostorListener binds address and records every byte any caller sends it.
type impostorListener struct {
	ln       net.Listener
	mu       sync.Mutex
	received bytes.Buffer
	conns    atomic.Int32
}

func startImpostor(t *testing.T, address string, behaviour impostorBehaviour) *impostorListener {
	t.Helper()
	ln, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("impostor could not bind %s: %v", address, err)
	}
	imp := &impostorListener{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			imp.conns.Add(1)
			go imp.serve(conn, behaviour)
		}
	}()
	return imp
}

func (imp *impostorListener) serve(conn net.Conn, behaviour impostorBehaviour) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	record := func(p []byte) {
		imp.mu.Lock()
		imp.received.Write(p)
		imp.mu.Unlock()
	}
	reader := io.TeeReader(conn, recordingWriter(record))
	dec := json.NewDecoder(reader)
	var first map[string]any
	if err := dec.Decode(&first); err != nil {
		return
	}
	switch behaviour {
	case impostorHangUp:
		return
	case impostorLegacy:
		_, _ = fmt.Fprintln(conn, `{"error":{"code":"unauthorized","message":"invalid control token"}}`)
		return
	case impostorForgedChallenge:
		_, _ = fmt.Fprintf(conn, `{"auth":%q,"server_nonce":%q,"proof":%q,"capabilities":["binary-frame","direct-call"]}`+"\n",
			controlAuthVersion, strings.Repeat("ab", controlNonceBytes), strings.Repeat("cd", 32))
		// Anything that follows gets a forged answer.
		var next map[string]any
		if err := dec.Decode(&next); err != nil {
			return
		}
		_, _ = fmt.Fprintln(conn, `{"response":{"type":"response","action":"shell.exec","payload":{"stdout":"FORGED","stderr":"","exit_code":0}}}`)
		_, _ = io.Copy(io.Discard, reader)
	}
}

func (imp *impostorListener) seen() []byte {
	imp.mu.Lock()
	defer imp.mu.Unlock()
	return append([]byte(nil), imp.received.Bytes()...)
}

type recordingWriter func([]byte)

func (f recordingWriter) Write(p []byte) (int, error) { f(p); return len(p), nil }

// assertOnlyAuthHellos checks that everything an impostor received was auth
// hellos: no token, no envelope, no command, no secret.
func assertOnlyAuthHellos(t *testing.T, seen []byte, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && bytes.Contains(seen, []byte(secret)) {
			t.Fatalf("impostor received %q: %s", secret, seen)
		}
	}
	for _, marker := range []string{`"token"`, `"envelope"`, `"client_proof"`, "shell.exec", `"type"`} {
		if bytes.Contains(seen, []byte(marker)) {
			t.Fatalf("impostor received %s: %s", marker, seen)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(seen))
	for {
		var hello map[string]any
		if err := dec.Decode(&hello); err == io.EOF {
			return
		} else if err != nil {
			t.Fatalf("impostor received something other than JSON auth hellos: %q", seen)
		}
		if hello["auth"] != controlAuthVersion || len(hello) != 2 {
			t.Fatalf("impostor received a message that is not an auth hello: %v", hello)
		}
	}
}

// TestDirectCallsNeverReachAnImpostorDaemon is the regression for the CLI
// talking to whatever listens on the control address. The daemon is down but
// its control token is still on disk (it crashed); another local user has
// bound the control address. A direct-mode exec carrying a secret must reveal
// nothing to that process — not the token, not the envelope — and must run by
// dialling the server directly, never returning the impostor's forged output.
func TestDirectCallsNeverReachAnImpostorDaemon(t *testing.T) {
	for _, tc := range []struct {
		name      string
		behaviour impostorBehaviour
	}{
		{"answers like an old daemon", impostorLegacy},
		{"forges a challenge", impostorForgedChallenge},
		{"hangs up", impostorHangUp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelayFleet(t)
			address := f.cli.Config.Runtime.ControlAddress
			f.stopCtl() // the daemon is gone, its token file is not
			token, err := readControlTokenFile(f.cli.controlTokenPath())
			if err != nil {
				t.Fatal(err)
			}
			imp := startImpostor(t, address, tc.behaviour)
			directRelayUnreachable.Delete(address)

			result, err := f.cli.ExecCommandContext(context.Background(), "web", "DB_PASSWORD=hunter2; printf real-%s \"$DB_PASSWORD\"")
			if err != nil {
				t.Fatalf("exec with an impostor on the control address: %v", err)
			}
			if result.Stdout != "real-hunter2" {
				t.Fatalf("exec returned %q; want the server's own output, not the impostor's", result.Stdout)
			}
			if imp.conns.Load() == 0 {
				t.Fatal("test did not exercise the impostor")
			}
			assertOnlyAuthHellos(t, imp.seen(), token, "hunter2", "DB_PASSWORD")
			if !f.cli.sessions.has("web") {
				t.Fatal("the CLI should have dialled the server itself")
			}
		})
	}
}

// Reverse-mode calls have no route but the daemon. With a token written by a
// daemon that authenticates itself, an impostor gets nothing: no legacy
// downgrade, no token, no envelope, and the call fails rather than returning
// forged data.
func TestReverseCallsNeverDowngradeForMutualAuthToken(t *testing.T) {
	for _, tc := range []struct {
		name      string
		behaviour impostorBehaviour
	}{
		{"answers like an old daemon", impostorLegacy},
		{"forges a challenge", impostorForgedChallenge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configDir := t.TempDir()
			app := &App{ConfigDir: configDir}
			token, err := app.generateControlToken()
			if err != nil {
				t.Fatal(err)
			}
			if !tokenRequiresMutualAuth(token) {
				t.Fatalf("daemon token %q does not announce mutual authentication", token)
			}
			app.Config.Runtime.ControlAddress = freeLoopbackAddress(t)
			imp := startImpostor(t, app.Config.Runtime.ControlAddress, tc.behaviour)

			_, err = app.callReverseControlContext(context.Background(), "rev", proto.Envelope{Action: "shell.exec", Payload: proto.ExecPayload{Command: "echo hunter2"}})
			if err == nil {
				t.Fatal("a reverse call through an impostor must fail")
			}
			if _, err := app.callReverseStatus("rev"); err == nil {
				t.Fatal("a status call through an impostor must fail")
			}
			assertOnlyAuthHellos(t, imp.seen(), token, "hunter2")
		})
	}
}

// The daemon side of mutual authentication: a caller that cannot prove it
// holds the token is refused before anything is executed.
func TestControlDaemonRejectsWrongClientProof(t *testing.T) {
	app, hub := controlBenchFleet(t, nil)
	var executed atomic.Int32
	client, agentSide := newPipeChannelPair()
	go func() {
		defer agentSide.Close()
		for {
			req, err := proto.Decode(agentSide)
			if err != nil {
				return
			}
			executed.Add(1)
			_ = proto.Encode(agentSide, proto.Envelope{Type: proto.EnvelopeTypeResponse, RequestID: req.RequestID, Action: req.Action})
		}
	}()
	defer client.Close()
	hub.mu.Lock()
	hub.sessions["counted"] = newReverseSession(&transport.Session{Mode: transport.ModeReverse, Channel: client}, nil, ReverseSessionInfo{Server: "counted"})
	hub.mu.Unlock()

	for name, proof := range map[string]func(serverNonce, clientNonce []byte) string{
		"wrong token":  func(s, c []byte) string { return hex.EncodeToString(controlClientProof("not-the-token", s, c)) },
		"daemon proof": func(s, c []byte) string { return hex.EncodeToString(controlDaemonProof("bench-control-token", c, s)) },
		"replayed":     func(s, c []byte) string { return hex.EncodeToString(controlClientProof("bench-control-token", c, s)) },
		"empty":        func(s, c []byte) string { return "" },
	} {
		conn, err := net.Dial("tcp", app.Config.Runtime.ControlAddress)
		if err != nil {
			t.Fatal(err)
		}
		clientNonce := bytes.Repeat([]byte{1}, controlNonceBytes)
		_, _ = fmt.Fprintf(conn, `{"auth":%q,"client_nonce":%q}`+"\n", controlAuthVersion, hex.EncodeToString(clientNonce))
		dec := json.NewDecoder(conn)
		var challenge controlAuthChallenge
		if err := dec.Decode(&challenge); err != nil {
			t.Fatal(err)
		}
		serverNonce, _ := decodeControlNonce(challenge.ServerNonce)
		req, _ := json.Marshal(reverseControlRequest{ClientProof: proof(serverNonce, clientNonce), Type: "call", Server: "counted",
			Envelope: proto.Envelope{Action: "metrics.collect"}})
		_, _ = conn.Write(append(req, '\n'))
		var resp reverseControlResponse
		if err := json.NewDecoder(io.MultiReader(dec.Buffered(), conn)).Decode(&resp); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_ = conn.Close()
		if resp.Error == nil || resp.Error.Code != "unauthorized" {
			t.Fatalf("%s: daemon answered %+v, want unauthorized", name, resp)
		}
	}
	if n := executed.Load(); n != 0 {
		t.Fatalf("%d unauthenticated requests reached the agent", n)
	}
	// The genuine article still works.
	if _, err := app.callReverseControlContext(context.Background(), "counted", proto.Envelope{Action: "metrics.collect"}); err != nil {
		t.Fatalf("authenticated call: %v", err)
	}
	if executed.Load() != 1 {
		t.Fatal("authenticated call did not reach the agent")
	}
}

// Before a caller has authenticated, the daemon reads a few KiB at most — not
// the full 32 MiB request limit — whether the caller opens with an auth hello
// or a legacy request.
func TestControlPreAuthReadIsSmall(t *testing.T) {
	huge := strings.Repeat("x", 1<<20)
	for name, payload := range map[string]string{
		"oversized auth hello":   `{"auth":"` + huge + `"}`,
		"oversized legacy token": `{"token":"` + huge + `"}`,
		"wrong legacy token":     `{"token":"wrong","type":"call","server":"s","envelope":{"action":"x","payload":"` + huge + `"}}`,
		"no credentials first":   `{"type":"call","server":"s","envelope":{"action":"x","payload":"` + huge + `"}}`,
	} {
		conn := &boundedControlConn{reader: bytes.NewReader([]byte(payload))}
		before := conn.reader.Len()
		NewReverseHub(&App{}, "token").handleControlConn(conn)
		read := before - conn.reader.Len()
		if read > 2*maxControlPreAuthBytes {
			t.Fatalf("%s: daemon read %d bytes before authenticating the caller", name, read)
		}
		if !bytes.Contains(conn.writer.Bytes(), []byte(`"error"`)) {
			t.Fatalf("%s: no error reply: %q", name, conn.writer.String())
		}
		if len(conn.deadlines) == 0 || time.Until(conn.deadlines[0]) > controlAuthTimeout+time.Second {
			t.Fatalf("%s: the unauthenticated phase must have a short deadline, got %v", name, conn.deadlines)
		}
	}
}

// Requests newer than mutual authentication are not accepted with the legacy
// raw token, even a correct one.
func TestControlLegacyTokenOnlyServesLegacyRequests(t *testing.T) {
	app, _ := controlBenchFleet(t, nil)
	for _, kind := range []string{controlTypeCallDirect, controlTypeCallFramed, "hello"} {
		conn, err := net.Dial("tcp", app.Config.Runtime.ControlAddress)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := json.Marshal(reverseControlRequest{Token: "bench-control-token", Type: kind, Server: "bench"})
		_, _ = conn.Write(append(req, '\n'))
		var resp reverseControlResponse
		if err := json.NewDecoder(conn).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
		if resp.Error == nil || resp.Error.Code != "unsupported_action" {
			t.Fatalf("legacy %s request answered %+v, want unsupported_action", kind, resp)
		}
	}
}

// When even the waiting queue is full the daemon answers "busy" instead of
// closing silently: a direct-mode call then dials the server itself (nothing
// was sent), and a reverse call gets a clear error rather than EOF.
func TestControlFullQueueAnswersBusy(t *testing.T) {
	f := newRelayFleet(t)
	address := f.cli.Config.Runtime.ControlAddress
	var idle []net.Conn
	defer func() {
		for _, c := range idle {
			_ = c.Close()
		}
	}()
	for i := 0; i < maxConcurrentControlConnections+maxQueuedControlConnections; i++ {
		conn, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		idle = append(idle, conn)
	}
	time.Sleep(200 * time.Millisecond) // every slot and queue place taken

	server, err := f.cli.GetServer("web")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, handled, err := f.cli.tryDaemonDirectRelay(context.Background(), server, proto.Envelope{Action: "service.list"})
	if handled {
		t.Fatalf("a saturated daemon's refusal was treated as handled (err=%v); the call would fail instead of falling back", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("falling back from a saturated daemon took %s", elapsed)
	}
	directRelayUnreachable.Delete(address)
	_, err = f.cli.callReverseControlContext(context.Background(), "rev", proto.Envelope{Action: "metrics.collect"})
	var respErr *controlResponseError
	if !errors.As(err, &respErr) || respErr.Code != "control_busy" {
		t.Fatalf("reverse call against a saturated daemon = %v, want control_busy", err)
	}
}

// The daemon removes its control token when it stops, unless another daemon
// has written its own since.
func TestDaemonRemovesItsControlTokenOnShutdown(t *testing.T) {
	run := func(t *testing.T, replace bool) {
		app := replayTestApp(t, "unused")
		app.Config.Runtime.ListenAddress = freeLoopbackAddress(t)
		app.Config.Runtime.ControlAddress = freeLoopbackAddress(t)
		app.Config.Runtime.MetricsPollInterval = ""
		ctx, stop := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- app.RunDaemon(ctx) }()
		tokenPath := app.controlTokenPath()
		waitForTestPath(t, tokenPath)
		token, err := readControlTokenFile(tokenPath)
		if err != nil || !tokenRequiresMutualAuth(token) {
			t.Fatalf("daemon token = %q, %v", token, err)
		}
		if replace {
			if err := os.WriteFile(tokenPath, []byte("someone-elses-token"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		stop()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("daemon did not stop")
		}
		_, statErr := os.Stat(tokenPath)
		if replace && statErr != nil {
			t.Fatal("the daemon removed a token that was not its own")
		}
		if !replace && !os.IsNotExist(statErr) {
			t.Fatalf("the daemon left its control token behind: %v", statErr)
		}
	}
	t.Run("own token", func(t *testing.T) { run(t, false) })
	t.Run("replaced token", func(t *testing.T) { run(t, true) })
}
