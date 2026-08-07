// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

// A reverse-mode RPC issued from a CLI process crosses the local control socket
// as JSON before the daemon puts it on the SSH channel. Envelope.Binary is
// deliberately not a JSON field — the wire codec moves it — so the control
// protocol has to carry it explicitly or a framed file chunk arrives empty.
func TestReverseControlPreservesBinaryFrame(t *testing.T) {
	t.Parallel()

	chunk := make([]byte, 256*1024)
	if _, err := rand.Read(chunk); err != nil {
		t.Fatal(err)
	}
	env := proto.DetachBinary(proto.Envelope{
		Action: proto.ActionFileWrite,
		Payload: &proto.FileWritePayload{
			TransferID: "t1", Path: "/tmp/x", Offset: 0, Data: chunk, SHA256: "sum",
		},
	})
	if env.Binary == nil {
		t.Fatal("precondition: chunk should have moved into the binary frame")
	}

	req := newReverseControlRequest("tok", "call", "web-1", env)
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var decoded reverseControlRequest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}

	got := decoded.envelope()
	if !bytes.Equal(got.Binary, chunk) {
		t.Fatalf("binary frame lost crossing the control socket: got %d bytes, want %d",
			len(got.Binary), len(chunk))
	}
}

// The same applies in the response direction, which carries downloaded chunks.
func TestReverseControlResponsePreservesBinaryFrame(t *testing.T) {
	t.Parallel()

	chunk := make([]byte, 128*1024)
	if _, err := rand.Read(chunk); err != nil {
		t.Fatal(err)
	}
	env := proto.DetachBinary(proto.Envelope{
		Type:    proto.EnvelopeTypeResponse,
		Action:  proto.ActionFileRead,
		Payload: &proto.FileReadResult{Offset: 0, Length: int64(len(chunk)), Data: chunk, SHA256: "sum"},
	})

	resp := reverseControlResponse{}
	resp.setResponse(env)
	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var decoded reverseControlResponse
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	got, ok := decoded.responseEnvelope()
	if !ok {
		t.Fatal("response envelope missing")
	}
	if !bytes.Equal(got.Binary, chunk) {
		t.Fatalf("binary frame lost in the control response: got %d bytes, want %d",
			len(got.Binary), len(chunk))
	}
}
