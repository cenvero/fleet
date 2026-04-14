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
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshal ed25519 private key: %w", err)
	}
	if err := writePEM(filepath.Join(dir, "id_ed25519"), "PRIVATE KEY", pkcs8, passphrase); err != nil {
		return err
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		return fmt.Errorf("create ed25519 public key: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "id_ed25519.pub"), ssh.MarshalAuthorizedKey(pub), 0o644)
}

func generateRSA4096(dir string, passphrase []byte) error {
	priv, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return fmt.Errorf("generate rsa key: %w", err)
	}
	if err := writePEM(filepath.Join(dir, "id_rsa4096"), "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(priv), passphrase); err != nil {
		return err
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		return fmt.Errorf("create rsa public key: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "id_rsa4096.pub"), ssh.MarshalAuthorizedKey(pub), 0o644)
}

func writePEM(path, blockType string, data, passphrase []byte) error {
	block := &pem.Block{Type: blockType, Bytes: data}
	if len(passphrase) > 0 {
		encrypted, err := x509.EncryptPEMBlock(rand.Reader, blockType, data, passphrase, x509.PEMCipherAES256)
		if err != nil {
			return fmt.Errorf("encrypt private key %s: %w", path, err)
		}
		block = encrypted
	}
	payload := pem.EncodeToMemory(block)
	return os.WriteFile(path, payload, 0o600)
}
