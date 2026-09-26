// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
)

// pipeChannel is an in-memory ssh.Channel: enough of one for transport.Session
// to run the real RPC codec against a fake agent without an SSH stack.
type pipeChannel struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (c *pipeChannel) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *pipeChannel) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c *pipeChannel) Close() error {
	_ = c.r.Close()
	return c.w.Close()
}
func (c *pipeChannel) CloseWrite() error                              { return c.w.Close() }
func (c *pipeChannel) SendRequest(string, bool, []byte) (bool, error) { return false, nil }
func (c *pipeChannel) Stderr() io.ReadWriter                          { return &bytes.Buffer{} }

func newPipeChannelPair() (*pipeChannel, *pipeChannel) {
	ar, aw := io.Pipe()
	br, bw := io.Pipe()
	return &pipeChannel{r: ar, w: bw}, &pipeChannel{r: br, w: aw}
}

// fakeEchoAgent answers fleet-rpc requests on ch the way a binary-frame
// capable agent does, without touching a filesystem: file.write is
// acknowledged, file.read returns readChunk, anything else gets a snapshot.
func fakeEchoAgent(ch *pipeChannel, readChunk []byte) {
	defer ch.Close()
	for {
		req, err := proto.Decode(ch)
		if err != nil {
			return
		}
		resp := proto.Envelope{Type: proto.EnvelopeTypeResponse, RequestID: req.RequestID, Action: req.Action}
		switch req.Action {
		case "test.sleep":
			time.Sleep(60 * time.Millisecond)
			resp.Payload = proto.ExecResult{}
		case proto.ActionFileWrite:
			resp.Payload = proto.FileWriteResult{BytesWritten: int64(len(req.Binary))}
		case proto.ActionFileRead:
			payload, _ := proto.DecodePayload[proto.FileReadPayload](req.Payload)
			resp.Payload = &proto.FileReadResult{Length: int64(len(readChunk)), Data: readChunk, SHA256: "s"}
			if proto.PeerWantsBinary(req, payload) {
				resp = proto.DetachBinary(resp)
			}
		default:
			resp.Payload = proto.MetricsSnapshot{Timestamp: time.Now().UTC(), Hostname: "bench", CPUPercent: 1, ProcessCount: 7}
		}
		if err := proto.Encode(ch, resp); err != nil {
			return
		}
	}
}

// benchControlToken is shaped like a current daemon's token (mutual-auth prefix).
const benchControlToken = controlTokenMutualAuthPrefix + "bench-control-token"

// controlBenchFleet runs a ReverseHub with a live control socket and one
// registered reverse "agent" answered by fakeEchoAgent.
func controlBenchFleet(tb testing.TB, readChunk []byte) (*App, *ReverseHub) {
	tb.Helper()
	configDir := tb.TempDir()
	if err := os.MkdirAll(filepath.Join(configDir, "data"), 0o700); err != nil {
		tb.Fatal(err)
	}
	const token = benchControlToken
	if err := os.WriteFile(filepath.Join(configDir, "data", "control.token"), []byte(token), 0o600); err != nil {
		tb.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	app := &App{ConfigDir: configDir}
	app.Config.Runtime.ControlAddress = listener.Addr().String()
	hub := NewReverseHub(app, token)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = hub.ServeControl(ctx, listener) }()

	client, agentSide := newPipeChannelPair()
	go fakeEchoAgent(agentSide, readChunk)
	session := &transport.Session{Mode: transport.ModeReverse, Channel: client}
	caps := []string{proto.CapabilityBinaryFrames}
	session.SetCapabilities(caps)
	hub.sessions["bench"] = &reverseSession{session: session, info: ReverseSessionInfo{Server: "bench", Connected: true, Hello: proto.HelloPayload{Capabilities: caps}}}
	tb.Cleanup(func() {
		cancel()
		_ = client.Close()
	})
	return app, hub
}

// BenchmarkReverseControlCall measures one reverse-mode RPC as issued by a CLI
// or web UI process: across the daemon's loopback control socket, into the hub
// and over the agent channel. "hub" is the same call made in-process, i.e. what
// the daemon's own poller pays once it stops dialling its own control socket.
func BenchmarkReverseControlCall(b *testing.B) {
	const size = 2 << 20
	chunk := make([]byte, size)
	if _, err := rand.Read(chunk); err != nil {
		b.Fatal(err)
	}
	app, hub := controlBenchFleet(b, chunk)
	small := proto.Envelope{Action: "metrics.collect", Payload: proto.MetricsPayload{Server: "bench"}}
	upload := func() proto.Envelope {
		return proto.DetachBinary(proto.Envelope{Action: proto.ActionFileWrite,
			Payload: &proto.FileWritePayload{TransferID: "t", Path: "/p", Data: chunk, SHA256: "s"}})
	}
	download := proto.Envelope{Action: proto.ActionFileRead, Payload: proto.FileReadPayload{Path: "/p", Length: size, Binary: true}}

	type caller func(context.Context, string, proto.Envelope) (proto.Envelope, error)
	paths := []struct {
		name string
		call caller
	}{
		{"control", app.callReverseControlContext},
		{"hub", hub.CallContext},
	}
	for _, path := range paths {
		b.Run("small/"+path.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := path.call(context.Background(), "bench", small); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("upload2MiB/"+path.name, func(b *testing.B) {
			env := upload()
			b.SetBytes(size)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := path.call(context.Background(), "bench", env); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("download2MiB/"+path.name, func(b *testing.B) {
			b.SetBytes(size)
			b.ReportAllocs()
			for b.Loop() {
				resp, err := path.call(context.Background(), "bench", download)
				if err != nil {
					b.Fatal(err)
				}
				if len(resp.Binary) != size {
					b.Fatalf("download returned %d bytes", len(resp.Binary))
				}
			}
		})
	}
}
