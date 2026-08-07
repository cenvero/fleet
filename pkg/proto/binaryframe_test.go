// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package proto

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"testing"
)

func TestBinaryFrameRoundTrip(t *testing.T) {
	data := make([]byte, 64*1024)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	env := DetachBinary(Envelope{
		Action:  ActionFileWrite,
		Payload: &FileWritePayload{TransferID: "t1", Path: "/tmp/x", Offset: 4096, Data: data, SHA256: "abc"},
	})
	if env.Binary == nil {
		t.Fatal("DetachBinary did not move the chunk out of the payload")
	}
	if got := env.Payload.(*FileWritePayload).Data; got != nil {
		t.Fatal("chunk bytes were left in the payload as well as the frame")
	}

	var buf bytes.Buffer
	if err := Encode(&buf, env); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	// The chunk must ride raw, not base64: with base64 the wire would be at
	// least 4/3 the payload size.
	if wire := buf.Len(); wire > len(data)+4096 {
		t.Fatalf("framed message is %d bytes for a %d byte chunk — bytes are not travelling raw", wire, len(data))
	}

	decoded, err := Decode(&buf)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	payload, err := DecodePayload[FileWritePayload](decoded.Payload)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}
	if payload.Data != nil {
		t.Fatal("payload carried data before the frame was attached")
	}
	AttachBinary(&payload, decoded)
	if !bytes.Equal(payload.Data, data) {
		t.Fatal("chunk bytes did not survive the round trip")
	}
	if payload.Offset != 4096 || payload.TransferID != "t1" {
		t.Fatalf("envelope fields corrupted: %+v", payload)
	}
}

// A message with no attachment must stay byte-identical to the original format,
// so a new peer talking to an old one is indistinguishable from an old peer.
func TestUnframedMessagesAreWireCompatible(t *testing.T) {
	env := Envelope{Action: ActionFileList, Payload: FileListPayload{Path: "/etc"}}
	var buf bytes.Buffer
	if err := Encode(&buf, env); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	header := binary.BigEndian.Uint32(buf.Bytes()[:4])
	if header&binaryFrameFlag != 0 {
		t.Fatal("a message without an attachment set the binary frame flag")
	}
	if int(header) != buf.Len()-4 {
		t.Fatalf("length prefix %d does not match body length %d", header, buf.Len()-4)
	}
}

// An oversized attachment length must be rejected before allocating, the same
// way an oversized envelope is.
func TestBinaryFrameRejectsOversizedLength(t *testing.T) {
	var buf bytes.Buffer
	body := []byte(`{"type":"request","action":"file.write"}`)
	binary.Write(&buf, binary.BigEndian, uint32(len(body))|binaryFrameFlag) //nolint:errcheck // in-memory writer
	buf.Write(body)
	binary.Write(&buf, binary.BigEndian, uint32(MaxBinaryFrameBytes+1)) //nolint:errcheck // in-memory writer

	if _, err := Decode(&buf); err == nil {
		t.Fatal("Decode accepted a binary frame larger than MaxBinaryFrameBytes")
	}
}

func TestPeerWantsBinary(t *testing.T) {
	// A request that arrived framed proves the sender speaks frames.
	if !PeerWantsBinary(Envelope{Binary: []byte{1}}, FileWritePayload{}) {
		t.Fatal("a framed request should imply binary support")
	}
	// An explicit ask counts too.
	if !PeerWantsBinary(Envelope{}, FileReadPayload{Binary: true}) {
		t.Fatal("an explicit Binary request should imply binary support")
	}
	// An older controller does neither and must get the legacy encoding.
	if PeerWantsBinary(Envelope{}, FileReadPayload{}) {
		t.Fatal("a plain request must not be answered with a binary frame")
	}
}

// BenchmarkChunkBinaryFrame is the counterpart to BenchmarkChunkRoundTrip: the
// same 4 MiB chunk carried as a raw frame instead of base64 inside JSON.
func BenchmarkChunkBinaryFrame(b *testing.B) {
	const size = 4 << 20
	data := make([]byte, size)
	_, _ = rand.Read(data)
	env := DetachBinary(Envelope{
		Action:  ActionFileWrite,
		Payload: &FileWritePayload{TransferID: "t", Path: "/p", Data: data, SHA256: "s"},
	})
	var buf bytes.Buffer
	if err := Encode(&buf, env); err != nil {
		b.Fatal(err)
	}
	wire := buf.Bytes()
	b.SetBytes(int64(size))
	b.ReportAllocs()
	for b.Loop() {
		e, err := Decode(bytes.NewReader(wire))
		if err != nil {
			b.Fatal(err)
		}
		p, err := DecodePayload[FileWritePayload](e.Payload)
		if err != nil {
			b.Fatal(err)
		}
		AttachBinary(&p, e)
	}
}
