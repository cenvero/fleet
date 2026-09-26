// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package proto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// binaryFrameFlag marks a message as carrying a raw byte attachment after the
// JSON envelope:
//
//	standard: [uint32 jsonLen              ][json]
//	framed:   [uint32 jsonLen|binaryFrameFlag][json][uint32 blobLen][blob]
//
// Bulk file chunks used to travel as a []byte field inside the envelope, which
// JSON base64-encodes: +33% on the wire, plus a base64 encode and decode and
// several multi-megabyte allocations per chunk. That capped file transfer at
// roughly 200 MB/s of pure CPU on fast hardware — far below what the disk or a
// good link can do — and made the agent allocate tens of megabytes per chunk.
// Carrying those bytes verbatim after the envelope costs a copy instead.
//
// jsonLen is bounded by MaxEnvelopeSize (16 MiB), so the top bit is always free.
// A peer running an older build reads the flagged length as an enormous size and
// rejects it, which is why framed messages are only ever sent to a peer that has
// advertised CapabilityBinaryFrames.
const binaryFrameFlag uint32 = 1 << 31

// MaxBinaryFrameBytes bounds a single binary attachment, mirroring the envelope
// ceiling so a hostile length can't drive an unbounded allocation.
const MaxBinaryFrameBytes = 16 * 1024 * 1024

// maxPooledCodecBuffer caps the buffers the codec keeps for reuse. Control
// messages are a few hundred bytes to a few KiB; the rare multi-megabyte
// envelope (a legacy base64 chunk, a big listing) is allocated and dropped
// rather than pinned in the pool.
const maxPooledCodecBuffer = 1 << 20

var encodeBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// Encode writes env to w.
//
// The length prefix, the JSON body and — for a framed message — the attachment
// length are assembled in one pooled buffer and handed to w in a single Write;
// only the attachment itself is written separately, straight from the caller's
// slice. On an SSH channel every Write becomes its own channel-data packet
// (its own encryption and usually its own syscall), so the previous 2 writes per
// message (4 with an attachment) cost measurably more than 1 (or 2). The bytes
// on the wire are identical to what the unbuffered encoder produced.
func Encode(w io.Writer, env Envelope) error {
	if env.ProtocolVersion == 0 {
		env.ProtocolVersion = CurrentProtocolVersion
	}
	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now().UTC()
	}

	blob := env.Binary
	if len(blob) > MaxBinaryFrameBytes {
		return fmt.Errorf("binary attachment of %d bytes exceeds maximum of %d", len(blob), MaxBinaryFrameBytes)
	}

	buf, _ := encodeBufPool.Get().(*bytes.Buffer)
	if buf == nil {
		buf = new(bytes.Buffer)
	}
	defer func() {
		if buf.Cap() <= maxPooledCodecBuffer {
			encodeBufPool.Put(buf)
		}
	}()
	buf.Reset()
	buf.Write([]byte{0, 0, 0, 0}) // length prefix, patched below

	// json.Encoder produces exactly json.Marshal's bytes (including HTML
	// escaping) plus a trailing newline, which is trimmed off.
	if err := json.NewEncoder(buf).Encode(env); err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	buf.Truncate(buf.Len() - 1)
	bodyLen := buf.Len() - 4
	if bodyLen > MaxEnvelopeSize {
		return fmt.Errorf("envelope of %d bytes exceeds maximum of %d", bodyLen, MaxEnvelopeSize)
	}

	header := uint32(bodyLen) // #nosec G115 -- bounded by MaxEnvelopeSize above
	if blob != nil {
		header |= binaryFrameFlag
		var blobLen [4]byte
		binary.BigEndian.PutUint32(blobLen[:], uint32(len(blob))) // #nosec G115 -- bounded above
		buf.Write(blobLen[:])
	}
	frame := buf.Bytes()
	binary.BigEndian.PutUint32(frame[:4], header)
	if _, err := w.Write(frame); err != nil {
		if blob != nil {
			return fmt.Errorf("write envelope: %w", err)
		}
		return fmt.Errorf("write envelope body: %w", err)
	}
	if blob == nil {
		return nil
	}
	if _, err := w.Write(blob); err != nil {
		return fmt.Errorf("write binary frame: %w", err)
	}
	return nil
}

// MaxEnvelopeSize is the maximum allowed size of a single protocol envelope.
// An attacker sending a crafted 4-byte size of 0xFFFFFFFF would otherwise
// cause the receiver to allocate 4 GiB of memory.
const MaxEnvelopeSize = 16 * 1024 * 1024 // 16 MiB

// decodeBufPool holds scratch buffers for the length prefixes and the JSON
// body. Reusing the body buffer is safe because nothing in a decoded Envelope
// aliases it: encoding/json copies strings, and Envelope.UnmarshalJSON keeps
// the payload as a json.RawMessage, whose UnmarshalJSON copies its bytes. The
// binary attachment, which IS handed to the caller, is always a fresh slice.
var decodeBufPool = sync.Pool{New: func() any {
	b := make([]byte, 0, 4096)
	return &b
}}

func Decode(r io.Reader) (Envelope, error) {
	scratch, _ := decodeBufPool.Get().(*[]byte)
	if scratch == nil {
		b := make([]byte, 0, 4096)
		scratch = &b
	}
	defer func() {
		if cap(*scratch) <= maxPooledCodecBuffer {
			decodeBufPool.Put(scratch)
		}
	}()
	buf := (*scratch)[:4]

	if _, err := io.ReadFull(r, buf); err != nil {
		return Envelope{}, fmt.Errorf("read envelope length: %w", err)
	}
	header := binary.BigEndian.Uint32(buf)
	framed := header&binaryFrameFlag != 0
	size := header &^ binaryFrameFlag
	if size == 0 {
		return Envelope{}, fmt.Errorf("envelope length must be greater than zero")
	}
	if size > MaxEnvelopeSize {
		return Envelope{}, fmt.Errorf("envelope length %d exceeds maximum allowed size of %d bytes", size, MaxEnvelopeSize)
	}

	if int(size) > cap(*scratch) {
		*scratch = make([]byte, 0, size)
	}
	body := (*scratch)[:size]
	if _, err := io.ReadFull(r, body); err != nil {
		return Envelope{}, fmt.Errorf("read envelope body: %w", err)
	}

	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Envelope{}, fmt.Errorf("unmarshal envelope: %w", err)
	}
	if framed {
		lenBuf := (*scratch)[:4]
		if _, err := io.ReadFull(r, lenBuf); err != nil {
			return Envelope{}, fmt.Errorf("read binary frame length: %w", err)
		}
		blobLen := binary.BigEndian.Uint32(lenBuf)
		if blobLen > MaxBinaryFrameBytes {
			return Envelope{}, fmt.Errorf("binary frame length %d exceeds maximum allowed size of %d bytes", blobLen, MaxBinaryFrameBytes)
		}
		blob := make([]byte, blobLen)
		if _, err := io.ReadFull(r, blob); err != nil {
			return Envelope{}, fmt.Errorf("read binary frame: %w", err)
		}
		env.Binary = blob
	}
	if env.ProtocolVersion == 0 {
		env.ProtocolVersion = CurrentProtocolVersion
	}
	// Reject completely empty envelopes — both fields absent is a strong signal
	// of malformed or garbage data (a valid request always has Action set; a valid
	// response always has Type set).
	if env.Action == "" && env.Type == "" {
		return Envelope{}, fmt.Errorf("malformed envelope: both action and type are empty")
	}
	return env, nil
}
