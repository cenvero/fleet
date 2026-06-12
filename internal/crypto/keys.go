// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package crypto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

type Algorithm string

const (
	AlgorithmEd25519 Algorithm = "ed25519"
	AlgorithmRSA4096 Algorithm = "rsa-4096"
	AlgorithmBoth    Algorithm = "both"
)

func ParseAlgorithm(v string) (Algorithm, error) {
	switch v {
	case string(AlgorithmEd25519):
		return AlgorithmEd25519, nil
	case string(AlgorithmRSA4096):
		return AlgorithmRSA4096, nil
	case string(AlgorithmBoth):
		return AlgorithmBoth, nil
	default:
		return "", fmt.Errorf("unknown key algorithm %q", v)
	}
}

// writeNewKeyFile writes a freshly generated key to path atomically and
// exclusively: it creates a temp file in the same directory, then publishes it
// with os.Link, which fails if path already exists. O_EXCL on the temp create
// and the Link-fails-on-exist guarantee mean we never follow or overwrite a
// pre-planted file or symlink at path — a local attacker cannot redirect a
// private-key write to clobber another file or trick us into trusting a planted
// key. Mirrors the temp+Link publish used by EnsureEd25519Signer in ssh.go.
//
// If path already exists the write fails (os.IsExist), because key generation
// must never silently overwrite an existing key pair.
func writeNewKeyFile(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".keygen-*")
	if err != nil {
		return fmt.Errorf("create key temp for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op after a successful link
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod key temp for %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write key temp for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("flush key temp for %s: %w", path, err)
	}
	if err := os.Link(tmpPath, path); err != nil {
		return fmt.Errorf("publish key %s: %w", path, err)
	}
	return nil
}

func GenerateKeySet(dir string, algorithm Algorithm, passphrase []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create key directory: %w", err)
	}

	if algorithm == AlgorithmEd25519 || algorithm == AlgorithmBoth {
		if err := generateEd25519(dir, passphrase); err != nil {
			return err
		}
	}
	if algorithm == AlgorithmRSA4096 || algorithm == AlgorithmBoth {
		if err := generateRSA4096(dir, passphrase); err != nil {
			return err
		}
	}
	return nil
}

func Fingerprints(dir string) (map[string]string, error) {
	files := map[string]string{
		"ed25519": filepath.Join(dir, "id_ed25519.pub"),
		"rsa4096": filepath.Join(dir, "id_rsa4096.pub"),
	}

	out := make(map[string]string)
	for key, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read public key %s: %w", path, err)
		}
		pub, _, _, _, err := ssh.ParseAuthorizedKey(bytes.TrimSpace(data))
		if err != nil {
			return nil, fmt.Errorf("parse public key %s: %w", path, err)
		}
		out[key] = ssh.FingerprintSHA256(pub)
	}
	return out, nil
}

func ExportPublicKeys(dir string) (map[string]string, error) {
	files := map[string]string{
		"ed25519": filepath.Join(dir, "id_ed25519.pub"),
		"rsa4096": filepath.Join(dir, "id_rsa4096.pub"),
	}

	out := make(map[string]string)
	for key, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read public key %s: %w", path, err)
		}
		out[key] = string(bytes.TrimSpace(data))
	}
	return out, nil
}

func generateEd25519(dir string, passphrase []byte) error {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate ed25519 key: %w", err)
	}
	var block *pem.Block
	if len(passphrase) > 0 {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", passphrase)
	} else {
		block, err = ssh.MarshalPrivateKey(priv, "")
	}
	if err != nil {
		return fmt.Errorf("marshal ed25519 private key: %w", err)
	}
	if err := writeNewKeyFile(filepath.Join(dir, "id_ed25519"), pem.EncodeToMemory(block), 0o600); err != nil {
		return fmt.Errorf("write ed25519 private key: %w", err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		return fmt.Errorf("create ed25519 public key: %w", err)
	}
	if err := writeNewKeyFile(filepath.Join(dir, "id_ed25519.pub"), ssh.MarshalAuthorizedKey(pub), 0o644); err != nil { // #nosec G306 -- public key is intentionally world-readable
		return fmt.Errorf("write ed25519 public key: %w", err)
	}
	return nil
}

// NOTE: ed25519 is the default and recommended key algorithm (see
// generateEd25519); RSA-4096 is offered only for interoperability with older
// systems. New deployments should prefer ed25519.
func generateRSA4096(dir string, passphrase []byte) error {
	priv, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return fmt.Errorf("generate rsa key: %w", err)
	}
	var block *pem.Block
	if len(passphrase) > 0 {
		// Encrypt using the modern OpenSSH private-key format (bcrypt KDF +
		// AES-CTR), the same scheme used for passphrase-protected ed25519 keys.
		// This deliberately replaces the deprecated x509.EncryptPEMBlock, whose
		// PEM ("DEK-Info") encryption uses a weak MD5-based KDF and is
		// considered insecure. Existing on-disk keys in the old format still
		// load: ssh.ParsePrivateKeyWithPassphrase (used by LoadPrivateKeySigner)
		// transparently decrypts both the legacy and the OpenSSH formats.
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", passphrase)
		if err != nil {
			return fmt.Errorf("encrypt rsa private key: %w", err)
		}
	} else {
		block = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)}
	}
	if err := writeNewKeyFile(filepath.Join(dir, "id_rsa4096"), pem.EncodeToMemory(block), 0o600); err != nil {
		return fmt.Errorf("write rsa private key: %w", err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		return fmt.Errorf("create rsa public key: %w", err)
	}
	if err := writeNewKeyFile(filepath.Join(dir, "id_rsa4096.pub"), ssh.MarshalAuthorizedKey(pub), 0o644); err != nil { // #nosec G306 -- public key is intentionally world-readable
		return fmt.Errorf("write rsa public key: %w", err)
	}
	return nil
}
