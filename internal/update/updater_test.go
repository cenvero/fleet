// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"aead.dev/minisign"
)

func TestApplyAndRollback(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	executablePath := filepath.Join(t.TempDir(), runtimeExecutableName())
	oldBinary := []byte("old-fleet-binary")
	if err := os.WriteFile(executablePath, oldBinary, 0o755); err != nil {
		t.Fatalf("WriteFile(executable) error = %v", err)
	}

	newBinary := []byte("new-fleet-binary")
	archive := tarGzArchive(t, runtimeExecutableName(), newBinary, 0o755)
	sum := sha256.Sum256(archive)
	manifest := Manifest{
		Channels: map[string]ChannelInfo{
			"stable": {Version: "v1.2.3", ReleaseNotes: "https://example.invalid/release"},
		},
		Binaries: map[string]map[string]BinaryInfo{
			"v1.2.3": {
				runtime.GOOS + "-" + runtime.GOARCH: {
					URL:    "https://example.invalid/fleet.tar.gz",
					SHA256: hex.EncodeToString(sum[:]),
				},
			},
		},
	}

	now := time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC)
	result, err := Apply(context.Background(), ApplyOptions{
		Channel:        "stable",
		ConfigDir:      configDir,
		ExecutablePath: executablePath,
		CurrentVersion: "v1.2.2",
		FetchManifest: func(context.Context, string) (Manifest, error) {
			return manifest, nil
		},
		DownloadURL: func(context.Context, string) ([]byte, error) {
			return archive, nil
		},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected update to be applied")
	}
	if result.SignatureVerified {
		t.Fatalf("expected signature verification to be skipped without a signature URL")
	}
	current, err := os.ReadFile(executablePath)
	if err != nil {
		t.Fatalf("ReadFile(executable) error = %v", err)
	}
	if string(current) != string(newBinary) {
		t.Fatalf("expected executable contents to be updated")
	}
	if _, err := os.Stat(result.BackupPath); err != nil {
		t.Fatalf("expected backup path to exist: %v", err)
	}
	if _, err := os.Stat(result.RollbackState); err != nil {
		t.Fatalf("expected rollback state to exist: %v", err)
	}

	rollback, err := Rollback(configDir, executablePath)
	if err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if !rollback.Restored {
		t.Fatalf("expected rollback to restore backup")
	}
	current, err = os.ReadFile(executablePath)
	if err != nil {
		t.Fatalf("ReadFile(executable after rollback) error = %v", err)
	}
	if string(current) != string(oldBinary) {
		t.Fatalf("expected rollback to restore original binary")
	}
	if _, err := os.Stat(result.RollbackState); !os.IsNotExist(err) {
		t.Fatalf("expected rollback state to be removed, got %v", err)
	}
}

func TestApplyNoOpWhenVersionMatches(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	executablePath := filepath.Join(t.TempDir(), runtimeExecutableName())
	oldBinary := []byte("old-fleet-binary")
	if err := os.WriteFile(executablePath, oldBinary, 0o755); err != nil {
		t.Fatalf("WriteFile(executable) error = %v", err)
	}

	manifest := Manifest{
		Channels: map[string]ChannelInfo{
			"stable": {Version: "v1.2.3"},
		},
		Binaries: map[string]map[string]BinaryInfo{
			"v1.2.3": {
				runtime.GOOS + "-" + runtime.GOARCH: {URL: "https://example.invalid/fleet.tar.gz"},
			},
		},
	}

	result, err := Apply(context.Background(), ApplyOptions{
		Channel:        "stable",
		ConfigDir:      configDir,
		ExecutablePath: executablePath,
		CurrentVersion: "v1.2.3",
		FetchManifest: func(context.Context, string) (Manifest, error) {
			return manifest, nil
		},
		DownloadURL: func(context.Context, string) ([]byte, error) {
			t.Fatalf("DownloadURL should not be called when already current")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if result.Applied {
		t.Fatalf("expected update to be a no-op when versions match")
	}
	if result.SignatureVerified {
		t.Fatalf("expected signature verification to be skipped on no-op update")
	}
}

func TestApplyVerifiesMinisignSignature(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	executablePath := filepath.Join(t.TempDir(), runtimeExecutableName())
	if err := os.WriteFile(executablePath, []byte("old-fleet-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile(executable) error = %v", err)
	}

	archive := tarGzArchive(t, runtimeExecutableName(), []byte("signed-fleet-binary"), 0o755)
	signingKey, signature := testSignature(t, archive)
	sum := sha256.Sum256(archive)
	manifest := Manifest{
		Channels: map[string]ChannelInfo{
			"stable": {Version: "v1.2.4"},
		},
		Binaries: map[string]map[string]BinaryInfo{
			"v1.2.4": {
				runtime.GOOS + "-" + runtime.GOARCH: {
					URL:       "https://example.invalid/fleet.tar.gz",
					Signature: "https://example.invalid/fleet.tar.gz.minisig",
					SHA256:    hex.EncodeToString(sum[:]),
				},
			},
		},
	}

	result, err := Apply(context.Background(), ApplyOptions{
		Channel:          "stable",
		ConfigDir:        configDir,
		ExecutablePath:   executablePath,
		CurrentVersion:   "v1.2.3",
		SigningPublicKey: signingKey,
		FetchManifest: func(context.Context, string) (Manifest, error) {
			return manifest, nil
		},
		DownloadURL: func(_ context.Context, rawURL string) ([]byte, error) {
			switch rawURL {
			case "https://example.invalid/fleet.tar.gz":
				return archive, nil
			case "https://example.invalid/fleet.tar.gz.minisig":
				return signature, nil
			default:
				t.Fatalf("unexpected download URL %q", rawURL)
				return nil, nil
			}
		},
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected signed update to be applied")
	}
	if !result.SignatureVerified {
		t.Fatalf("expected minisign signature to be verified")
	}
}

func TestApplyRejectsInvalidMinisignSignature(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	executablePath := filepath.Join(t.TempDir(), runtimeExecutableName())
	oldBinary := []byte("old-fleet-binary")
	if err := os.WriteFile(executablePath, oldBinary, 0o755); err != nil {
		t.Fatalf("WriteFile(executable) error = %v", err)
	}

	archive := tarGzArchive(t, runtimeExecutableName(), []byte("signed-fleet-binary"), 0o755)
	signingKey, signature := testSignature(t, []byte("different-payload"))
	sum := sha256.Sum256(archive)
	manifest := Manifest{
		Channels: map[string]ChannelInfo{
			"stable": {Version: "v1.2.4"},
		},
		Binaries: map[string]map[string]BinaryInfo{
			"v1.2.4": {
				runtime.GOOS + "-" + runtime.GOARCH: {
					URL:       "https://example.invalid/fleet.tar.gz",
					Signature: "https://example.invalid/fleet.tar.gz.minisig",
					SHA256:    hex.EncodeToString(sum[:]),
				},
			},
		},
	}

	_, err := Apply(context.Background(), ApplyOptions{
		Channel:          "stable",
		ConfigDir:        configDir,
		ExecutablePath:   executablePath,
		CurrentVersion:   "v1.2.3",
		SigningPublicKey: signingKey,
		FetchManifest: func(context.Context, string) (Manifest, error) {
			return manifest, nil
		},
		DownloadURL: func(_ context.Context, rawURL string) ([]byte, error) {
			switch rawURL {
			case "https://example.invalid/fleet.tar.gz":
				return archive, nil
			case "https://example.invalid/fleet.tar.gz.minisig":
				return signature, nil
			default:
				t.Fatalf("unexpected download URL %q", rawURL)
				return nil, nil
			}
		},
	})
	if err == nil {
		t.Fatalf("expected invalid signature to fail")
	}
	if !strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("expected signature failure, got %v", err)
	}
	current, readErr := os.ReadFile(executablePath)
	if readErr != nil {
		t.Fatalf("ReadFile(executable) error = %v", readErr)
	}
	if string(current) != string(oldBinary) {
		t.Fatalf("expected invalid signature to leave executable unchanged")
	}
}

func tarGzArchive(t *testing.T, name string, payload []byte, mode os.FileMode) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: int64(mode.Perm()),
		Size: int64(len(payload)),
	}); err != nil {
		t.Fatalf("WriteHeader() error = %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("Write(payload) error = %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar.Close() error = %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip.Close() error = %v", err)
	}
	return buf.Bytes()
}

func testSignature(t *testing.T, payload []byte) (string, []byte) {
	t.Helper()

	publicKey, privateKey, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	publicKeyText, err := publicKey.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText() error = %v", err)
	}
	return string(publicKeyText), minisign.Sign(privateKey, payload)
}
