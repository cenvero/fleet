// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package crypto

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestLoadPrivateKeyRejectsInsecurePerms verifies a group/world-accessible
// private key is refused, mirroring OpenSSH's unprotected-key-file check.
func TestLoadPrivateKeyRejectsInsecurePerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions are not enforced on Windows")
	}
	t.Parallel()
	dir := t.TempDir()
	if err := GenerateKeySet(dir, AlgorithmEd25519, nil); err != nil {
		t.Fatalf("GenerateKeySet: %v", err)
	}
	keyPath := filepath.Join(dir, "id_ed25519")

	// 0600 must load fine (sanity check of the happy path).
	if _, err := LoadPrivateKeySigner(keyPath, nil); err != nil {
		t.Fatalf("0600 key should load, got: %v", err)
	}

	// Loosen perms to group-readable and confirm it is now rejected.
	if err := os.Chmod(keyPath, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_, err := LoadPrivateKeySigner(keyPath, nil)
	if err == nil {
		t.Fatal("group-readable private key must be rejected")
	}
	if !strings.Contains(err.Error(), "insecure permissions") {
		t.Fatalf("error %q should mention insecure permissions", err)
	}

	// World-writable must also be rejected.
	if err := os.Chmod(keyPath, 0o602); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := LoadPrivateKeySigner(keyPath, nil); err == nil {
		t.Fatal("world-writable private key must be rejected")
	}
}

// TestRSAPassphraseRoundTrip verifies the modern OpenSSH-format encryption used
// for passphrase-protected RSA keys can be generated and loaded back. This is
// the replacement for the deprecated x509.EncryptPEMBlock path.
func TestRSAPassphraseRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pass := []byte("correct horse battery staple")
	if err := GenerateKeySet(dir, AlgorithmRSA4096, pass); err != nil {
		t.Fatalf("GenerateKeySet(rsa, passphrase): %v", err)
	}
	keyPath := filepath.Join(dir, "id_rsa4096")

	// Sanity: the on-disk key is the modern OpenSSH format, not the legacy
	// "DEK-Info" PEM encryption that x509.EncryptPEMBlock produced.
	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read rsa key: %v", err)
	}
	if strings.Contains(string(data), "DEK-Info") {
		t.Fatal("RSA key should not use the deprecated DEK-Info PEM encryption")
	}
	if !strings.Contains(string(data), "OPENSSH PRIVATE KEY") {
		t.Fatalf("RSA key should use the OpenSSH private-key format, got:\n%s", data)
	}

	// Wrong passphrase must fail; correct passphrase must load.
	if _, err := LoadPrivateKeySigner(keyPath, []byte("wrong")); err == nil {
		t.Fatal("loading with wrong passphrase should fail")
	}
	if _, err := LoadPrivateKeySigner(keyPath, pass); err != nil {
		t.Fatalf("loading with correct passphrase should succeed, got: %v", err)
	}
}

// TestRSAUnencryptedStillPKCS1 confirms the unencrypted RSA path is unchanged
// (legacy PKCS#1 PEM) and still loads.
func TestRSAUnencryptedStillPKCS1(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := GenerateKeySet(dir, AlgorithmRSA4096, nil); err != nil {
		t.Fatalf("GenerateKeySet(rsa): %v", err)
	}
	keyPath := filepath.Join(dir, "id_rsa4096")
	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read rsa key: %v", err)
	}
	if !strings.Contains(string(data), "RSA PRIVATE KEY") {
		t.Fatalf("unencrypted RSA key should be PKCS#1 PEM, got:\n%s", data)
	}
	if _, err := LoadPrivateKeySigner(keyPath, nil); err != nil {
		t.Fatalf("unencrypted RSA key should load, got: %v", err)
	}
}
