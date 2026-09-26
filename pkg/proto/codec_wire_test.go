// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package proto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// legacyEncode is the encoder as it shipped before writes were coalesced. It
// is the reference the current encoder must match byte for byte, because the
// peer on the other end of the channel may be running that older build.
func legacyEncode(w io.Writer, env Envelope) error {
	if env.ProtocolVersion == 0 {
		env.ProtocolVersion = CurrentProtocolVersion
	}
	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now().UTC()
	}
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	header := uint32(len(body))
	if env.Binary != nil {
		header |= binaryFrameFlag
	}
	if err := binary.Write(w, binary.BigEndian, header); err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	if env.Binary == nil {
		return nil
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(env.Binary))); err != nil {
		return err
	}
	_, err = w.Write(env.Binary)
	return err
}

func TestEncodeIsByteIdenticalToLegacyEncoder(t *testing.T) {
	ts := time.Date(2026, 9, 26, 8, 0, 0, 123456789, time.UTC)
	cases := map[string]Envelope{
		"small": smallResponseEnvelope(),
		// HTML-sensitive characters must be escaped exactly as json.Marshal does.
		"html": {Type: EnvelopeTypeRequest, Action: "shell.exec", Timestamp: ts,
			Payload: ExecPayload{Command: `echo "<b>&amp;</b>" > /tmp/x && ls`}},
		"error": {Type: EnvelopeTypeResponse, Action: "file.stat", Timestamp: ts, RequestID: "r1",
			Error: &Error{Code: "not_found", Message: "no such file   here"}},
		"raw payload": {Type: EnvelopeTypeResponse, Action: "x", Timestamp: ts, Payload: json.RawMessage(`{"a":[1,2,3]}`)},
		"framed": DetachBinary(Envelope{Action: ActionFileWrite, Timestamp: ts,
			Payload: &FileWritePayload{TransferID: "t", Path: "/p", Offset: 7, Data: []byte("chunk-bytes"), SHA256: "s"}}),
		"empty frame": {Action: ActionFileWrite, Timestamp: ts, Binary: []byte{}},
	}
	for name, env := range cases {
		var got, want bytes.Buffer
		if err := Encode(&got, env); err != nil {
			t.Fatalf("%s: Encode() error = %v", name, err)
		}
		if err := legacyEncode(&want, env); err != nil {
			t.Fatalf("%s: legacyEncode() error = %v", name, err)
		}
		if !bytes.Equal(got.Bytes(), want.Bytes()) {
			t.Fatalf("%s: wire bytes changed\n got: %q\nwant: %q", name, got.Bytes(), want.Bytes())
		}
	}
}

func TestEncodeCoalescesWrites(t *testing.T) {
	var small countingWriter
	if err := Encode(&small, smallResponseEnvelope()); err != nil {
		t.Fatal(err)
	}
	if small.writes != 1 {
		t.Fatalf("unframed message took %d writes, want 1", small.writes)
	}
	var framed countingWriter
	env := DetachBinary(Envelope{Action: ActionFileWrite, Payload: &FileWritePayload{Data: bytes.Repeat([]byte{7}, 1024)}})
	if err := Encode(&framed, env); err != nil {
		t.Fatal(err)
	}
	if framed.writes != 2 {
		t.Fatalf("framed message took %d writes, want 2 (envelope, then attachment)", framed.writes)
	}
}

// Decode reuses its body buffer between calls, so nothing it returns may
// alias that buffer: decode several messages back to back and check the
// first one is still intact afterwards.
func TestDecodeResultsDoNotAliasPooledBuffers(t *testing.T) {
	var stream bytes.Buffer
	first := Envelope{Type: EnvelopeTypeResponse, Action: "first", RequestID: "req-1",
		Capabilities: []string{"cap-a"}, Payload: map[string]any{"value": strings.Repeat("A", 300)},
		Error: &Error{Code: "c1", Message: "m1"}}
	if err := Encode(&stream, first); err != nil {
		t.Fatal(err)
	}
	blob := bytes.Repeat([]byte{1}, 64)
	if err := Encode(&stream, Envelope{Action: ActionFileWrite, Binary: blob}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := Encode(&stream, Envelope{Type: EnvelopeTypeResponse, Action: "overwrite", RequestID: strings.Repeat("Z", 7),
			Capabilities: []string{"zzzzz"}, Payload: map[string]any{"value": strings.Repeat("Z", 300)},
			Error: &Error{Code: "zz", Message: "zz"}}); err != nil {
			t.Fatal(err)
		}
	}

	gotFirst, err := Decode(&stream)
	if err != nil {
		t.Fatal(err)
	}
	gotFramed, err := Decode(&stream)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := Decode(&stream); err != nil {
			t.Fatal(err)
		}
	}
	payload, err := DecodePayload[map[string]string](gotFirst.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if gotFirst.Action != "first" || gotFirst.RequestID != "req-1" || gotFirst.Capabilities[0] != "cap-a" ||
		gotFirst.Error.Code != "c1" || payload["value"] != strings.Repeat("A", 300) {
		t.Fatalf("first envelope was corrupted by later decodes: %+v payload=%v", gotFirst, payload)
	}
	if !bytes.Equal(gotFramed.Binary, blob) {
		t.Fatal("binary attachment was corrupted by later decodes")
	}
}

// A message split across many short reads (as an SSH channel delivers it) must
// decode identically.
func TestDecodeHandlesShortReads(t *testing.T) {
	var stream bytes.Buffer
	env := DetachBinary(Envelope{Action: ActionFileWrite, RequestID: "r",
		Payload: &FileWritePayload{TransferID: "t", Data: bytes.Repeat([]byte{9}, 5000)}})
	if err := Encode(&stream, env); err != nil {
		t.Fatal(err)
	}
	got, err := Decode(&oneByteReader{r: &stream})
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestID != "r" || len(got.Binary) != 5000 {
		t.Fatalf("short-read decode = %+v (%d attachment bytes)", got, len(got.Binary))
	}
}

type oneByteReader struct{ r io.Reader }

func (o *oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return o.r.Read(p)
}

func TestDecodeReportsTruncatedStreams(t *testing.T) {
	var full bytes.Buffer
	if err := Encode(&full, smallResponseEnvelope()); err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(bytes.NewReader(nil)); err == nil || !strings.Contains(err.Error(), "read envelope length") {
		t.Fatalf("empty stream error = %v", err)
	}
	if _, err := Decode(bytes.NewReader(full.Bytes()[:full.Len()-3])); err == nil || !strings.Contains(err.Error(), "read envelope body") {
		t.Fatalf("truncated body error = %v", err)
	}
}
