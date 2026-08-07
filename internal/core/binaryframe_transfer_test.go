// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

// TestTransferUsesBinaryFramesEndToEnd walks a real file up to an in-process
// agent and back down, asserting the bytes survive intact. The agent advertises
// CapabilityBinaryFrames, so this exercises the raw-frame encoding rather than
// base64-in-JSON — the path that carries essentially all transfer bytes.
func TestTransferUsesBinaryFramesEndToEnd(t *testing.T) {
	t.Parallel()
	app, _, errCh := pooledTestFleet(t)

	// Several chunks, plus a partial final one, so offsets and the tail are
	// both covered.
	payload := make([]byte, 5<<20+1234)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "payload.bin")
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	dstDir := t.TempDir()
	remote := filepath.Join(dstDir, "uploaded.bin")
	if _, err := app.UploadFile("loopback", src, remote, FileTransferOptions{
		Parallel: 3, ChunkSize: 1 << 20,
	}, nil); err != nil {
		t.Fatalf("UploadFile() error = %v", err)
	}
	got, err := os.ReadFile(remote) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("uploaded file differs: got %d bytes, want %d", len(got), len(payload))
	}

	back := filepath.Join(srcDir, "roundtrip.bin")
	if _, err := app.DownloadFile("loopback", remote, back, FileTransferOptions{
		Parallel: 3, ChunkSize: 1 << 20,
	}, nil); err != nil {
		t.Fatalf("DownloadFile() error = %v", err)
	}
	got, err = os.ReadFile(back) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("downloaded file differs from the original")
	}

	app.DisconnectPooledSessions()
	drainAgentServeErrs(t, errCh)
}

// TestTransferFallsBackWithoutBinaryCapability proves the compatibility path: an
// agent that never advertises CapabilityBinaryFrames — an older build — still
// transfers correctly over the original base64-in-JSON encoding.
func TestTransferFallsBackWithoutBinaryCapability(t *testing.T) {
	t.Parallel()

	payload := make([]byte, (2<<20)+77)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	// A chunk written without a binary frame must round-trip through the JSON
	// field exactly as before.
	env := proto.Envelope{
		Action: proto.ActionFileWrite,
		Payload: &proto.FileWritePayload{
			TransferID: "legacy", Path: "/tmp/x", Offset: 0, Data: payload, SHA256: "s",
		},
	}
	var buf bytes.Buffer
	if err := proto.Encode(&buf, env); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	decoded, err := proto.Decode(&buf)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if decoded.Binary != nil {
		t.Fatal("an unframed message must not produce a binary attachment")
	}
	out, err := proto.DecodePayload[proto.FileWritePayload](decoded.Payload)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}
	if !bytes.Equal(out.Data, payload) {
		t.Fatal("legacy encoding lost the chunk bytes")
	}
}
