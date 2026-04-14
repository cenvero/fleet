// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package crypto

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

func LoadPrivateKeySigner(path string, passphrase []byte) (ssh.Signer, error) {
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

	pkcs8, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("marshal ed25519 host key: %w", err)
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, fmt.Errorf("write host key %s: %w", path, err)
	}

	return LoadPrivateKeySigner(path, nil)
}
