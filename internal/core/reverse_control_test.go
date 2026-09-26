// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

func randomChunk(t testing.TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func uploadEnvelope(chunk []byte) proto.Envelope {
	return proto.DetachBinary(proto.Envelope{Action: proto.ActionFileWrite,
		Payload: &proto.FileWritePayload{TransferID: "t", Path: "/p", Data: chunk, SHA256: "s"}})
}

// TestControlSocketQueuesExcessConnections is the regression for the daemon
// closing control connections beyond its handler slots: the caller saw a bare
// EOF, which reads as "server offline". Excess connections now wait.
func TestControlSocketQueuesExcessConnections(t *testing.T) {
	app, _ := controlBenchFleet(t, nil)
	var idle []net.Conn
	for i := 0; i < maxConcurrentControlConnections; i++ {
		conn, err := net.Dial("tcp", app.Config.Runtime.ControlAddress)
		if err != nil {
			t.Fatal(err)
		}
		idle = append(idle, conn) // holds a read slot without sending anything
	}
	time.Sleep(50 * time.Millisecond) // let the daemon hand every slot out

	result := make(chan error, 1)
	go func() {
		_, err := app.callReverseControlContext(context.Background(), "bench", proto.Envelope{Action: "metrics.collect"})
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("call finished while every slot was busy (err=%v): it was dropped, not queued", err)
	case <-time.After(150 * time.Millisecond):
	}
	_ = idle[0].Close() // one slot frees up
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("queued call failed once a slot freed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued call never got a slot")
	}
	for _, c := range idle[1:] {
		_ = c.Close()
	}
}

func TestControlSocketAnswersBusyInsteadOfDropping(t *testing.T) {
	restore := controlSlotWait
	controlSlotWait = 50 * time.Millisecond
	defer func() { controlSlotWait = restore }()

	app, _ := controlBenchFleet(t, nil)
	var idle []net.Conn
	for i := 0; i < maxConcurrentControlConnections; i++ {
		conn, err := net.Dial("tcp", app.Config.Runtime.ControlAddress)
		if err != nil {
			t.Fatal(err)
		}
		idle = append(idle, conn)
	}
	defer func() {
		for _, c := range idle {
			_ = c.Close()
		}
	}()
	time.Sleep(50 * time.Millisecond)
	_, err := app.callReverseControlContext(context.Background(), "bench", proto.Envelope{Action: "metrics.collect"})
	var respErr *controlResponseError
	if !errors.As(err, &respErr) || respErr.Code != "control_busy" {
		t.Fatalf("saturated control socket returned %v; want a control_busy answer, not EOF", err)
	}
}

// Slow calls must not starve other control requests: the read slot is handed
// back once a request is authenticated.
func TestControlCallsDoNotHoldReadSlots(t *testing.T) {
	app, _ := controlBenchFleet(t, nil)
	const slow = maxConcurrentControlConnections + 8
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < slow; i++ {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := app.callReverseControlContext(ctx, "bench", proto.Envelope{Action: "test.sleep"}); err != nil {
				failures.Add(1)
				t.Logf("slow call: %v", err)
			}
		})
	}
	time.Sleep(100 * time.Millisecond) // all slow calls are in flight
	start := time.Now()
	info, err := app.callReverseStatus("bench")
	if err != nil || info.Server != "bench" {
		t.Fatalf("status during %d slow calls = %+v, err=%v", slow, info, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("status waited %s behind slow calls", elapsed)
	}
	wg.Wait()
	if n := failures.Load(); n != 0 {
		t.Fatalf("%d of %d slow calls failed", n, slow)
	}
}

// The token is read from disk once per process, and re-read only when the
// daemon rejects it (it mints a new one every time it starts).
func TestControlTokenIsCachedAndRefreshedAfterDaemonRestart(t *testing.T) {
	configDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(configDir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(configDir, "data", "control.token")
	if err := os.WriteFile(tokenPath, []byte("ma1-token-one"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	app := &App{ConfigDir: configDir}
	app.Config.Runtime.ControlAddress = address

	serve := func(listener net.Listener, token string) (*ReverseHub, context.CancelFunc, chan struct{}) {
		hub := NewReverseHub(app, token)
		client, agentSide := newPipeChannelPair()
		go fakeEchoAgent(agentSide, nil)
		hub.sessions["bench"] = newReverseSession(&transport.Session{Mode: transport.ModeReverse, Channel: client}, nil, ReverseSessionInfo{Server: "bench"})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = hub.ServeControl(ctx, listener) }()
		return hub, func() { cancel(); _ = client.Close() }, done
	}
	_, stop1, done1 := serve(listener, "ma1-token-one")
	call := func() error {
		_, err := app.callReverseControlContext(context.Background(), "bench", proto.Envelope{Action: "metrics.collect"})
		return err
	}
	if err := call(); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Garbage on disk is never read while the cached token works.
	if err := os.WriteFile(tokenPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := call(); err != nil {
		t.Fatalf("call with cached token after the file changed: %v", err)
	}

	// Restart the daemon with a new token.
	stop1()
	<-done1
	listener2, err := net.Listen("tcp", address)
	if err != nil {
		t.Skipf("could not re-listen on %s: %v", address, err)
	}
	if err := os.WriteFile(tokenPath, []byte("ma1-token-two"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stop2, done2 := serve(listener2, "ma1-token-two")
	defer func() { stop2(); <-done2 }()
	if err := call(); err != nil {
		t.Fatalf("call after the daemon restarted with a new token: %v", err)
	}
	if info, err := app.callReverseStatus("bench"); err != nil || info.Server != "bench" {
		t.Fatalf("status after restart: %+v, %v", info, err)
	}
}

// TestControlBinaryFramingRoundTrip: attachments cross the control socket raw
// in both directions against a current daemon.
func TestControlBinaryFramingRoundTrip(t *testing.T) {
	chunk := randomChunk(t, 1<<20)
	app, _ := controlBenchFleet(t, chunk)

	resp, err := app.callReverseControlContext(context.Background(), "bench", uploadEnvelope(chunk))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	written, err := proto.DecodePayload[proto.FileWriteResult](resp.Payload)
	if err != nil || written.BytesWritten != int64(len(chunk)) {
		t.Fatalf("agent received %d bytes (err=%v), want %d", written.BytesWritten, err, len(chunk))
	}

	down, err := app.callReverseControlContext(context.Background(), "bench",
		proto.Envelope{Action: proto.ActionFileRead, Payload: proto.FileReadPayload{Path: "/p", Length: int64(len(chunk)), Binary: true}})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if !bytes.Equal(down.Binary, chunk) {
		t.Fatalf("download returned %d bytes, want the %d-byte chunk intact", len(down.Binary), len(chunk))
	}
}

// The authenticated, framed wire format, spelled out: auth hello, the daemon's
// challenge and proof, then the request (client proof first) as a JSON line
// followed by the raw attachment.
func TestControlFramedWireFormat(t *testing.T) {
	chunk := randomChunk(t, 4096)
	app, _ := controlBenchFleet(t, chunk)
	conn, err := net.Dial("tcp", app.Config.Runtime.ControlAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	const token = benchControlToken
	clientNonce := bytes.Repeat([]byte{7}, controlNonceBytes)
	if _, err := fmt.Fprintf(conn, `{"auth":%q,"client_nonce":%q}`+"\n", controlAuthVersion, hex.EncodeToString(clientNonce)); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(conn)
	var challenge controlAuthChallenge
	if err := dec.Decode(&challenge); err != nil {
		t.Fatal(err)
	}
	serverNonce, ok := decodeControlNonce(challenge.ServerNonce)
	proof, _ := hex.DecodeString(challenge.Proof)
	if !ok || !hmac.Equal(proof, controlDaemonProof(token, clientNonce, serverNonce)) {
		t.Fatalf("daemon did not prove it holds the token: %+v", challenge)
	}
	if !slices.Contains(challenge.Capabilities, controlCapBinaryFrame) {
		t.Fatalf("daemon capabilities %v lack %s", challenge.Capabilities, controlCapBinaryFrame)
	}
	n := len(chunk)
	req := reverseControlRequest{ClientProof: hex.EncodeToString(controlClientProof(token, serverNonce, clientNonce)),
		Type: controlTypeCallFramed, Server: "bench",
		Envelope: uploadEnvelope(chunk), Accept: []string{controlCapBinaryFrame}, BinaryLength: &n}
	header, _ := json.Marshal(req)
	if bytes.Contains(header, []byte("envelope_binary")) || bytes.Contains(header, []byte(token)) {
		t.Fatal("a framed request must carry neither the token nor the attachment as base64")
	}
	if !bytes.HasPrefix(header, []byte(`{"client_proof":`)) {
		t.Fatalf("the client proof must be the first member: %s", header[:40])
	}
	if _, err := conn.Write(append(append(header, '\n'), chunk...)); err != nil {
		t.Fatal(err)
	}
	var resp reverseControlResponse
	if err := json.NewDecoder(io.MultiReader(dec.Buffered(), conn)).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("framed request rejected: %+v", resp.Error)
	}
	written, _ := proto.DecodePayload[proto.FileWriteResult](resp.Response.Payload)
	if written.BytesWritten != int64(n) {
		t.Fatalf("daemon relayed %d attachment bytes, want %d", written.BytesWritten, n)
	}
}

// An older CLI never sends Accept or BinaryLength; a newer daemon must answer
// it exactly as before, attachment base64 in response_binary.
func TestControlOldClientGetsBase64FromNewDaemon(t *testing.T) {
	chunk := randomChunk(t, 64<<10)
	app, _ := controlBenchFleet(t, chunk)
	conn, err := net.Dial("tcp", app.Config.Runtime.ControlAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The request exactly as the previous release built it.
	type oldRequest struct {
		Token          string         `json:"token"`
		Type           string         `json:"type"`
		Server         string         `json:"server"`
		Envelope       proto.Envelope `json:"envelope,omitempty"`
		EnvelopeBinary []byte         `json:"envelope_binary,omitempty"`
	}
	if err := json.NewEncoder(conn).Encode(oldRequest{Token: benchControlToken, Type: "call", Server: "bench",
		Envelope: proto.Envelope{Action: proto.ActionFileRead, Payload: proto.FileReadPayload{Path: "/p", Binary: true}}}); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("binary_length")) {
		t.Fatal("a newer daemon sent a framed response to a client that did not ask for one")
	}
	var resp struct {
		Response       *proto.Envelope `json:"response"`
		ResponseBinary []byte          `json:"response_binary"`
		Error          *proto.Error    `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &resp); err != nil {
		t.Fatalf("old client could not parse the response: %v (%q...)", err, raw[:min(len(raw), 80)])
	}
	if resp.Error != nil || !bytes.Equal(resp.ResponseBinary, chunk) {
		t.Fatalf("old client got error=%+v and %d attachment bytes, want %d", resp.Error, len(resp.ResponseBinary), len(chunk))
	}
}

// legacyControlServer mimics the control socket of a daemon from before
// binary framing: one JSON request, trailing-data check, token check, the
// original request types, base64 attachments.
func legacyControlServer(t *testing.T, token string, handle func(proto.Envelope) proto.Envelope) (address string, requests *atomic.Int32) {
	t.Helper()
	type oldRequest struct {
		Token          string         `json:"token"`
		Type           string         `json:"type"`
		Server         string         `json:"server"`
		Envelope       proto.Envelope `json:"envelope,omitempty"`
		EnvelopeBinary []byte         `json:"envelope_binary,omitempty"`
	}
	type oldResponse struct {
		Response       *proto.Envelope `json:"response,omitempty"`
		ResponseBinary []byte          `json:"response_binary,omitempty"`
		Error          *proto.Error    `json:"error,omitempty"`
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	requests = &atomic.Int32{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				requests.Add(1)
				dec := json.NewDecoder(conn)
				var req oldRequest
				reply := func(r oldResponse) { _ = json.NewEncoder(conn).Encode(r) }
				if err := dec.Decode(&req); err != nil {
					reply(oldResponse{Error: &proto.Error{Code: "decode_error", Message: "invalid control request"}})
					return
				}
				buffered, _ := io.ReadAll(dec.Buffered())
				if len(strings.TrimSpace(string(buffered))) != 0 {
					reply(oldResponse{Error: &proto.Error{Code: "request_too_large", Message: "trailing data"}})
					return
				}
				if req.Token != token {
					reply(oldResponse{Error: &proto.Error{Code: "unauthorized", Message: "invalid control token"}})
					return
				}
				if req.Type != "call" {
					reply(oldResponse{Error: &proto.Error{Code: "unsupported_action", Message: "control request " + req.Type + " is not supported"}})
					return
				}
				env := req.Envelope
				if req.EnvelopeBinary != nil {
					env.Binary = req.EnvelopeBinary
				}
				out := handle(env)
				reply(oldResponse{Response: &out, ResponseBinary: out.Binary})
			}()
		}
	}()
	return ln.Addr().String(), requests
}

func legacyDaemonApp(t *testing.T, chunk []byte) (*App, *atomic.Int32, *atomic.Int64) {
	t.Helper()
	configDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(configDir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "data", "control.token"), []byte("legacy-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	var received atomic.Int64
	address, requests := legacyControlServer(t, "legacy-token", func(env proto.Envelope) proto.Envelope {
		out := proto.Envelope{Type: proto.EnvelopeTypeResponse, Action: env.Action}
		switch env.Action {
		case proto.ActionFileWrite:
			received.Store(int64(len(env.Binary)))
			out.Payload = proto.FileWriteResult{BytesWritten: int64(len(env.Binary))}
		case proto.ActionFileRead:
			out.Binary = chunk
			out.Payload = proto.FileReadResult{Length: int64(len(chunk))}
		}
		return out
	})
	app := &App{ConfigDir: configDir}
	app.Config.Runtime.ControlAddress = address
	return app, requests, &received
}

// A newer CLI against an older daemon: the daemon rejects the auth hello, so
// (for a reverse call, with a token that did not come from a mutually
// authenticating daemon) the request goes the legacy way, attachment base64.
func TestControlNewClientFallsBackForOldDaemon(t *testing.T) {
	chunk := randomChunk(t, 256<<10)
	app, _, received := legacyDaemonApp(t, chunk)

	resp, err := app.callReverseControlContext(context.Background(), "srv", uploadEnvelope(chunk))
	if err != nil {
		t.Fatalf("upload through an older daemon: %v", err)
	}
	if received.Load() != int64(len(chunk)) {
		t.Fatalf("older daemon received %d attachment bytes, want %d", received.Load(), len(chunk))
	}
	if written, _ := proto.DecodePayload[proto.FileWriteResult](resp.Payload); written.BytesWritten != int64(len(chunk)) {
		t.Fatalf("unexpected response payload %+v", written)
	}
	down, err := app.callReverseControlContext(context.Background(), "srv", proto.Envelope{Action: proto.ActionFileRead})
	if err != nil || !bytes.Equal(down.Binary, chunk) {
		t.Fatalf("download through an older daemon: %d bytes, err=%v", len(down.Binary), err)
	}
}

// TestDaemonPollerUsesHubInProcess is the regression for the daemon reaching
// its own reverse agents through its own control socket: with the control
// token file gone (so any loopback control call would fail), the daemon's
// metrics poller must still collect from the reverse agent.
func TestDaemonPollerUsesHubInProcess(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeReverse,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly}); err != nil {
		t.Fatal(err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err := app.AddServer(ServerRecord{Name: "rev", Address: "unknown", Mode: transport.ModeReverse,
		User: "cenvero-agent", EnrollSecret: testReverseEnroll}); err != nil {
		t.Fatal(err)
	}
	app.Config.Runtime.ListenAddress = freeLoopbackAddress(t)
	app.Config.Runtime.ControlAddress = freeLoopbackAddress(t)
	app.Config.Runtime.MetricsPollInterval = "100ms"
	fingerprint := testControllerFingerprint(t, app)
	reverseAddress := app.Config.Runtime.ListenAddress

	ctx, stop := context.WithCancel(context.Background())
	daemonDone := make(chan error, 1)
	go func() { daemonDone <- app.RunDaemon(ctx) }()
	tokenPath := filepath.Join(configDir, "data", "control.token")
	waitForTestPath(t, tokenPath)

	agentCtx, stopAgent := context.WithCancel(context.Background())
	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		_ = agent.RunReverse(agentCtx, agent.ReverseOptions{
			EnrollToken: testReverseEnroll, ControllerFingerprint: fingerprint,
			ControllerAddress: reverseAddress, ServerName: "rev",
			KnownHostsPath: filepath.Join(t.TempDir(), "known_hosts"),
			MinRetryDelay:  20 * time.Millisecond, MaxRetryDelay: 100 * time.Millisecond,
		}, agent.Server{Mode: transport.ModeReverse, HostKeyPath: filepath.Join(t.TempDir(), "agent_key")})
	}()
	defer func() {
		stopAgent()
		<-agentDone
		stop()
		<-daemonDone
	}()

	// Wait until the agent is registered, then take the token away.
	deadline := time.Now().Add(10 * time.Second)
	for {
		record, err := app.GetServer("rev")
		if err == nil && record.Observed.Reachable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reverse agent never registered with the daemon")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.Remove(tokenPath); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	for {
		entries, err := app.MetricsDB.ListMetricSnapshots("rev", 1)
		if err == nil && len(entries) > 0 && !entries[0].Timestamp.Before(before.Add(-time.Second)) {
			return // collected in-process, without the control socket
		}
		if time.Now().After(before.Add(5 * time.Second)) {
			t.Fatal("daemon poller could not collect from its reverse agent without the control socket")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func freeLoopbackAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	_ = ln.Close()
	return address
}

// TestReverseCallsBeyondChannelLanesWait: more concurrent calls than a
// multiplexed session has channels used to pile onto the agent-opened channel
// and run one at a time; they now wait for the next free channel.
func TestReverseCallsBeyondChannelLanesWait(t *testing.T) {
	app := replayTestApp(t, "lanes")
	hub := NewReverseHub(app, "test-token")
	app.ReverseRPCContext = hub.CallContext
	stop := connectReverseAgent(t, app, hub, "lanes", agent.Server{Mode: transport.ModeReverse,
		HostKeyPath: filepath.Join(t.TempDir(), "agent_key")})
	defer stop()
	waitForReverseSession(t, hub, "lanes")

	const calls = 3 * (maxReverseExtraChannels + 1)
	var wg sync.WaitGroup
	var failures atomic.Int32
	start := time.Now()
	for i := 0; i < calls; i++ {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := app.ExecCommandContext(ctx, "lanes", "sleep 0.2"); err != nil {
				failures.Add(1)
				t.Logf("exec: %v", err)
			}
		})
	}
	wg.Wait()
	elapsed := time.Since(start)
	t.Logf("%d concurrent 200ms calls over %d channel lanes took %s", calls, maxReverseExtraChannels+1, elapsed)
	if failures.Load() != 0 {
		t.Fatalf("%d of %d calls failed", failures.Load(), calls)
	}
	// Three rounds of 200ms; serialising the overflow on one channel took
	// (calls-16)*200ms ≈ 7s.
	if elapsed > 4*time.Second {
		t.Fatalf("calls beyond the channel count were serialised: %s", elapsed)
	}
}
