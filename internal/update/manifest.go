// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"
)

const DefaultManifestURL = "https://fleet.cenvero.org/manifest.json"

// maxManifestBytes bounds how much of the manifest body we will read/decode.
// The manifest is a small JSON document; 8 MiB is far more than any legitimate
// release manifest needs while preventing a malicious or misconfigured server
// from streaming an unbounded body and exhausting memory.
const maxManifestBytes = 8 << 20 // 8 MiB

type Policy string

const (
	PolicyAutoUpdate Policy = "auto-update"
	PolicyNotifyOnly Policy = "notify-only"
	PolicyDisabled   Policy = "disabled"
)

type ChannelInfo struct {
	Version      string `json:"version"`
	ReleaseDate  string `json:"release_date"`
	MinSupported string `json:"min_supported"`
	ReleaseNotes string `json:"release_notes_url"`
}

type BinaryInfo struct {
	URL         string `json:"url"`
	SHA256      string `json:"sha256"`
	Signature   string `json:"signature_url"`
	Size        int64  `json:"size"`
	DisplayName string `json:"display_name,omitempty"`
}

type Manifest struct {
	Channels      map[string]ChannelInfo           `json:"channels"`
	Binaries      map[string]map[string]BinaryInfo `json:"binaries"`
	AgentBinaries map[string]map[string]BinaryInfo `json:"agent_binaries"`
	GeneratedAt   string                           `json:"generated_at,omitempty"`
}

func ReadFile(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	return manifest, nil
}

func Fetch(ctx context.Context, manifestURL string) (Manifest, error) {
	if manifestURL == "" {
		manifestURL = DefaultManifestURL
	}
	// Same scheme allowlist as artifact downloads: https only (http only for a
	// loopback mirror). Rejects file://, ftp://, etc. so a poisoned config can't
	// point the manifest fetch at a local file or an odd scheme.
	if err := validateDownloadScheme(manifestURL); err != nil {
		return Manifest{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return Manifest{}, fmt.Errorf("create manifest request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Manifest{}, fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Manifest{}, fmt.Errorf("unexpected manifest status %s", resp.Status)
	}

	// Bound the manifest body before decoding so a malicious/oversized response
	// cannot exhaust memory. We allow one extra byte so we can distinguish a
	// body that is exactly at the limit from one that overruns it.
	limited := io.LimitReader(resp.Body, maxManifestBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest body: %w", err)
	}
	if int64(len(body)) > maxManifestBytes {
		return Manifest{}, fmt.Errorf("manifest exceeds maximum size of %d bytes", maxManifestBytes)
	}

	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	return manifest, nil
}

func (m Manifest) BinaryFor(channel string, agent bool) (string, BinaryInfo, error) {
	return m.BinaryForTarget(channel, agent, runtime.GOOS, runtime.GOARCH)
}

func (m Manifest) BinaryForTarget(channel string, agent bool, goos, goarch string) (string, BinaryInfo, error) {
	ch, ok := m.Channels[channel]
	if !ok {
		return "", BinaryInfo{}, fmt.Errorf("channel %q not found", channel)
	}

	target := goos + "-" + goarch
	binaries := m.Binaries
	if agent {
		binaries = m.AgentBinaries
	}
	versions, ok := binaries[ch.Version]
	if !ok {
		return ch.Version, BinaryInfo{}, fmt.Errorf("version %q not found for channel %q", ch.Version, channel)
	}
	binary, ok := versions[target]
	if !ok {
		return ch.Version, BinaryInfo{}, fmt.Errorf("target %q not found for version %q", target, ch.Version)
	}
	return ch.Version, binary, nil
}
