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
	"net/http"
	"net/http/httptest"
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
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey error = %v", err)
	}
	signature := minisign.Sign(priv, archive)
	pubText, err := pub.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText error = %v", err)
	}
	const sigURL = "https://example.invalid/fleet.tar.gz.minisig"
	manifest := Manifest{
		Channels: map[string]ChannelInfo{
			"stable": {Version: "v1.2.3", ReleaseNotes: "https://example.invalid/release"},
		},
		Binaries: map[string]map[string]BinaryInfo{
			"v1.2.3": {
				runtime.GOOS + "-" + runtime.GOARCH: {
					URL:       "https://example.invalid/fleet.tar.gz",
					SHA256:    hex.EncodeToString(sum[:]),
					Signature: sigURL,
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
		DownloadURL: func(_ context.Context, url string) ([]byte, error) {
			if url == sigURL {
				return signature, nil
			}
			return archive, nil
		},
		SigningPublicKey: string(pubText),
		Now:              func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected update to be applied")
	}
	if !result.SignatureVerified {
		t.Fatalf("expected signature to be verified")
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
		CurrentVersion: "1.2.3",
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

// TestApplyRefusesUnsignedWithoutOptIn verifies that an update whose manifest
// entry carries no minisign signature is refused fail-closed, on every channel,
// when AllowUnsigned is not set — even when a SHA-256 checksum is present.
func TestApplyRefusesUnsignedWithoutOptIn(t *testing.T) {
	t.Parallel()

	for _, channel := range []string{"stable", "dev", ""} {
		channel := channel
		t.Run("channel="+channel, func(t *testing.T) {
			t.Parallel()

			configDir := filepath.Join(t.TempDir(), "fleet")
			executablePath := filepath.Join(t.TempDir(), runtimeExecutableName())
			oldBinary := []byte("old-fleet-binary")
			if err := os.WriteFile(executablePath, oldBinary, 0o755); err != nil {
				t.Fatalf("WriteFile(executable) error = %v", err)
			}

			archive := tarGzArchive(t, runtimeExecutableName(), []byte("unsigned-fleet-binary"), 0o755)
			sum := sha256.Sum256(archive)
			manifest := Manifest{
				Channels: map[string]ChannelInfo{
					"stable": {Version: "v9.9.9"},
					"dev":    {Version: "v9.9.9"},
				},
				Binaries: map[string]map[string]BinaryInfo{
					"v9.9.9": {
						runtime.GOOS + "-" + runtime.GOARCH: {
							URL:    "https://example.invalid/fleet.tar.gz",
							SHA256: hex.EncodeToString(sum[:]),
							// No Signature on purpose.
						},
					},
				},
			}

			_, err := Apply(context.Background(), ApplyOptions{
				Channel:        channel,
				ConfigDir:      configDir,
				ExecutablePath: executablePath,
				CurrentVersion: "v1.0.0",
				FetchManifest: func(context.Context, string) (Manifest, error) {
					return manifest, nil
				},
				DownloadURL: func(_ context.Context, rawURL string) ([]byte, error) {
					return archive, nil
				},
			})
			if err == nil {
				t.Fatalf("expected unsigned update to be refused without opt-in")
			}
			if !strings.Contains(err.Error(), "no minisign signature") {
				t.Fatalf("expected missing-signature error, got %v", err)
			}
			current, readErr := os.ReadFile(executablePath)
			if readErr != nil {
				t.Fatalf("ReadFile(executable) error = %v", readErr)
			}
			if string(current) != string(oldBinary) {
				t.Fatalf("expected refused update to leave executable unchanged")
			}
		})
	}
}

// TestApplyAllowsUnsignedWithExplicitOptIn verifies that an unsigned update is
// applied only when the explicit AllowUnsigned opt-in is set.
func TestApplyAllowsUnsignedWithExplicitOptIn(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	executablePath := filepath.Join(t.TempDir(), runtimeExecutableName())
	if err := os.WriteFile(executablePath, []byte("old-fleet-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile(executable) error = %v", err)
	}

	newBinary := []byte("unsigned-but-allowed")
	archive := tarGzArchive(t, runtimeExecutableName(), newBinary, 0o755)
	manifest := Manifest{
		Channels: map[string]ChannelInfo{
			"stable": {Version: "v2.0.0"},
		},
		Binaries: map[string]map[string]BinaryInfo{
			"v2.0.0": {
				runtime.GOOS + "-" + runtime.GOARCH: {
					URL: "https://example.invalid/fleet.tar.gz",
				},
			},
		},
	}

	result, err := Apply(context.Background(), ApplyOptions{
		Channel:        "stable",
		ConfigDir:      configDir,
		ExecutablePath: executablePath,
		CurrentVersion: "v1.0.0",
		AllowUnsigned:  true,
		FetchManifest: func(context.Context, string) (Manifest, error) {
			return manifest, nil
		},
		DownloadURL: func(_ context.Context, rawURL string) ([]byte, error) {
			return archive, nil
		},
	})
	if err != nil {
		t.Fatalf("Apply() with AllowUnsigned error = %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected unsigned update to be applied with opt-in")
	}
	if result.SignatureVerified {
		t.Fatalf("did not expect signature verification on unsigned update")
	}
	current, err := os.ReadFile(executablePath)
	if err != nil {
		t.Fatalf("ReadFile(executable) error = %v", err)
	}
	if string(current) != string(newBinary) {
		t.Fatalf("expected executable to be updated under opt-in")
	}
}

// TestDownloadURLRejectsFileScheme verifies that file:// URLs are rejected
// (no local-file read / LFI), along with other non-https schemes.
func TestDownloadURLRejectsFileScheme(t *testing.T) {
	t.Parallel()

	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("top-secret"), 0o600); err != nil {
		t.Fatalf("WriteFile(secret) error = %v", err)
	}

	for _, rawURL := range []string{
		"file://" + secret,
		"file:///etc/passwd",
		"ftp://example.invalid/fleet.tar.gz",
		"gopher://example.invalid/x",
		"http://example.invalid/fleet.tar.gz",
	} {
		rawURL := rawURL
		t.Run(rawURL, func(t *testing.T) {
			t.Parallel()
			if _, err := downloadURL(context.Background(), rawURL); err == nil {
				t.Fatalf("expected download of %q to be rejected", rawURL)
			}
		})
	}
}

// TestDownloadURLRejectsOversizedArtifact verifies that downloadURL bounds the
// artifact body before buffering: an oversized response is rejected with a
// clear error rather than buffered in full (OOM protection). The server runs on
// loopback http://, which the scheme allowlist permits for local mirrors, so
// the real downloadURL code path (LimitReader + size check) is exercised.
func TestDownloadURLRejectsOversizedArtifact(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping multi-GiB streaming test in -short mode")
	}

	// Stream just past maxArtifactBytes without allocating it all up front.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		chunk := make([]byte, 1<<20) // 1 MiB
		var written int64
		for written <= maxArtifactBytes {
			n, err := w.Write(chunk)
			if err != nil {
				return
			}
			written += int64(n)
		}
	}))
	defer server.Close()

	_, err := downloadURL(context.Background(), server.URL)
	if err == nil {
		t.Fatalf("expected oversized artifact to be rejected")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

// TestValidateDownloadSchemeAllowsHTTPSAndLoopback documents the allowlist:
// https everywhere, http only for loopback mirrors, everything else rejected.
func TestValidateDownloadSchemeAllowsHTTPSAndLoopback(t *testing.T) {
	t.Parallel()

	allowed := []string{
		"https://fleet.cenvero.org/fleet.tar.gz",
		"http://localhost:8080/fleet.tar.gz",
		"http://127.0.0.1/fleet.tar.gz",
		"http://[::1]:9000/fleet.tar.gz",
	}
	for _, u := range allowed {
		if err := validateDownloadScheme(u); err != nil {
			t.Fatalf("validateDownloadScheme(%q) = %v, want nil", u, err)
		}
	}

	rejected := []string{
		"file:///etc/passwd",
		"http://example.invalid/fleet.tar.gz",
		"ftp://example.invalid/fleet.tar.gz",
		"://broken",
	}
	for _, u := range rejected {
		if err := validateDownloadScheme(u); err == nil {
			t.Fatalf("validateDownloadScheme(%q) = nil, want error", u)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"v1.2.3", "1.2.3", 0},
		{"1.2.10", "1.2.3", 1}, // numeric, not lexical
		{"1.2.3", "1.2.10", -1},
		{"2.0.0", "1.9.9", 1},
		{"1.2.3-rc1", "1.2.3", 0}, // pre-release suffix dropped
		{"1.0", "1.0.0", 0},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestApplyRefusesDowngrade(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	executablePath := filepath.Join(t.TempDir(), runtimeExecutableName())
	if err := os.WriteFile(executablePath, []byte("current-2.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	archive := tarGzArchive(t, runtimeExecutableName(), []byte("old-1.0.0"), 0o755)
	sum := sha256.Sum256(archive)
	pubText, signature := testSignature(t, archive)
	const sigURL = "https://example.invalid/fleet.tar.gz.minisig"
	manifest := Manifest{
		Channels: map[string]ChannelInfo{"stable": {Version: "1.0.0"}},
		Binaries: map[string]map[string]BinaryInfo{
			"1.0.0": {runtime.GOOS + "-" + runtime.GOARCH: {
				URL: "https://example.invalid/fleet.tar.gz", SHA256: hex.EncodeToString(sum[:]), Signature: sigURL,
			}},
		},
	}
	dl := func(_ context.Context, url string) ([]byte, error) {
		if url == sigURL {
			return signature, nil
		}
		return archive, nil
	}
	base := ApplyOptions{
		Channel: "stable", ConfigDir: configDir, ExecutablePath: executablePath,
		CurrentVersion: "2.0.0", SigningPublicKey: pubText,
		FetchManifest: func(context.Context, string) (Manifest, error) { return manifest, nil },
		DownloadURL:   dl,
	}
	// Downgrade (2.0.0 -> 1.0.0) refused by default.
	if _, err := Apply(context.Background(), base); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("expected anti-rollback refusal, got %v", err)
	}
	// Explicit opt-in permits it.
	opt := base
	opt.AllowDowngrade = true
	res, err := Apply(context.Background(), opt)
	if err != nil {
		t.Fatalf("AllowDowngrade should permit downgrade: %v", err)
	}
	if !res.Applied {
		t.Fatal("expected the downgrade to apply under AllowDowngrade")
	}
}
