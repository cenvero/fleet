// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"aead.dev/minisign"
	"golang.org/x/mod/semver"
)

type ApplyOptions struct {
	ManifestURL      string
	Channel          string
	ConfigDir        string
	ExecutablePath   string
	CurrentVersion   string
	AgentBinary      bool
	SigningPublicKey string
	// AllowUnsigned is an explicit insecure-development opt-in that permits an
	// update with incomplete release verification metadata (signature, SHA-256,
	// or size). It defaults to false so production verification is FAIL-CLOSED.
	// It must only be set from an explicit operator action (the CLI's prominently
	// insecure --allow-unsigned flag) for local/ad-hoc development builds.
	AllowUnsigned bool
	// AllowDowngrade is an explicit opt-in that permits applying a target version
	// OLDER than the running version (or below the manifest's MinSupported floor).
	// It defaults to false so anti-rollback is enforced: a replayed/old signed
	// manifest cannot silently downgrade the binary to a known-vulnerable release.
	AllowDowngrade bool
	FetchManifest  func(context.Context, string) (Manifest, error)
	DownloadURL    func(context.Context, string) ([]byte, error)
	Now            func() time.Time

	// fileOps is injected only by package tests to simulate platform-specific
	// replacement/state failures without depending on the host OS.
	fileOps *updateFileOps
}

type ApplyResult struct {
	Channel           string `json:"channel"`
	CurrentVersion    string `json:"current_version"`
	Version           string `json:"version"`
	ExecutablePath    string `json:"executable_path"`
	BackupPath        string `json:"backup_path"`
	RollbackState     string `json:"rollback_state"`
	ReleaseNotesURL   string `json:"release_notes_url,omitempty"`
	Note              string `json:"note,omitempty"`
	Applied           bool   `json:"applied"`
	SHA256Verified    bool   `json:"sha256_verified"`
	SignatureVerified bool   `json:"signature_verified"`
}

type RollbackResult struct {
	ExecutablePath string `json:"executable_path"`
	RestoredFrom   string `json:"restored_from"`
	Version        string `json:"version"`
	Restored       bool   `json:"restored"`
}

type rollbackState struct {
	ExecutablePath  string    `json:"executable_path"`
	BackupPath      string    `json:"backup_path"`
	PreviousVersion string    `json:"previous_version"`
	AppliedVersion  string    `json:"applied_version"`
	Channel         string    `json:"channel"`
	AppliedAt       time.Time `json:"applied_at"`
}

func Apply(ctx context.Context, opts ApplyOptions) (ApplyResult, error) {
	if strings.TrimSpace(opts.Channel) == "" {
		opts.Channel = "stable"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.FetchManifest == nil {
		opts.FetchManifest = Fetch
	}
	if opts.DownloadURL == nil {
		opts.DownloadURL = downloadURL
	}
	if strings.TrimSpace(opts.SigningPublicKey) == "" {
		opts.SigningPublicKey = embeddedSigningPublicKey
	}
	if strings.TrimSpace(opts.ExecutablePath) == "" {
		path, err := os.Executable()
		if err != nil {
			return ApplyResult{}, err
		}
		opts.ExecutablePath = path
	}
	if strings.TrimSpace(opts.ConfigDir) == "" {
		return ApplyResult{}, fmt.Errorf("config dir is required")
	}
	ops := opts.fileOps
	if ops == nil {
		ops = defaultUpdateFileOps()
	}
	if err := recoverInterruptedUpdate(opts.ConfigDir, ops); err != nil {
		return ApplyResult{}, fmt.Errorf("recover interrupted update: %w", err)
	}

	manifest, err := opts.FetchManifest(ctx, opts.ManifestURL)
	if err != nil {
		return ApplyResult{}, err
	}
	version, binary, err := manifest.BinaryFor(opts.Channel, opts.AgentBinary)
	if err != nil {
		return ApplyResult{}, err
	}

	canonicalVersion, err := canonicalSemVersion(version)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("invalid manifest version %q: %w", version, err)
	}
	var canonicalCurrent string
	if strings.TrimSpace(opts.CurrentVersion) != "" {
		canonicalCurrent, err = canonicalSemVersion(opts.CurrentVersion)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("invalid current version %q: %w", opts.CurrentVersion, err)
		}
	}

	var canonicalMin string
	if minV := strings.TrimSpace(manifest.Channels[opts.Channel].MinSupported); minV != "" {
		canonicalMin, err = canonicalSemVersion(minV)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("invalid minimum supported version %q: %w", minV, err)
		}
	}

	result := ApplyResult{
		Channel:         opts.Channel,
		CurrentVersion:  opts.CurrentVersion,
		Version:         version,
		ExecutablePath:  opts.ExecutablePath,
		ReleaseNotesURL: manifest.Channels[opts.Channel].ReleaseNotes,
	}
	if canonicalCurrent != "" && semver.Compare(canonicalVersion, canonicalCurrent) == 0 {
		return result, nil
	}

	// Anti-rollback / downgrade protection uses strict SemVer precedence,
	// including numeric/non-numeric prerelease identifiers. Invalid versions are
	// rejected above rather than being coerced to zero.
	if !opts.AllowDowngrade {
		if canonicalCurrent != "" && semver.Compare(canonicalVersion, canonicalCurrent) < 0 {
			return ApplyResult{}, fmt.Errorf(
				"refusing to downgrade from %s to %s (anti-rollback); pass --allow-downgrade to override",
				opts.CurrentVersion, version)
		}
		if canonicalMin != "" && semver.Compare(canonicalVersion, canonicalMin) < 0 {
			return ApplyResult{}, fmt.Errorf(
				"target version %s is below the manifest's minimum supported version %s; refusing (pass --allow-downgrade to override)",
				version, manifest.Channels[opts.Channel].MinSupported)
		}
	}

	role := artifactRoleFor(opts.AgentBinary)
	hasSig := strings.TrimSpace(binary.Signature) != ""
	hashText := strings.TrimSpace(binary.SHA256)
	hasHash := hashText != ""
	hasSize := binary.Size > 0
	if hasHash {
		decoded, decodeErr := hex.DecodeString(hashText)
		if decodeErr != nil || len(decoded) != sha256.Size {
			return ApplyResult{}, fmt.Errorf("manifest entry for %s has an invalid SHA-256", binary.URL)
		}
	}
	if binary.Size < 0 || binary.Size > maxArtifactBytes {
		return ApplyResult{}, fmt.Errorf("manifest entry for %s has an invalid artifact size %d", binary.URL, binary.Size)
	}
	// --allow-unsigned is the explicit insecure development override. Production
	// updates require all three independent controls: signature, SHA-256, and size.
	if !opts.AllowUnsigned && (!hasHash || !hasSize) {
		return ApplyResult{}, fmt.Errorf("manifest entry for %s must include SHA-256 and size metadata", binary.URL)
	}
	if !hasSig && !opts.AllowUnsigned {
		return ApplyResult{}, fmt.Errorf(
			"manifest entry for %s has no minisign signature — refusing to apply (pass --allow-unsigned only for insecure development builds)",
			binary.URL,
		)
	}
	if opts.AllowUnsigned && (!hasSig || !hasHash || !hasSize) {
		result.Note = "WARNING: insecure development override accepted incomplete release verification metadata (--allow-unsigned)"
	}

	archive, err := opts.DownloadURL(ctx, binary.URL)
	if err != nil {
		return ApplyResult{}, err
	}
	if hasSize && int64(len(archive)) != binary.Size {
		return ApplyResult{}, fmt.Errorf("download size mismatch for %s: got %d bytes, want %d", binary.URL, len(archive), binary.Size)
	}
	if hasSig {
		signature, err := opts.DownloadURL(ctx, binary.Signature)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("download signature: %w", err)
		}
		if err := verifyMinisignSignature(opts.SigningPublicKey, archive, signature); err != nil {
			return ApplyResult{}, fmt.Errorf("verify minisign signature: %w", err)
		}
		result.SignatureVerified = true
		if err := assertSignedComment(signature, role, canonicalVersion, runtimeUpdateTarget()); err != nil {
			return ApplyResult{}, fmt.Errorf("verify minisign signature binding: %w", err)
		}
	}
	if hasHash {
		actual := sha256.Sum256(archive)
		if !strings.EqualFold(hex.EncodeToString(actual[:]), hashText) {
			return ApplyResult{}, fmt.Errorf("download sha256 mismatch for %s", binary.URL)
		}
		result.SHA256Verified = true
	}

	payload, mode, err := extractBinaryPayload(binary.URL, archive, canonicalRoleBinary(role))
	if err != nil {
		return ApplyResult{}, err
	}

	state := rollbackState{
		ExecutablePath:  opts.ExecutablePath,
		PreviousVersion: opts.CurrentVersion,
		AppliedVersion:  version,
		Channel:         opts.Channel,
		AppliedAt:       opts.Now().UTC(),
	}
	backupPath, statePath, err := installUpdate(opts, payload, mode, state)
	if err != nil {
		return ApplyResult{}, err
	}

	result.BackupPath = backupPath
	result.RollbackState = statePath
	result.Applied = true
	return result, nil
}

// canonicalSemVersion accepts an optional lowercase v prefix, requires a complete
// SemVer core (major.minor.patch), and preserves prerelease/build identifiers.
// x/mod/semver implements SemVer 2.0 precedence, including numeric prerelease
// ordering. Any malformed input returns an error so security decisions fail closed.
func canonicalSemVersion(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", fmt.Errorf("version is empty")
	}
	if strings.HasPrefix(v, "V") {
		return "", fmt.Errorf("uppercase V prefix is not canonical")
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	withoutPrefix := strings.TrimPrefix(v, "v")
	core := withoutPrefix
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("semantic version must contain major.minor.patch")
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return "", fmt.Errorf("invalid semantic version core")
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return "", fmt.Errorf("invalid semantic version core")
			}
		}
	}
	if !semver.IsValid(v) {
		return "", fmt.Errorf("not a valid semantic version")
	}
	return v, nil
}

func CompareSemVer(a, b string) (int, error) {
	av, err := canonicalSemVersion(a)
	if err != nil {
		return 0, err
	}
	bv, err := canonicalSemVersion(b)
	if err != nil {
		return 0, err
	}
	return semver.Compare(av, bv), nil
}

func Rollback(configDir, executablePath string) (RollbackResult, error) {
	return rollbackUpdate(strings.TrimSpace(configDir), strings.TrimSpace(executablePath), defaultUpdateFileOps())
}

func rollbackStatePath(configDir string) string {
	return filepath.Join(configDir, "data", "update-rollback.json")
}

func readRollbackState(path string) (rollbackState, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is derived from the configured executable/controller update transaction directory
	if err != nil {
		return rollbackState{}, fmt.Errorf("read rollback state: %w", err)
	}
	var state rollbackState
	if err := json.Unmarshal(data, &state); err != nil {
		return rollbackState{}, fmt.Errorf("decode rollback state: %w", err)
	}
	return state, nil
}

// maxArtifactBytes bounds how much of a downloaded artifact (binary archive or
// signature) we will buffer in memory. The body is wrapped in an io.LimitReader
// BEFORE the signature is verified, so a malicious or misconfigured server
// cannot stream an unbounded body and OOM the host ahead of verification. 1 GiB
// is generous for any release archive while remaining safely bounded.
//
// It is a var (not a const) only so tests can lower it to trip the limit without
// streaming a real gigabyte through loopback (which times out under -race).
var maxArtifactBytes int64 = 1 << 30 // 1 GiB

func downloadURL(ctx context.Context, rawURL string) ([]byte, error) {
	return downloadURLForHosts(ctx, rawURL, nil)
}

// DownloadApprovedURL downloads a release payload only when the initial URL and
// every redirect use one of approvedHosts. All hops also use the resolved-address
// SSRF policy and DNS-pinned transport.
func DownloadApprovedURL(ctx context.Context, rawURL string, approvedHosts ...string) ([]byte, error) {
	approved := make(map[string]bool, len(approvedHosts))
	for _, host := range approvedHosts {
		host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
		if host != "" {
			approved[host] = true
		}
	}
	if len(approved) == 0 {
		return nil, fmt.Errorf("at least one approved download host is required")
	}
	return downloadURLForHosts(ctx, rawURL, approved)
}

func downloadURLForHosts(ctx context.Context, rawURL string, approvedHosts map[string]bool) ([]byte, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("parse download url: %w", err)
	}
	if len(approvedHosts) > 0 && !approvedHosts[strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))] {
		return nil, fmt.Errorf("refusing unapproved download host %q", parsed.Hostname())
	}
	if err := validateUpdateDestination(ctx, net.DefaultResolver, parsed); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create download request: %w", err)
	}
	resp, err := newUpdateHTTPClientForHosts(30*time.Second, approvedHosts).Do(req)
	if err != nil {
		return nil, fmt.Errorf("download artifact: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected artifact status %s", resp.Status)
	}
	data, err := readBoundedHTTPBody(resp.Body, maxArtifactBytes, "artifact")
	if err != nil {
		return nil, err
	}
	return data, nil
}

func readBoundedHTTPBody(body io.Reader, limit int64, label string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s body: %w", label, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds maximum size of %d bytes", label, limit)
	}
	return data, nil
}

// validateDownloadScheme applies the URL-level half of the update network policy.
// DNS/address validation is performed separately immediately before every hop.
func validateDownloadScheme(rawURL string) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("parse download url: %w", err)
	}
	if !parsed.IsAbs() || parsed.Hostname() == "" {
		return fmt.Errorf("refusing download URL without an absolute scheme and host: %s", rawURL)
	}
	if parsed.User != nil {
		return fmt.Errorf("refusing download URL containing credentials: %s", parsed.Redacted())
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return fmt.Errorf("refusing download URL with disallowed scheme %q (only https:// is allowed): %s", parsed.Scheme, rawURL)
	}
	return nil
}

func extractBinaryPayload(sourceURL string, archive []byte, canonicalBinary string) ([]byte, os.FileMode, error) {
	targets := []string{canonicalBinary}

	switch {
	case strings.HasSuffix(sourceURL, ".zip"):
		return extractZipBinary(archive, targets)
	case strings.HasSuffix(sourceURL, ".tar.gz"), strings.HasSuffix(sourceURL, ".tgz"):
		return extractTarGzBinary(archive, targets)
	default:
		return archive, 0o755, nil
	}
}

func extractZipBinary(archive []byte, targets []string) ([]byte, os.FileMode, error) {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, 0, err
	}
	for _, target := range targets {
		for _, file := range reader.File {
			if !file.Mode().IsRegular() {
				continue
			}
			if filepath.Base(file.Name) != target {
				continue
			}
			rc, err := file.Open()
			if err != nil {
				return nil, 0, err
			}
			defer rc.Close()
			data, err := readBoundedBinary(rc)
			if err != nil {
				return nil, 0, err
			}
			return data, file.Mode(), nil
		}
	}
	return nil, 0, fmt.Errorf("binary payload not found in zip archive")
}

// maxExtractedBinaryBytes bounds a single decompressed archive member so a
// decompression bomb (a small artifact that inflates to gigabytes) can't exhaust
// memory during extraction. A controller/agent binary is well under this.
const maxExtractedBinaryBytes = 512 << 20 // 512 MiB

// readBoundedBinary reads r fully but refuses more than maxExtractedBinaryBytes.
func readBoundedBinary(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxExtractedBinaryBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxExtractedBinaryBytes {
		return nil, fmt.Errorf("extracted binary exceeds %d bytes (possible decompression bomb)", maxExtractedBinaryBytes)
	}
	return data, nil
}

func extractTarGzBinary(archive []byte, targets []string) ([]byte, os.FileMode, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, 0, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != 0 {
			continue
		}
		for _, target := range targets {
			if filepath.Base(header.Name) != target {
				continue
			}
			data, err := readBoundedBinary(tr)
			if err != nil {
				return nil, 0, err
			}
			return data, fsMode(header.FileInfo().Mode()), nil
		}
	}
	return nil, 0, fmt.Errorf("binary payload not found in tar archive")
}

func runtimeExecutableName() string {
	if runtime.GOOS == "windows" {
		return "fleet.exe"
	}
	return "fleet"
}

// userBinaryInstallPath returns the recommended user-writable install location
// for the fleet binary on the current OS. This is shown when a system-path
// binary can't be updated due to missing write permission.
func userBinaryInstallPath() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			return filepath.Join(localAppData, "Programs", "fleet", "fleet.exe")
		}
		if home != "" {
			return filepath.Join(home, "AppData", "Local", "Programs", "fleet", "fleet.exe")
		}
		return `C:\Users\<you>\AppData\Local\Programs\fleet\fleet.exe`
	default:
		// Linux and macOS both use XDG_BIN_HOME or ~/.local/bin
		if xdgBin := os.Getenv("XDG_BIN_HOME"); xdgBin != "" {
			return filepath.Join(xdgBin, "fleet")
		}
		if home != "" {
			return filepath.Join(home, ".local", "bin", "fleet")
		}
		return "~/.local/bin/fleet"
	}
}

func normalizeMode(mode os.FileMode) os.FileMode {
	if mode == 0 {
		return 0o755
	}
	return mode.Perm()
}

func replaceFile(stagedPath, targetPath string) error {
	return replaceFilePlatform(stagedPath, targetPath)
}

func fsMode(mode os.FileMode) os.FileMode {
	if mode == 0 {
		return 0o755
	}
	return mode.Perm()
}

func verifyMinisignSignature(publicKeyText string, message, signature []byte) error {
	if !hasConfiguredSigningKey(publicKeyText) {
		return fmt.Errorf("minisign public key is not configured")
	}

	var publicKey minisign.PublicKey
	if err := publicKey.UnmarshalText([]byte(strings.TrimSpace(publicKeyText))); err != nil {
		return err
	}
	if !minisign.Verify(publicKey, message, signature) {
		return fmt.Errorf("signature verification failed")
	}
	return nil
}

func hasConfiguredSigningKey(publicKeyText string) bool {
	trimmed := strings.TrimSpace(publicKeyText)
	if trimmed == "" {
		return false
	}
	return !strings.Contains(trimmed, "REPLACE_WITH_MINISIGN_PUBLIC_KEY")
}

var (
	knownUpdateOS = map[string]bool{
		"linux": true, "darwin": true, "windows": true,
		"freebsd": true, "openbsd": true, "netbsd": true,
	}
	knownUpdateArch = map[string]bool{
		"amd64": true, "arm64": true, "armv7": true, "386": true,
		"ppc64le": true, "s390x": true, "riscv64": true,
	}
)

type artifactRole string

const (
	artifactRoleFleet      artifactRole = "fleet"
	artifactRoleFleetAgent artifactRole = "fleet-agent"
)

func artifactRoleFor(agent bool) artifactRole {
	if agent {
		return artifactRoleFleetAgent
	}
	return artifactRoleFleet
}

func canonicalRoleBinary(role artifactRole) string {
	name := string(role)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func canonicalUpdateTarget(goos, goarch string) string {
	goos = strings.ToLower(strings.TrimSpace(goos))
	goarch = strings.ToLower(strings.TrimSpace(goarch))
	switch goarch {
	case "arm", "armv7", "armv7l":
		goarch = "armv7"
	}
	return goos + "-" + goarch
}

func runtimeUpdateTarget() string {
	return canonicalUpdateTarget(runtime.GOOS, runtime.GOARCH)
}

// assertSignedComment requires exact equality with the one authenticated release
// identity trusted by the caller. No extra words, aliases, case folding, or
// manifest-controlled display names are accepted.
func assertSignedComment(signature []byte, role artifactRole, version, target string) error {
	if role != artifactRoleFleet && role != artifactRoleFleetAgent {
		return fmt.Errorf("invalid trusted artifact role %q", role)
	}
	canonicalVersion, err := canonicalSemVersion(version)
	if err != nil {
		return fmt.Errorf("invalid expected signed version: %w", err)
	}
	parts := strings.SplitN(target, "-", 2)
	if len(parts) != 2 || !knownUpdateOS[parts[0]] || !knownUpdateArch[parts[1]] || canonicalUpdateTarget(parts[0], parts[1]) != target {
		return fmt.Errorf("expected signed target %q is not canonical", target)
	}
	var sig minisign.Signature
	if err := sig.UnmarshalText(signature); err != nil {
		return fmt.Errorf("parse signature trusted comment: %w", err)
	}
	expected := fmt.Sprintf("cenvero-fleet %s %s %s", role, canonicalVersion, target)
	if sig.TrustedComment != expected {
		return fmt.Errorf("signed trusted comment %q does not exactly match %q", sig.TrustedComment, expected)
	}
	return nil
}
