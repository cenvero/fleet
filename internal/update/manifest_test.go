// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package update

import (
	"runtime"
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
