// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package transport_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
	"golang.org/x/crypto/ssh"
)

// writeCountingConn counts socket writes, which is what each SSH packet costs.
type writeCountingConn struct {
	net.Conn
	writes *atomic.Int64
}

func (c writeCountingConn) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(p)
}

type rpcBenchRig struct {
	session      *transport.Session
	clientWrites atomic.Int64
	serverWrites atomic.Int64
	close        func()
}

// newRPCBenchRig connects a real SSH client and server over loopback TCP and
// serves fleet-rpc channels with respond, so a benchmark measures the codec and
// the SSH channel exactly as a controller→agent call pays for them.
func newRPCBenchRig(b *testing.B, respond func(proto.Envelope) proto.Envelope) *rpcBenchRig {
	b.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		b.Fatal(err)
	}
	sshCfg := ssh.Config{Ciphers: transport.SupportedCiphers(), KeyExchanges: transport.SupportedKEX(), MACs: transport.SupportedMACs()}
	serverCfg := &ssh.ServerConfig{NoClientAuth: true, Config: sshCfg}
	serverCfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	rig := &rpcBenchRig{}
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		conn, chans, reqs, err := ssh.NewServerConn(writeCountingConn{raw, &rig.serverWrites}, serverCfg)
		if err != nil {
			return
		}
		defer conn.Close()
		go ssh.DiscardRequests(reqs)
		for nc := range chans {
			ch, chReqs, err := nc.Accept()
			if err != nil {
				continue
			}
			go ssh.DiscardRequests(chReqs)
			go func() {
				for {
					req, err := proto.Decode(ch)
					if err != nil {
						return
					}
					if err := proto.Encode(ch, respond(req)); err != nil {
						return
					}
				}
			}()
		}
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	clientCfg := &ssh.ClientConfig{User: "bench", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Config: sshCfg} // #nosec G106 -- in-process benchmark peer
	conn, chans, reqs, err := ssh.NewClientConn(writeCountingConn{raw, &rig.clientWrites}, "bench", clientCfg)
	if err != nil {
		b.Fatal(err)
	}
	client := ssh.NewClient(conn, chans, reqs)
	channel, chReqs, err := client.OpenChannel(transport.RPCChannelType, nil)
	if err != nil {
		b.Fatal(err)
	}
	go ssh.DiscardRequests(chReqs)
	rig.session = &transport.Session{Mode: transport.ModeDirect, Client: client, Channel: channel}
	rig.close = func() {
		_ = rig.session.Close()
		_ = ln.Close()
	}
	return rig
}

func (r *rpcBenchRig) report(b *testing.B, c0, s0 int64) {
	b.ReportMetric(float64(r.clientWrites.Load()-c0)/float64(b.N), "cli-writes/op")
	b.ReportMetric(float64(r.serverWrites.Load()-s0)/float64(b.N), "srv-writes/op")
}

// BenchmarkSessionCallSmallRPC is one small control RPC (metrics.collect) over
// a real SSH channel on loopback: the steady-state cost of a pooled call.
func BenchmarkSessionCallSmallRPC(b *testing.B) {
	snapshot := proto.MetricsSnapshot{
		Timestamp: time.Now().UTC(), Hostname: "web-01", CPUPercent: 12.5, MemoryPercent: 44.1,
		MemoryUsedBytes: 1 << 33, MemoryTotalBytes: 1 << 34, DiskPath: "/", DiskPercent: 51,
		Load1: 0.3, Load5: 0.2, Load15: 0.1, UptimeSeconds: 123456, ProcessCount: 321,
	}
	rig := newRPCBenchRig(b, func(req proto.Envelope) proto.Envelope {
		return proto.Envelope{Type: proto.EnvelopeTypeResponse, RequestID: req.RequestID, Action: req.Action, Payload: snapshot}
	})
	defer rig.close()
	req := proto.Envelope{Action: "metrics.collect", Payload: proto.MetricsPayload{Server: "web-01"}}
	ctx := context.Background()
	if _, err := rig.session.Call(ctx, req); err != nil { // warm up
		b.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	c0, s0 := rig.clientWrites.Load(), rig.serverWrites.Load()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := rig.session.Call(ctx, req)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := proto.DecodePayload[proto.MetricsSnapshot](resp.Payload); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	rig.report(b, c0, s0)
}

// BenchmarkSessionCallChunkUpload sends 2 MiB file chunks as binary frames and
// reads the small acknowledgement, i.e. one step of an upload.
func BenchmarkSessionCallChunkUpload(b *testing.B) {
	const size = 2 << 20
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		b.Fatal(err)
	}
	rig := newRPCBenchRig(b, func(req proto.Envelope) proto.Envelope {
		return proto.Envelope{Type: proto.EnvelopeTypeResponse, RequestID: req.RequestID, Action: req.Action,
			Payload: proto.FileWriteResult{BytesWritten: int64(len(req.Binary))}}
	})
	defer rig.close()
	ctx := context.Background()
	c0, s0 := rig.clientWrites.Load(), rig.serverWrites.Load()
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		env := proto.DetachBinary(proto.Envelope{Action: proto.ActionFileWrite,
			Payload: &proto.FileWritePayload{TransferID: "t", Path: "/p", Offset: int64(i) * size, Data: data, SHA256: "s"}})
		if _, err := rig.session.Call(ctx, env); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	rig.report(b, c0, s0)
}
