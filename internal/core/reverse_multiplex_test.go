// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

// reverseTransferRig stands up a controller hub and a connected reverse agent,
// the way a real deployment does, so transfers can be exercised over the reverse
// path rather than only direct mode.
func reverseTransferRig(t *testing.T) (*App, *ReverseHub, context.CancelFunc) {
	t.Helper()
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeReverse,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	if err := app.AddServer(ServerRecord{
		Name: "reverse-node", Address: "unknown", Mode: transport.ModeReverse,
		User: "cenvero-agent", EnrollSecret: testReverseEnroll,
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	hub := NewReverseHub(app, "test-token")
	t.Cleanup(hub.Close)
	app.ReverseRPC = hub.Call
	app.ReverseStatusLookup = hub.Status

	reverseServer := agent.Server{
		Mode:        transport.ModeReverse,
		HostKeyPath: filepath.Join(t.TempDir(), "agent_reverse_key"),
	}
	clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:41100", "127.0.0.1:9543")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = hub.ServeConn(serverConn) }()
	go func() {
		_ = agent.RunReverse(ctx, agent.ReverseOptions{
			EnrollToken:       testReverseEnroll,
			ControllerAddress: "127.0.0.1:9543",
			ServerName:        "reverse-node",
			KnownHostsPath:    filepath.Join(t.TempDir(), "controller_known_hosts"),
			NetworkDialContext: func(context.Context, string, string) (net.Conn, error) {
				return clientConn, nil
			},
		}, reverseServer)
	}()
	waitForReverseSession(t, hub, "reverse-node")
	t.Cleanup(func() { cancel(); _ = clientConn.Close() })
	return app, hub, cancel
}

// TestReverseTransferUsesMultipleChannels is the point of reverse multiplexing:
// a reverse transfer must be able to run more than one stream. Previously the
// hub held a single mutex-serialised channel, so every chunk queued behind the
// last one no matter how many streams were configured.
func TestReverseTransferUsesMultipleChannels(t *testing.T) {
	app, hub, _ := reverseTransferRig(t)

	// The agent must have advertised the capability for the controller to try.
	server, err := app.GetServer("reverse-node")
	if err != nil {
		t.Fatal(err)
	}
	if !serverSupportsReverseMultiplex(server) {
		t.Fatal("reverse agent did not advertise CapabilityReverseMultiplex")
	}
	// And the transfer planner must no longer clamp reverse mode to one stream.
	if got := app.resolveTransferOptions(server, FileTransferOptions{Parallel: 4}); got.Parallel != 4 {
		t.Fatalf("reverse transfer clamped to %d streams; multiplexing should allow 4", got.Parallel)
	}

	payload := make([]byte, 3<<20+919) // several chunks plus a partial tail
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "uploaded.bin")

	if _, err := app.UploadFile("reverse-node", src, dst, FileTransferOptions{
		Parallel: 4, ChunkSize: 512 << 10,
	}, nil); err != nil {
		t.Fatalf("UploadFile(reverse) error = %v", err)
	}
	got, err := os.ReadFile(dst) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("reverse upload corrupted the file: got %d bytes, want %d", len(got), len(payload))
	}

	// The controller should have opened channels back to the agent rather than
	// funnelling everything through the one the agent opened.
	hub.mu.RLock()
	session := hub.sessions["reverse-node"]
	hub.mu.RUnlock()
	if session == nil {
		t.Fatal("reverse session disappeared")
	}
	session.mu.Lock()
	opened := session.opened
	session.mu.Unlock()
	if opened < 2 {
		t.Fatalf("expected the transfer to open extra channels, got %d", opened)
	}
}

// A round trip back down the reverse path must also survive, which exercises
// binary frames in the response direction.
func TestReverseTransferRoundTrip(t *testing.T) {
	app, _, _ := reverseTransferRig(t)

	payload := make([]byte, 1<<20+37)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "payload.bin")
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(dir, "remote.bin")
	if _, err := app.UploadFile("reverse-node", src, remote, FileTransferOptions{
		Parallel: 3, ChunkSize: 256 << 10,
	}, nil); err != nil {
		t.Fatalf("UploadFile(reverse) error = %v", err)
	}

	back := filepath.Join(dir, "back.bin")
	if _, err := app.DownloadFile("reverse-node", remote, back, FileTransferOptions{
		Parallel: 3, ChunkSize: 256 << 10,
	}, nil); err != nil {
		t.Fatalf("DownloadFile(reverse) error = %v", err)
	}
	got, err := os.ReadFile(back) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("reverse download differs from the original")
	}
}

// An agent that never advertises the capability must still work, on the original
// single-channel path.
func TestReverseTransferFallsBackWithoutMultiplex(t *testing.T) {
	t.Parallel()
	server := ServerRecord{
		Name: "old-agent", Mode: transport.ModeReverse,
		Capabilities: []string{"file.transfer", proto.CapabilityBinaryFrames},
	}
	app := &App{}
	got := app.resolveTransferOptions(server, FileTransferOptions{Parallel: 8})
	if got.Parallel != 1 {
		t.Fatalf("an agent without reverse multiplexing must stay single-stream, got %d", got.Parallel)
	}
}
