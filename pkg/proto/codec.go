// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package proto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
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

	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	if len(body) > MaxEnvelopeSize {
		return fmt.Errorf("envelope of %d bytes exceeds maximum of %d", len(body), MaxEnvelopeSize)
	}

	header := uint32(len(body)) // #nosec G115 -- bounded by MaxEnvelopeSize above
	if blob != nil {
		header |= binaryFrameFlag
	}
	if err := binary.Write(w, binary.BigEndian, header); err != nil {
		return fmt.Errorf("write envelope length: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("write envelope body: %w", err)
	}
	if blob == nil {
		return nil
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(blob))); err != nil { // #nosec G115 -- bounded above
		return fmt.Errorf("write binary frame length: %w", err)
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

func Decode(r io.Reader) (Envelope, error) {
	var header uint32
	if err := binary.Read(r, binary.BigEndian, &header); err != nil {
		return Envelope{}, fmt.Errorf("read envelope length: %w", err)
	}
	framed := header&binaryFrameFlag != 0
	size := header &^ binaryFrameFlag
	if size == 0 {
		return Envelope{}, fmt.Errorf("envelope length must be greater than zero")
	}
	if size > MaxEnvelopeSize {
		return Envelope{}, fmt.Errorf("envelope length %d exceeds maximum allowed size of %d bytes", size, MaxEnvelopeSize)
	}

	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return Envelope{}, fmt.Errorf("read envelope body: %w", err)
	}

	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Envelope{}, fmt.Errorf("unmarshal envelope: %w", err)
	}
	if framed {
		var blobLen uint32
		if err := binary.Read(r, binary.BigEndian, &blobLen); err != nil {
			return Envelope{}, fmt.Errorf("read binary frame length: %w", err)
		}
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
