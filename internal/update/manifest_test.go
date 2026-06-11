// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

func TestBinaryForSelectsCurrentTarget(t *testing.T) {
	t.Parallel()

	target := runtime.GOOS + "-" + runtime.GOARCH
	manifest := Manifest{
		Channels: map[string]ChannelInfo{
			"stable": {Version: "v1.0.0"},
		},
		Binaries: map[string]map[string]BinaryInfo{
			"v1.0.0": {
				target: {URL: "https://example.com/fleet.tar.gz", SHA256: "abc123"},
			},
		},
	}

	version, binary, err := manifest.BinaryFor("stable", false)
	if err != nil {
		t.Fatalf("BinaryFor() error = %v", err)
	}
	if version != "v1.0.0" {
		t.Fatalf("version = %s, want v1.0.0", version)
	}
	if binary.URL == "" {
		t.Fatalf("binary URL should not be empty")
	}
}

// TestFetchRejectsOversizedManifest verifies that Fetch bounds the manifest body
// before decoding, so an oversized response is rejected rather than buffered in
// full.
func TestFetchRejectsOversizedManifest(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Emit a valid-looking JSON prefix, then pad past the manifest limit so
		// the bound trips before decoding completes.
		_, _ = w.Write([]byte(`{"channels":{},"_pad":"`))
		chunk := make([]byte, 1<<20) // 1 MiB
		for i := range chunk {
			chunk[i] = 'a'
		}
		var written int64
		for written <= maxManifestBytes {
			n, err := w.Write(chunk)
			if err != nil {
				return
			}
			written += int64(n)
		}
	}))
	defer server.Close()

	_, err := Fetch(context.Background(), server.URL)
	if err == nil {
		t.Fatalf("expected oversized manifest to be rejected")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}
