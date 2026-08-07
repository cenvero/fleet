// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package update

import (
	"context"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fixedUpdateResolver map[string][]net.IP

func (r fixedUpdateResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ips := r[host]
	out := make([]net.IPAddr, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.IPAddr{IP: ip})
	}
	return out, nil
}

func TestResolvedAddressPolicyRejectsInternalDestinations(t *testing.T) {
	t.Parallel()
	for _, blocked := range []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "100.100.100.200", "::1", "fe80::1", "fd00:ec2::254",
	} {
		blocked := blocked
		t.Run(blocked, func(t *testing.T) {
			parsed, err := url.Parse("https://updates.example/artifact")
			if err != nil {
				t.Fatal(err)
			}
			resolver := fixedUpdateResolver{"updates.example": {net.ParseIP(blocked)}}
			if err := validateUpdateDestination(context.Background(), resolver, parsed); err == nil {
				t.Fatalf("resolved address %s was not rejected", blocked)
			}
		})
	}

	parsed, _ := url.Parse("https://updates.example/artifact")
	mixed := fixedUpdateResolver{"updates.example": {net.ParseIP("93.184.216.34"), net.ParseIP("127.0.0.1")}}
	if err := validateUpdateDestination(context.Background(), mixed, parsed); err == nil {
		t.Fatal("mixed public/private DNS answer did not fail closed")
	}
	public := fixedUpdateResolver{"updates.example": {net.ParseIP("93.184.216.34")}}
	if err := validateUpdateDestination(context.Background(), public, parsed); err != nil {
		t.Fatalf("public-only DNS answer rejected: %v", err)
	}
}

func TestApplyRequiresHashAndSizeBeforeDownload(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		info BinaryInfo
	}{
		{name: "missing sha256", info: BinaryInfo{URL: "https://example.invalid/fleet.tar.gz", Signature: "https://example.invalid/fleet.tar.gz.minisig", Size: 123}},
		{name: "missing size", info: BinaryInfo{URL: "https://example.invalid/fleet.tar.gz", Signature: "https://example.invalid/fleet.tar.gz.minisig", SHA256: strings.Repeat("a", 64)}},
		{name: "malformed sha256", info: BinaryInfo{URL: "https://example.invalid/fleet.tar.gz", Signature: "https://example.invalid/fleet.tar.gz.minisig", SHA256: "not-a-hash", Size: 123}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			executable := filepath.Join(t.TempDir(), runtimeExecutableName())
			if err := os.WriteFile(executable, []byte("original"), 0o755); err != nil {
				t.Fatal(err)
			}
			manifest := Manifest{
				Channels: map[string]ChannelInfo{"stable": {Version: "v2.0.0"}},
				Binaries: map[string]map[string]BinaryInfo{"v2.0.0": {runtimeUpdateTarget(): tc.info}},
			}
			downloads := 0
			_, err := Apply(context.Background(), ApplyOptions{
				Channel: "stable", ConfigDir: t.TempDir(), ExecutablePath: executable, CurrentVersion: "v1.0.0",
				FetchManifest: func(context.Context, string) (Manifest, error) { return manifest, nil },
				DownloadURL:   func(context.Context, string) ([]byte, error) { downloads++; return nil, nil },
			})
			if err == nil {
				t.Fatal("incomplete integrity metadata was accepted")
			}
			if downloads != 0 {
				t.Fatalf("made %d downloads before rejecting metadata, want zero", downloads)
			}
		})
	}
}

func TestApplyRejectsMalformedMinimumVersionDespiteOverrides(t *testing.T) {
	t.Parallel()
	executable := filepath.Join(t.TempDir(), runtimeExecutableName())
	if err := os.WriteFile(executable, []byte("original"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{
		Channels: map[string]ChannelInfo{"stable": {Version: "v1.0.0", MinSupported: "not-semver"}},
		Binaries: map[string]map[string]BinaryInfo{"v1.0.0": {runtimeUpdateTarget(): {URL: "https://example.invalid/fleet.tar.gz"}}},
	}
	_, err := Apply(context.Background(), ApplyOptions{
		Channel: "stable", ConfigDir: t.TempDir(), ExecutablePath: executable,
		CurrentVersion: "v1.0.0", AllowDowngrade: true, AllowUnsigned: true,
		FetchManifest: func(context.Context, string) (Manifest, error) { return manifest, nil },
		DownloadURL: func(context.Context, string) ([]byte, error) {
			t.Fatal("malformed min_supported must fail before download")
			return nil, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid minimum supported version") {
		t.Fatalf("malformed min_supported did not fail closed: %v", err)
	}
}
