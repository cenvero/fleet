// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkUploadThroughput moves a real file to an in-process agent over the
// full stack: SSH channels, the RPC codec, chunk checksums and the temp-file
// assembly. There is no network latency here, so it measures the CPU and
// allocation cost of the transfer engine itself — which is exactly what the
// binary frame encoding, the reused chunk buffers and the pooled connections
// were meant to reduce.
func BenchmarkUploadThroughput(b *testing.B) {
	app, _, _ := pooledTestFleet(b)

	const size = 16 << 20
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		b.Fatal(err)
	}
	src := filepath.Join(b.TempDir(), "payload.bin")
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		b.Fatal(err)
	}
	dstDir := b.TempDir()

	b.SetBytes(size)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		dst := filepath.Join(dstDir, "out.bin")
		if _, err := app.UploadFile("loopback", src, dst, FileTransferOptions{}, nil); err != nil {
			b.Fatalf("UploadFile() error = %v", err)
		}
		// Each iteration must actually move bytes, not resume a finished temp.
		if err := os.Remove(dst); err != nil {
			b.Fatal(err)
		}
	}
}
