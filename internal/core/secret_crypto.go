// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/ssh"
)

// secret_crypto.go holds the encryption-at-rest helpers for SecretStore. Secret
// values are sealed with AES-256-GCM under a 32-byte key derived once per store.
//
// Key-derivation choice (in priority order):
//
//  1. CONTROLLER KEY (preferred): if the controller's primary private key is
//     present at <configDir>/keys/id_ed25519 (or id_rsa4096) and loads WITHOUT a
//     passphrase, the AES key is derived from its raw private-key material via
//     HKDF-SHA256 with the fixed info string secretEncInfo. This needs no new key
//     file and binds the secrets to the controller identity — they cannot be
//     decrypted without that private key.
//
//  2. DEDICATED KEY FILE (fallback): if no usable controller private key is
//     present (a bare config dir, e.g. tests, or a passphrase-protected key that
//     core cannot unlock), a dedicated 32-byte random key is generated once at
//     <configDir>/keys/secret.key (0600) and reused thereafter.
//
// The fallback guarantees the store always has a stable key, so encryption never
// blocks a Set and a secret is never lost.

// secretEncInfo is the fixed HKDF info string binding the derived key to this
// purpose and version. Changing it would orphan existing ciphertexts.
const secretEncInfo = "cenvero-fleet:secret-encryption:v1" // #nosec G101 -- HKDF info label, not a credential

// secretEncMarker is the per-record format marker written alongside an encrypted
// value. Records without it are treated as legacy plaintext (see secret.go).
const secretEncMarker = "aesgcm-v1" // #nosec G101 -- on-disk format marker, not a credential

// secretKeyFile is the dedicated fallback key, relative to <configDir>/keys.
const secretKeyFile = "secret.key"

// controllerPrimaryKeyNames lists the on-disk private-key file names tried, in
// order, when deriving the encryption key from controller identity. id_ed25519
// is the configured PrimaryKey default (see DefaultConfig).
var controllerPrimaryKeyNames = []string{"id_ed25519", "id_rsa4096"}

// DeriveSecretKey is the exported form of deriveSecretKey, used by the controller
// key-rotation flow to capture the secret-encryption key BEFORE and AFTER the
// controller private key is replaced on disk. Because the derived key depends on
// the controller private-key material, callers must snapshot it at the right
// moment (old key before promotion, new key after) and pass both to
// SecretStore.Rekey.
func DeriveSecretKey(configDir string) ([]byte, error) {
	return deriveSecretKey(configDir)
}

// deriveSecretKey returns the stable 32-byte AES key for the store rooted at
// configDir, preferring the controller private key and falling back to a
// dedicated key file. It is deterministic for a given config dir.
func deriveSecretKey(configDir string) ([]byte, error) {
	keysDir := filepath.Join(configDir, "keys")

	if seed, ok := loadControllerKeyMaterial(keysDir); ok {
		key := hkdfKey(seed, []byte("controller-private-key"))
		// Wipe the copied seed material once the key is derived.
		for i := range seed {
			seed[i] = 0
		}
		return key, nil
	}

	raw, err := loadOrCreateSecretKeyFile(keysDir)
	if err != nil {
		return nil, err
	}
	// The dedicated file already holds 32 random bytes; run it through HKDF too
	// so both paths share one derivation shape and info-string binding.
	return hkdfKey(raw, []byte("dedicated-key-file")), nil
}

// hkdfKey expands ikm into a 32-byte key with HKDF-SHA256 using the fixed
// secretEncInfo and a path-discriminating salt.
func hkdfKey(ikm, salt []byte) []byte {
	r := hkdf.New(sha256.New, ikm, salt, []byte(secretEncInfo))
	key := make([]byte, 32)
	// hkdf.Read never errors for a 32-byte read with SHA-256.
	_, _ = io.ReadFull(r, key)
	return key
}

// loadControllerKeyMaterial returns raw private-key bytes for the first
// controller key that exists and loads without a passphrase. ok is false when no
// usable key is found, in which case the caller falls back to the key file.
func loadControllerKeyMaterial(keysDir string) (seed []byte, ok bool) {
	for _, name := range controllerPrimaryKeyNames {
		path := filepath.Join(keysDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		raw, err := ssh.ParseRawPrivateKey(data)
		if err != nil {
			// Passphrase-protected or unparseable: core cannot unlock it here, so
			// skip and let the dedicated key file take over.
			continue
		}
		if material, ok := rawPrivateKeyBytes(raw); ok {
			return material, true
		}
	}
	return nil, false
}

// rawPrivateKeyBytes extracts stable secret bytes from a parsed private key for
// use as HKDF input keying material.
func rawPrivateKeyBytes(raw any) ([]byte, bool) {
	switch k := raw.(type) {
	case *ed25519.PrivateKey:
		return append([]byte(nil), (*k)...), true
	case ed25519.PrivateKey:
		return append([]byte(nil), k...), true
	case *rsa.PrivateKey:
		return append([]byte(nil), k.D.Bytes()...), true
	default:
		return nil, false
	}
}

// loadOrCreateSecretKeyFile returns the 32-byte dedicated key, generating it on
// first use. The file is 0600 and its parent dir 0700.
func loadOrCreateSecretKeyFile(keysDir string) ([]byte, error) {
	path := filepath.Join(keysDir, secretKeyFile)
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) < 32 {
			return nil, fmt.Errorf("secret key file %s is too short (%d bytes)", path, len(data))
		}
		return data[:32], nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read secret key: %w", err)
	}

	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		return nil, fmt.Errorf("create keys dir: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate secret key: %w", err)
	}
	// Atomic create: temp file -> chmod 0600 -> rename, so a concurrent reader
	// never sees a partial key.
	tmp, err := os.CreateTemp(keysDir, ".secret-key-*")
	if err != nil {
		return nil, fmt.Errorf("write secret key: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write secret key: %w", err)
	}
	if _, err := tmp.Write(key); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write secret key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("write secret key: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		// A racing writer may have created it first; fall back to its contents.
		if existing, rerr := os.ReadFile(path); rerr == nil && len(existing) >= 32 {
			return existing[:32], nil
		}
		return nil, fmt.Errorf("write secret key: %w", err)
	}
	return key, nil
}

// encryptValue seals plaintext with AES-256-GCM under key, returning the
// base64(nonce||ciphertext) string stored on the record.
func encryptValue(key []byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("init cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("init gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("read nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// decryptValue opens a base64(nonce||ciphertext) string sealed by encryptValue.
func decryptValue(key []byte, encoded string) (string, error) {
	sealed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode secret: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("init cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("init gcm: %w", err)
	}
	if len(sealed) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt secret: %w", err)
	}
	return string(plaintext), nil
}
