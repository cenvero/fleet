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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"aead.dev/minisign"
)

type ApplyOptions struct {
	ManifestURL      string
	Channel          string
	ConfigDir        string
	ExecutablePath   string
	CurrentVersion   string
	AgentBinary      bool
	SigningPublicKey string
	FetchManifest    func(context.Context, string) (Manifest, error)
	DownloadURL      func(context.Context, string) ([]byte, error)
	Now              func() time.Time
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

	manifest, err := opts.FetchManifest(ctx, opts.ManifestURL)
	if err != nil {
		return ApplyResult{}, err
	}
	version, binary, err := manifest.BinaryFor(opts.Channel, opts.AgentBinary)
	if err != nil {
		return ApplyResult{}, err
	}

	result := ApplyResult{
		Channel:         opts.Channel,
		CurrentVersion:  opts.CurrentVersion,
		Version:         version,
		ExecutablePath:  opts.ExecutablePath,
		ReleaseNotesURL: manifest.Channels[opts.Channel].ReleaseNotes,
	}
	if version == opts.CurrentVersion && opts.CurrentVersion != "" {
		return result, nil
	}

	archive, err := opts.DownloadURL(ctx, binary.URL)
	if err != nil {
		return ApplyResult{}, err
	}
	hasSig := strings.TrimSpace(binary.Signature) != ""
	hasHash := strings.TrimSpace(binary.SHA256) != ""
	// Require at least a SHA256 checksum for any non-development channel.
	// Development builds (channel == "dev" or empty) are allowed to omit both
	// so that ad-hoc binaries can be tested without a full signing pipeline.
	if !hasSig && !hasHash && opts.Channel != "dev" && opts.Channel != "" {
		return ApplyResult{}, fmt.Errorf("manifest entry for %s has no SHA256 or signature — refusing to apply unverified binary on channel %q", binary.URL, opts.Channel)
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
	}
	if hasHash {
		actual := sha256.Sum256(archive)
		if !strings.EqualFold(hex.EncodeToString(actual[:]), strings.TrimSpace(binary.SHA256)) {
			return ApplyResult{}, fmt.Errorf("download sha256 mismatch for %s", binary.URL)
		}
		result.SHA256Verified = true
	}

	binaryName := filepath.Base(opts.ExecutablePath)
	payload, mode, err := extractBinaryPayload(binary.URL, archive, binary.DisplayName, binaryName)
	if err != nil {
		return ApplyResult{}, err
	}

	updateDir := filepath.Join(opts.ConfigDir, "backups", "updates")
	if err := os.MkdirAll(updateDir, 0o750); err != nil {
		return ApplyResult{}, err
	}

	backupPath := filepath.Join(updateDir, filepath.Base(opts.ExecutablePath)+"."+opts.Now().UTC().Format("20060102T150405Z")+".bak")
	if err := copyFile(opts.ExecutablePath, backupPath, 0o755); err != nil {
		return ApplyResult{}, fmt.Errorf("backup current executable: %w", err)
	}

	stagedPath := opts.ExecutablePath + ".new"
	// Register cleanup unconditionally so that even a partial write (e.g. disk
	// full after file creation) never leaves a stray .new file behind.
	defer os.Remove(stagedPath) //nolint:errcheck
	if err := os.WriteFile(stagedPath, payload, normalizeMode(mode)); err != nil {
		if os.IsPermission(err) {
			userBin := userBinaryInstallPath()
			return ApplyResult{}, fmt.Errorf(
				"cannot write to %s: permission denied\n\n"+
					"The fleet binary is at a system path that requires root access to update.\n\n"+
					"Option 1 — run with sudo and pass your config dir:\n"+
					"  sudo fleet --config-dir %s update apply\n\n"+
					"Option 2 — reinstall fleet to your user path (no sudo needed):\n"+
					"  install -m 0755 %s %s\n"+
					"  Then add %s to your PATH.",
				opts.ExecutablePath,
				opts.ConfigDir,
				opts.ExecutablePath, userBin,
				filepath.Dir(userBin),
			)
		}
		return ApplyResult{}, fmt.Errorf("write staged executable: %w", err)
	}

	if err := replaceFile(stagedPath, opts.ExecutablePath); err != nil {
		return ApplyResult{}, fmt.Errorf("replace executable: %w", err)
	}

	state := rollbackState{
		ExecutablePath:  opts.ExecutablePath,
		BackupPath:      backupPath,
		PreviousVersion: opts.CurrentVersion,
		AppliedVersion:  version,
		Channel:         opts.Channel,
		AppliedAt:       opts.Now().UTC(),
	}
	statePath := rollbackStatePath(opts.ConfigDir)
	if err := writeRollbackState(statePath, state); err != nil {
		return ApplyResult{}, err
	}

	result.BackupPath = backupPath
	result.RollbackState = statePath
	result.Applied = true
	return result, nil
}

func Rollback(configDir, executablePath string) (RollbackResult, error) {
	if strings.TrimSpace(configDir) == "" {
		return RollbackResult{}, fmt.Errorf("config dir is required")
	}
	if strings.TrimSpace(executablePath) == "" {
		path, err := os.Executable()
		if err != nil {
			return RollbackResult{}, err
		}
		executablePath = path
	}

	state, err := readRollbackState(rollbackStatePath(configDir))
	if err != nil {
		return RollbackResult{}, err
	}
	if state.ExecutablePath != "" {
		executablePath = state.ExecutablePath
	}
	if state.BackupPath == "" {
		return RollbackResult{}, fmt.Errorf("rollback backup path is missing")
	}

	stagedPath := executablePath + ".rollback"
	// Register cleanup before copyFile so a partial copy is always removed.
	defer os.Remove(stagedPath) //nolint:errcheck
	if err := copyFile(state.BackupPath, stagedPath, 0o755); err != nil {
		return RollbackResult{}, fmt.Errorf("stage rollback executable: %w", err)
	}
	if err := replaceFile(stagedPath, executablePath); err != nil {
		return RollbackResult{}, fmt.Errorf("replace executable during rollback: %w", err)
	}
	if err := os.Remove(rollbackStatePath(configDir)); err != nil && !os.IsNotExist(err) {
		return RollbackResult{}, err
	}

	return RollbackResult{
		ExecutablePath: executablePath,
		RestoredFrom:   state.BackupPath,
		Version:        state.PreviousVersion,
		Restored:       true,
	}, nil
}

func rollbackStatePath(configDir string) string {
	return filepath.Join(configDir, "data", "update-rollback.json")
}

func writeRollbackState(path string, state rollbackState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func readRollbackState(path string) (rollbackState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return rollbackState{}, fmt.Errorf("read rollback state: %w", err)
	}
	var state rollbackState
	if err := json.Unmarshal(data, &state); err != nil {
		return rollbackState{}, fmt.Errorf("decode rollback state: %w", err)
	}
	return state, nil
}

func downloadURL(ctx context.Context, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err == nil && parsed.Scheme == "file" {
		return os.ReadFile(parsed.Path)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create download request: %w", err)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("download artifact: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected artifact status %s", resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read artifact body: %w", err)
	}
	return data, nil
}

func extractBinaryPayload(sourceURL string, archive []byte, displayName, executableName string) ([]byte, os.FileMode, error) {
	targets := make([]string, 0, 3)
	if strings.TrimSpace(displayName) != "" {
		targets = append(targets, strings.TrimSpace(displayName))
	}
	targets = append(targets, executableName, runtimeExecutableName())

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
			if file.FileInfo().IsDir() {
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
			data, err := io.ReadAll(rc)
			if err != nil {
				return nil, 0, err
			}
			return data, file.Mode(), nil
		}
	}
	return nil, 0, fmt.Errorf("binary payload not found in zip archive")
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
		if header.FileInfo().IsDir() {
			continue
		}
		for _, target := range targets {
			if filepath.Base(header.Name) != target {
				continue
			}
			data, err := io.ReadAll(tr)
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

func copyFile(source, target string, mode os.FileMode) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if mode == 0 {
		if info, err := os.Stat(source); err == nil {
			mode = info.Mode().Perm()
		}
	}
	if mode == 0 {
		mode = 0o755
	}
	return os.WriteFile(target, data, mode)
}

func replaceFile(stagedPath, targetPath string) error {
	if runtime.GOOS == "windows" {
		_ = os.Remove(targetPath + ".old")
		if err := os.Rename(targetPath, targetPath+".old"); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.Rename(stagedPath, targetPath)
	}
	return os.Rename(stagedPath, targetPath)
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
