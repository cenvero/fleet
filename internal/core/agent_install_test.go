// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"aead.dev/minisign"
	"github.com/cenvero/fleet/internal/update"
)

func TestVerifyAgentBinariesRequiresSignedCompleteBoundMatrix(t *testing.T) {
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubText, err := pub.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	manifest := update.Manifest{AgentBinaries: map[string]map[string]update.BinaryInfo{"v1.2.3": {}}}
	payloads := map[string][]byte{}
	for _, target := range agentLinuxTargets {
		archive := agentTestArchive(t, []byte("binary-"+target.name))
		sum := sha256.Sum256(archive)
		url := "https://github.com/cenvero/fleet/releases/download/v1.2.3/fleet-agent_1.2.3_linux_" + target.arch + ".tar.gz"
		sigURL := url + ".minisig"
		manifest.AgentBinaries["v1.2.3"][target.name] = update.BinaryInfo{
			URL: url, Signature: sigURL, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(archive)),
		}
		payloads[url] = archive
		payloads[sigURL] = minisign.SignWithComments(priv, archive, "cenvero-fleet fleet-agent v1.2.3 "+target.name, "test")
	}
	downloader := func(_ context.Context, url string) ([]byte, error) { return payloads[url], nil }
	got, err := verifyAgentBinaries(context.Background(), "1.2.3", manifest, downloader, string(pubText))
	if err != nil {
		t.Fatalf("verifyAgentBinaries() error = %v", err)
	}
	if len(got) != len(agentLinuxTargets) {
		t.Fatalf("verified %d binaries, want %d", len(got), len(agentLinuxTargets))
	}

	delete(manifest.AgentBinaries["v1.2.3"], "linux-armv7")
	if _, err := verifyAgentBinaries(context.Background(), "1.2.3", manifest, downloader, string(pubText)); err == nil {
		t.Fatal("expected incomplete agent matrix to fail closed")
	}
}

func TestVerifyAgentBinariesRejectsWrongTrustedBinding(t *testing.T) {
	pub, priv, _ := minisign.GenerateKey(rand.Reader)
	pubText, _ := pub.MarshalText()
	manifest := update.Manifest{AgentBinaries: map[string]map[string]update.BinaryInfo{"v1.2.3": {}}}
	payloads := map[string][]byte{}
	for _, target := range agentLinuxTargets {
		archive := agentTestArchive(t, []byte(target.name))
		sum := sha256.Sum256(archive)
		url := "https://github.com/cenvero/fleet/releases/download/v1.2.3/fleet-agent_1.2.3_linux_" + target.arch + ".tar.gz"
		manifest.AgentBinaries["v1.2.3"][target.name] = update.BinaryInfo{URL: url, Signature: url + ".minisig", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(archive))}
		payloads[url] = archive
		comment := "cenvero-fleet fleet-agent v1.2.3 " + target.name
		if target.name == "linux-amd64" {
			comment = "cenvero-fleet fleet-agent v9.9.9 linux-amd64"
		}
		payloads[url+".minisig"] = minisign.SignWithComments(priv, archive, comment, "test")
	}
	downloader := func(_ context.Context, url string) ([]byte, error) { return payloads[url], nil }
	if _, err := verifyAgentBinaries(context.Background(), "v1.2.3", manifest, downloader, string(pubText)); err == nil {
		t.Fatal("expected wrong trusted comment binding to fail closed")
	}
}

func TestValidateAgentReleaseURL(t *testing.T) {
	for _, raw := range []string{"http://github.com/a", "https://example.com/a", "file:///tmp/a", "https://user@github.com/a"} {
		if err := validateAgentReleaseURL(raw); err == nil {
			t.Fatalf("expected %q to be rejected", raw)
		}
	}
	if err := validateAgentReleaseURL("https://release-assets.githubusercontent.com/a"); err != nil {
		t.Fatal(err)
	}
}

func agentTestArchive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "fleet-agent", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
