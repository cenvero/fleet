// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package update

import (
	"bytes"
	"strings"
	"testing"
)

func TestBinaryForSelectsCurrentTarget(t *testing.T) {
	t.Parallel()

	target := runtimeUpdateTarget()
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

// TestManifestBodyLimitRejectsOversizedResponse proves manifests are bounded
// before JSON decoding without making an insecure network request.
func TestManifestBodyLimitRejectsOversizedResponse(t *testing.T) {
	t.Parallel()
	_, err := readBoundedHTTPBody(bytes.NewReader(make([]byte, maxManifestBytes+1)), maxManifestBytes, "manifest")
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

func TestBinaryForTargetCanonicalizesARMv7(t *testing.T) {
	t.Parallel()
	manifest := Manifest{
		Channels: map[string]ChannelInfo{"stable": {Version: "v2.0.0"}},
		Binaries: map[string]map[string]BinaryInfo{"v2.0.0": {
			"linux-armv7": {URL: "https://example.com/fleet_linux_armv7.tar.gz"},
		}},
	}
	_, binary, err := manifest.BinaryForTarget("stable", false, "linux", "arm")
	if err != nil {
		t.Fatalf("BinaryForTarget(linux, arm) error = %v", err)
	}
	if !strings.Contains(binary.URL, "armv7") {
		t.Fatalf("selected URL = %q, want armv7 artifact", binary.URL)
	}
}
