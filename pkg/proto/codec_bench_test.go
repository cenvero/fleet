// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package proto

import (
	"bytes"
	"crypto/rand"
	"testing"
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
