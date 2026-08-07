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
	"os"
	"path/filepath"
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
	signature := minisign.SignWithComments(priv, archive, "cenvero-fleet fleet v1.2.3 "+runtimeUpdateTarget(), "test")
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
				runtimeUpdateTarget(): {
					URL:       "https://example.invalid/fleet.tar.gz",
					SHA256:    hex.EncodeToString(sum[:]),
					Size:      int64(len(archive)),
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
				runtimeUpdateTarget(): {URL: "https://example.invalid/fleet.tar.gz"},
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
	signingKey, signature := testSignature(t, archive, "v1.2.4")
	sum := sha256.Sum256(archive)
	manifest := Manifest{
		Channels: map[string]ChannelInfo{
			"stable": {Version: "v1.2.4"},
		},
		Binaries: map[string]map[string]BinaryInfo{
			"v1.2.4": {
				runtimeUpdateTarget(): {
					URL:       "https://example.invalid/fleet.tar.gz",
					Signature: "https://example.invalid/fleet.tar.gz.minisig",
					SHA256:    hex.EncodeToString(sum[:]),
					Size:      int64(len(archive)),
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
	signingKey, signature := testSignature(t, []byte("different-payload"), "v1.2.4")
	sum := sha256.Sum256(archive)
	manifest := Manifest{
		Channels: map[string]ChannelInfo{
			"stable": {Version: "v1.2.4"},
		},
		Binaries: map[string]map[string]BinaryInfo{
			"v1.2.4": {
				runtimeUpdateTarget(): {
					URL:       "https://example.invalid/fleet.tar.gz",
					Signature: "https://example.invalid/fleet.tar.gz.minisig",
					SHA256:    hex.EncodeToString(sum[:]),
					Size:      int64(len(archive)),
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

func testSignature(t *testing.T, payload []byte, version string) (string, []byte) {
	t.Helper()

	publicKey, privateKey, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	publicKeyText, err := publicKey.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText() error = %v", err)
	}
	trustedComment := "cenvero-fleet fleet " + mustCanonicalTestVersion(t, version) + " " + runtimeUpdateTarget()
	return string(publicKeyText), minisign.SignWithComments(privateKey, payload, trustedComment, "test")
}

func mustCanonicalTestVersion(t *testing.T, version string) string {
	t.Helper()
	canonical, err := canonicalSemVersion(version)
	if err != nil {
		t.Fatalf("canonicalSemVersion(%q): %v", version, err)
	}
	return canonical
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
						runtimeUpdateTarget(): {
							URL:    "https://example.invalid/fleet.tar.gz",
							SHA256: hex.EncodeToString(sum[:]),
							Size:   int64(len(archive)),
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
				runtimeUpdateTarget(): {
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

// TestReadBoundedHTTPBodyRejectsOversizedArtifact proves response bodies are
// bounded before buffering without weakening the HTTPS/SSRF network policy.
func TestReadBoundedHTTPBodyRejectsOversizedArtifact(t *testing.T) {
	const limit = 8 << 20
	_, err := readBoundedHTTPBody(bytes.NewReader(make([]byte, limit+1)), limit, "artifact")
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

func TestValidateDownloadSchemeHTTPSOnly(t *testing.T) {
	t.Parallel()
	if err := validateDownloadScheme("https://fleet.cenvero.org/fleet.tar.gz"); err != nil {
		t.Fatalf("valid HTTPS URL rejected: %v", err)
	}
	for _, u := range []string{
		"http://localhost:8080/fleet.tar.gz",
		"http://127.0.0.1/fleet.tar.gz",
		"file:///etc/passwd",
		"ftp://example.invalid/fleet.tar.gz",
		"https://user@example.invalid/file",
		"://broken",
	} {
		if err := validateDownloadScheme(u); err == nil {
			t.Fatalf("validateDownloadScheme(%q) = nil, want error", u)
		}
	}
}

func TestCompareVersionsStrictSemVer(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "v1.2.3", 0},
		{"1.2.10", "1.2.3", 1},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1},
		{"1.0.0-beta.2", "1.0.0-beta.11", -1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0+build.1", "1.0.0+build.2", 0},
	}
	for _, c := range cases {
		got, err := CompareSemVer(c.a, c.b)
		if err != nil {
			t.Errorf("CompareSemVer(%q,%q) error = %v", c.a, c.b, err)
			continue
		}
		if got != c.want {
			t.Errorf("CompareSemVer(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
	for _, malformed := range []string{"", "1", "1.0", "01.2.3", "1.2.3-01", "v1.2.x", "V1.2.3"} {
		if _, err := CompareSemVer(malformed, "1.0.0"); err == nil {
			t.Errorf("malformed version %q did not fail closed", malformed)
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
	pubText, signature := testSignature(t, archive, "1.0.0")
	const sigURL = "https://example.invalid/fleet.tar.gz.minisig"
	manifest := Manifest{
		Channels: map[string]ChannelInfo{"stable": {Version: "1.0.0"}},
		Binaries: map[string]map[string]BinaryInfo{
			"1.0.0": {runtimeUpdateTarget(): {
				URL: "https://example.invalid/fleet.tar.gz", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(archive)), Signature: sigURL,
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

// TestAssertSignedComment verifies exact product, trusted role, canonical version,
// and canonical target binding, including reciprocal role-confusion attacks.
func TestAssertSignedComment(t *testing.T) {
	_, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	payload := []byte("fleet-archive-bytes")
	target := runtimeUpdateTarget()
	sign := func(trusted string) []byte {
		return minisign.SignWithComments(priv, payload, trusted, "test")
	}
	expected := "cenvero-fleet fleet v1.4.0 " + target
	if err := assertSignedComment(sign(expected), artifactRoleFleet, "1.4.0", target); err != nil {
		t.Fatalf("exact binding rejected: %v", err)
	}
	attacks := []struct {
		name    string
		comment string
		role    artifactRole
	}{
		{"fleet signature cannot update agent", expected, artifactRoleFleetAgent},
		{"agent signature cannot update fleet", "cenvero-fleet fleet-agent v1.4.0 " + target, artifactRoleFleet},
		{"wrong product", "other-product fleet v1.4.0 " + target, artifactRoleFleet},
		{"wrong version", "cenvero-fleet fleet v1.4.1 " + target, artifactRoleFleet},
		{"noncanonical version", "cenvero-fleet fleet 1.4.0 " + target, artifactRoleFleet},
		{"wrong target", "cenvero-fleet fleet v1.4.0 linux-arm64", artifactRoleFleet},
		{"extra token", expected + " extra", artifactRoleFleet},
		{"case change", "Cenvero-Fleet fleet v1.4.0 " + target, artifactRoleFleet},
		{"empty", "", artifactRoleFleet},
	}
	for _, attack := range attacks {
		t.Run(attack.name, func(t *testing.T) {
			if err := assertSignedComment(sign(attack.comment), attack.role, "v1.4.0", target); err == nil {
				t.Fatalf("accepted trusted comment %q for role %q", attack.comment, attack.role)
			}
		})
	}
	if got := canonicalUpdateTarget("linux", "arm"); got != "linux-armv7" {
		t.Fatalf("canonical target = %q, want linux-armv7", got)
	}
}

func TestApplyRejectsOldSignedArtifactAdvertisedAsNewVersion(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	executablePath := filepath.Join(t.TempDir(), runtimeExecutableName())
	original := []byte("current-v2")
	if err := os.WriteFile(executablePath, original, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := tarGzArchive(t, runtimeExecutableName(), []byte("genuine-old-v1"), 0o755)
	publicKey, privateKey, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicText, _ := publicKey.MarshalText()
	signature := minisign.SignWithComments(privateKey, archive, "cenvero-fleet fleet v1.0.0 "+runtimeUpdateTarget(), "test")
	sum := sha256.Sum256(archive)
	manifest := Manifest{
		Channels: map[string]ChannelInfo{"stable": {Version: "v9.0.0"}},
		Binaries: map[string]map[string]BinaryInfo{"v9.0.0": {
			runtimeUpdateTarget(): {
				URL: "https://example.invalid/fleet.tar.gz", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(archive)), Signature: "https://example.invalid/fleet.tar.gz.minisig",
			},
		}},
	}
	_, err = Apply(context.Background(), ApplyOptions{
		Channel: "stable", ConfigDir: configDir, ExecutablePath: executablePath,
		CurrentVersion: "v2.0.0", SigningPublicKey: string(publicText),
		FetchManifest: func(context.Context, string) (Manifest, error) { return manifest, nil },
		DownloadURL: func(_ context.Context, rawURL string) ([]byte, error) {
			if strings.HasSuffix(rawURL, ".minisig") {
				return signature, nil
			}
			return archive, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "does not exactly match") {
		t.Fatalf("expected fake-version replay rejection, got %v", err)
	}
	got, readErr := os.ReadFile(executablePath)
	if readErr != nil || !bytes.Equal(got, original) {
		t.Fatalf("replay changed executable: data=%q err=%v", got, readErr)
	}
}

func TestUpdateRedirectPolicy(t *testing.T) {
	t.Parallel()
	request := func(rawURL string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatalf("NewRequest(%q): %v", rawURL, err)
		}
		return req
	}

	t.Run("rejects HTTPS downgrade", func(t *testing.T) {
		err := checkUpdateRedirect(request("http://127.0.0.1/artifact"), []*http.Request{request("https://93.184.216.34/artifact")})
		if err == nil || !strings.Contains(err.Error(), "only https") {
			t.Fatalf("expected downgrade rejection, got %v", err)
		}
	})
	t.Run("rejects private target pivot", func(t *testing.T) {
		err := checkUpdateRedirect(request("https://127.0.0.1/artifact"), []*http.Request{request("https://93.184.216.34/artifact")})
		if err == nil || !strings.Contains(err.Error(), "private/internal") {
			t.Fatalf("expected private-target rejection, got %v", err)
		}
	})
	t.Run("bounds redirect count", func(t *testing.T) {
		via := make([]*http.Request, maxUpdateRedirects+1)
		for i := range via {
			via[i] = request("https://93.184.216.34/artifact")
		}
		err := checkUpdateRedirect(request("https://93.184.216.35/artifact"), via)
		if err == nil || !strings.Contains(err.Error(), "maximum") {
			t.Fatalf("expected redirect-limit rejection, got %v", err)
		}
	})
}

type redirectProbeTransport struct {
	forbiddenRequests int
}

func (p *redirectProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Hostname() == "127.0.0.1" {
		p.forbiddenRequests++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: req}, nil
	}
	header := make(http.Header)
	header.Set("Location", "https://127.0.0.1/internal")
	return &http.Response{StatusCode: http.StatusFound, Header: header, Body: http.NoBody, Request: req}, nil
}

func TestForbiddenRedirectEndpointReceivesZeroRequests(t *testing.T) {
	t.Parallel()
	probe := &redirectProbeTransport{}
	client := &http.Client{Transport: probe, CheckRedirect: checkUpdateRedirect}
	_, err := client.Get("https://93.184.216.34/artifact")
	if err == nil || !strings.Contains(err.Error(), "private/internal") {
		t.Fatalf("expected private redirect rejection, got %v", err)
	}
	if probe.forbiddenRequests != 0 {
		t.Fatalf("forbidden redirect endpoint received %d requests, want zero", probe.forbiddenRequests)
	}
}

func TestApplyRejectsReciprocalArchiveRoleConfusion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		agentCaller bool
		memberRole  artifactRole
	}{
		{name: "fleet caller rejects fleet-agent member", agentCaller: false, memberRole: artifactRoleFleetAgent},
		{name: "fleet-agent caller rejects fleet member", agentCaller: true, memberRole: artifactRoleFleet},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			callerRole := artifactRoleFor(tc.agentCaller)
			archive := tarGzArchive(t, canonicalRoleBinary(tc.memberRole), []byte("wrong-role-binary"), 0o755)
			sum := sha256.Sum256(archive)
			pub, priv, err := minisign.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			pubText, _ := pub.MarshalText()
			sigURL := "https://example.invalid/artifact.minisig"
			signature := minisign.SignWithComments(priv, archive,
				"cenvero-fleet "+string(callerRole)+" v2.0.0 "+runtimeUpdateTarget(), "test")
			entry := BinaryInfo{
				URL: "https://example.invalid/artifact.tar.gz", Signature: sigURL,
				SHA256: hex.EncodeToString(sum[:]), Size: int64(len(archive)),
				DisplayName: canonicalRoleBinary(tc.memberRole), // attacker-controlled and ignored
			}
			manifest := Manifest{Channels: map[string]ChannelInfo{"stable": {Version: "v2.0.0"}}}
			if tc.agentCaller {
				manifest.AgentBinaries = map[string]map[string]BinaryInfo{"v2.0.0": {runtimeUpdateTarget(): entry}}
			} else {
				manifest.Binaries = map[string]map[string]BinaryInfo{"v2.0.0": {runtimeUpdateTarget(): entry}}
			}
			executablePath := filepath.Join(t.TempDir(), canonicalRoleBinary(callerRole))
			if err := os.WriteFile(executablePath, []byte("original"), 0o755); err != nil {
				t.Fatal(err)
			}
			_, err = Apply(context.Background(), ApplyOptions{
				Channel: "stable", ConfigDir: t.TempDir(), ExecutablePath: executablePath,
				CurrentVersion: "v1.0.0", AgentBinary: tc.agentCaller, SigningPublicKey: string(pubText),
				FetchManifest: func(context.Context, string) (Manifest, error) { return manifest, nil },
				DownloadURL: func(_ context.Context, url string) ([]byte, error) {
					if url == sigURL {
						return signature, nil
					}
					return archive, nil
				},
			})
			if err == nil || !strings.Contains(err.Error(), "binary payload not found") {
				t.Fatalf("expected canonical role extraction rejection, got %v", err)
			}
			got, readErr := os.ReadFile(executablePath)
			if readErr != nil || string(got) != "original" {
				t.Fatalf("role-confusion attempt changed executable: %q, %v", got, readErr)
			}
		})
	}
}

func TestApplyReplacementFailureRestoresExecutable(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	executablePath := filepath.Join(t.TempDir(), runtimeExecutableName())
	original := []byte("original-binary")
	if err := os.WriteFile(executablePath, original, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := tarGzArchive(t, runtimeExecutableName(), []byte("replacement-binary"), 0o755)
	manifest := Manifest{
		Channels: map[string]ChannelInfo{"stable": {Version: "v2.0.0"}},
		Binaries: map[string]map[string]BinaryInfo{"v2.0.0": {
			runtimeUpdateTarget(): {URL: "https://example.invalid/fleet.tar.gz"},
		}},
	}
	ops := defaultUpdateFileOps()
	calls := 0
	ops.replace = func(stagedPath, targetPath string) error {
		calls++
		if calls == 1 {
			if err := os.Remove(targetPath); err != nil {
				return err
			}
			return os.ErrPermission
		}
		return replaceFile(stagedPath, targetPath)
	}
	_, err := Apply(context.Background(), ApplyOptions{
		Channel: "stable", ConfigDir: configDir, ExecutablePath: executablePath,
		CurrentVersion: "v1.0.0", AllowUnsigned: true, fileOps: ops,
		FetchManifest: func(context.Context, string) (Manifest, error) { return manifest, nil },
		DownloadURL:   func(context.Context, string) ([]byte, error) { return archive, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "replace executable") {
		t.Fatalf("expected injected replacement failure, got %v", err)
	}
	got, readErr := os.ReadFile(executablePath)
	if readErr != nil || !bytes.Equal(got, original) {
		t.Fatalf("original executable was not restored: data=%q err=%v", got, readErr)
	}
	if _, statErr := os.Stat(updateJournalPath(configDir)); !os.IsNotExist(statErr) {
		t.Fatalf("aborted transaction journal remains: %v", statErr)
	}
	if _, statErr := os.Stat(rollbackStatePath(configDir)); !os.IsNotExist(statErr) {
		t.Fatalf("failed update created rollback metadata: %v", statErr)
	}
}

func TestApplyRollbackStateFailureRestoresExecutable(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	executablePath := filepath.Join(t.TempDir(), runtimeExecutableName())
	original := []byte("original-before-state-failure")
	if err := os.WriteFile(executablePath, original, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := tarGzArchive(t, runtimeExecutableName(), []byte("new-before-state-failure"), 0o755)
	manifest := Manifest{
		Channels: map[string]ChannelInfo{"stable": {Version: "v2.0.0"}},
		Binaries: map[string]map[string]BinaryInfo{"v2.0.0": {
			runtimeUpdateTarget(): {URL: "https://example.invalid/fleet.tar.gz"},
		}},
	}
	ops := defaultUpdateFileOps()
	ops.writeRollbackState = func(string, rollbackState) error { return os.ErrPermission }
	_, err := Apply(context.Background(), ApplyOptions{
		Channel: "stable", ConfigDir: configDir, ExecutablePath: executablePath,
		CurrentVersion: "v1.0.0", AllowUnsigned: true, fileOps: ops,
		FetchManifest: func(context.Context, string) (Manifest, error) { return manifest, nil },
		DownloadURL:   func(context.Context, string) ([]byte, error) { return archive, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "write rollback state") {
		t.Fatalf("expected rollback-state failure, got %v", err)
	}
	got, readErr := os.ReadFile(executablePath)
	if readErr != nil || !bytes.Equal(got, original) {
		t.Fatalf("state failure did not restore executable: data=%q err=%v", got, readErr)
	}
}

func TestRecoverInterruptedApplyRestoresOriginal(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	executablePath := filepath.Join(t.TempDir(), runtimeExecutableName())
	original := []byte("original-before-crash")
	if err := os.WriteFile(executablePath, []byte("replacement-before-crash"), 0o755); err != nil {
		t.Fatal(err)
	}
	recoveryPath := filepath.Join(t.TempDir(), "original.bak")
	if err := os.WriteFile(recoveryPath, original, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := rollbackStatePath(configDir)
	journal := updateJournal{
		Operation:         transactionApply,
		ExecutablePath:    executablePath,
		RecoveryPath:      recoveryPath,
		StagedPath:        executablePath + ".new",
		RollbackStatePath: statePath,
		AppliedVersion:    "v2.0.0",
	}
	if err := writeJSONAtomic(updateJournalPath(configDir), journal, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverInterruptedUpdate(configDir, defaultUpdateFileOps()); err == nil || !strings.Contains(err.Error(), "recovered interrupted") {
		t.Fatalf("expected recovery notice, got %v", err)
	}
	got, err := os.ReadFile(executablePath)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("interrupted update was not restored: data=%q err=%v", got, err)
	}
	if _, err := os.Stat(updateJournalPath(configDir)); !os.IsNotExist(err) {
		t.Fatalf("recovered journal remains: %v", err)
	}
}
