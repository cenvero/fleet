// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package proto

import (
	"bytes"
	"crypto/rand"
	"testing"
	"time"
)

// chunkEnvelope builds a file.write envelope carrying a raw chunk of size
// bytes — the single hottest message in the protocol.
func chunkEnvelope(size int) Envelope {
	data := make([]byte, size)
	_, _ = rand.Read(data)
	return Envelope{
		Action: ActionFileWrite,
		Payload: FileWritePayload{
			TransferID: "abcdef0123456789",
			Path:       "/var/tmp/target.bin",
			Offset:     0,
			Data:       data,
			SHA256:     "0000000000000000000000000000000000000000000000000000000000000000",
		},
	}
}

// BenchmarkChunkRoundTrip measures the full receive path a peer runs per chunk:
// Decode off the channel, then DecodePayload into the concrete struct. Keeping
// Payload as raw bytes (see Envelope.UnmarshalJSON) removes a second
// marshal/unmarshal round trip that used to halve throughput here.
func BenchmarkChunkRoundTrip(b *testing.B) {
	for _, size := range []int{1 << 20, 4 << 20} {
		env := chunkEnvelope(size)
		var buf bytes.Buffer
		if err := Encode(&buf, env); err != nil {
			b.Fatal(err)
		}
		wire := buf.Bytes()

		b.Run(byteLabel(size), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				e, err := Decode(bytes.NewReader(wire))
				if err != nil {
					b.Fatal(err)
				}
				if _, err := DecodePayload[FileWritePayload](e.Payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkChunkEncode(b *testing.B) {
	for _, size := range []int{1 << 20, 4 << 20} {
		env := chunkEnvelope(size)
		b.Run(byteLabel(size), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				var out bytes.Buffer
				if err := Encode(&out, env); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func byteLabel(n int) string {
	switch n {
	case 1 << 20:
		return "1MiB"
	case 4 << 20:
		return "4MiB"
	}
	return "chunk"
}

// countingWriter counts Write calls. Every Write on an SSH channel becomes its
// own channel-data packet (and its own encrypt + syscall), so the number of
// writes per message is a cost in its own right, not just the bytes.
type countingWriter struct {
	bytes.Buffer
	writes int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(p)
}

func smallResponseEnvelope() Envelope {
	return Envelope{
		Type:      EnvelopeTypeResponse,
		RequestID: "0123456789abcdef",
		Action:    "metrics.collect",
		Timestamp: time.Unix(1700000000, 0).UTC(),
		Payload: MetricsSnapshot{
			Timestamp: time.Unix(1700000000, 0).UTC(), Hostname: "web-01", CPUPercent: 12.5,
			MemoryPercent: 44.1, MemoryUsedBytes: 1 << 33, MemoryTotalBytes: 1 << 34, DiskPath: "/",
			DiskPercent: 51, Load1: 0.3, Load5: 0.2, Load15: 0.1, UptimeSeconds: 123456, ProcessCount: 321,
		},
	}
}

// BenchmarkEncodeSmallEnvelope is the per-RPC encode cost of a typical control
// message (a metrics snapshot). writes/op is how many channel packets it takes.
func BenchmarkEncodeSmallEnvelope(b *testing.B) {
	env := smallResponseEnvelope()
	var w countingWriter
	b.ReportAllocs()
	for b.Loop() {
		w.Reset()
		if err := Encode(&w, env); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(w.writes)/float64(b.N), "writes/op")
}

// BenchmarkEncodeFramedChunk encodes a 2 MiB file chunk carried as a binary
// frame; writes/op shows how many separate channel writes one chunk costs.
func BenchmarkEncodeFramedChunk(b *testing.B) {
	const size = 2 << 20
	data := make([]byte, size)
	_, _ = rand.Read(data)
	env := DetachBinary(Envelope{
		Action:  ActionFileWrite,
		Payload: &FileWritePayload{TransferID: "abcdef0123456789", Path: "/var/tmp/target.bin", Data: data, SHA256: "s"},
	})
	if env.Binary == nil {
		b.Fatal("chunk was not detached into a binary frame")
	}
	var w countingWriter
	w.Grow(size + 4096)
	b.SetBytes(size)
	b.ReportAllocs()
	for b.Loop() {
		w.Reset()
		if err := Encode(&w, env); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(w.writes)/float64(b.N), "writes/op")
}

// BenchmarkDecodeSmallEnvelope is the per-RPC receive cost of the same message.
func BenchmarkDecodeSmallEnvelope(b *testing.B) {
	var buf bytes.Buffer
	if err := Encode(&buf, smallResponseEnvelope()); err != nil {
		b.Fatal(err)
	}
	wire := buf.Bytes()
	r := bytes.NewReader(wire)
	b.ReportAllocs()
	for b.Loop() {
		r.Reset(wire)
		env, err := Decode(r)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := DecodePayload[MetricsSnapshot](env.Payload); err != nil {
			b.Fatal(err)
		}
	}
}
