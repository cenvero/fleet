// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package crypto

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/crypto/ssh"
)

// checkPrivateKeyPerms refuses to load a private key whose file is accessible to
// anyone other than its owner, mirroring OpenSSH's "UNPROTECTED PRIVATE KEY
// FILE" check. A key that is group- or world-readable/writable may already be
// compromised, so we fail closed rather than silently trusting it.
//
// Permission bits are not meaningful on Windows, where Go reports synthetic
// modes, so the check is skipped there.
func checkPrivateKeyPerms(path string, info os.FileInfo) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf(
			"private key %s has insecure permissions %#o: it is accessible by group/others; "+
				"run `chmod 600 %s` (OpenSSH refuses such keys)",
			path, perm, path)
	}
	return nil
}

func LoadPrivateKeySigner(path string, passphrase []byte) (ssh.Signer, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat private key %s: %w", path, err)
	}
	if err := checkPrivateKeyPerms(path, info); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key %s: %w", path, err)
	}

	signer, err := ssh.ParsePrivateKey(data)
	if err == nil {
		return signer, nil
	}

	var passphraseErr *ssh.PassphraseMissingError
	if errors.As(err, &passphraseErr) {
		if len(passphrase) == 0 {
			return nil, fmt.Errorf("private key %s requires a passphrase; passphrase-backed transport is not wired yet", path)
		}
		signer, err = ssh.ParsePrivateKeyWithPassphrase(data, passphrase)
		if err != nil {
			return nil, fmt.Errorf("parse private key %s with passphrase: %w", path, err)
		}
		return signer, nil
	}

	return nil, fmt.Errorf("parse private key %s: %w", path, err)
}

func EnsureEd25519Signer(path string) (ssh.Signer, error) {
	if _, err := os.Stat(path); err == nil {
		return LoadPrivateKeySigner(path, nil)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create signer directory: %w", err)
	}

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 host key: %w", err)
	}

	block, err := ssh.MarshalPrivateKey(privateKey, "")
	if err != nil {
		return nil, fmt.Errorf("marshal ed25519 host key: %w", err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, fmt.Errorf("write host key %s: %w", path, err)
	}

	return LoadPrivateKeySigner(path, nil)
}
