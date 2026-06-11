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
	"strings"

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

// secretKeySourceFile pins which source produced the secret-encryption key on
// first use. It records a stable source id (see controllerSourceID /
// dedicatedSourceID) alongside the keys so a later derivation can REQUIRE the
// same source instead of silently flipping to a different one (e.g. controller
// key -> dedicated file, or id_ed25519 -> id_rsa4096), which would derive a
// different key and brick decryption of every existing secret. It is relative to
// <configDir>/keys.
const secretKeySourceFile = "secret.keysource"

// controllerPrimaryKeyNames lists the on-disk private-key file names tried, in
// order, when deriving the encryption key from controller identity. id_ed25519
// is the configured PrimaryKey default (see DefaultConfig). The configured
// Config.Crypto.PrimaryKey (when a config is present in the dir) is moved to the
// front of this list — see controllerKeyOrder.
var controllerPrimaryKeyNames = []string{"id_ed25519", "id_rsa4096"}

// controllerKeyOrder returns the controller key file names to try, in priority
// order, honoring Config.Crypto.PrimaryKey when a config is present in configDir.
// The configured primary key is tried FIRST; the remaining known names follow as
// fallbacks. When no config (or no PrimaryKey) is available — bare dirs, tests —
// the default order is used unchanged.
func controllerKeyOrder(configDir string) []string {
	order := append([]string(nil), controllerPrimaryKeyNames...)
	primary := configuredPrimaryKey(configDir)
	if primary == "" {
		return order
	}
	// Move the configured primary to the front, de-duplicating.
	out := []string{primary}
	for _, name := range order {
		if name != primary {
			out = append(out, name)
		}
	}
	return out
}

// configuredPrimaryKey reads Config.Crypto.PrimaryKey from the config file in
// configDir, if one exists. A missing/unreadable config yields "" so derivation
// falls back to the default key order. The name is validated to a bare key file
// name so it can never escape the keys dir.
func configuredPrimaryKey(configDir string) string {
	if configDir == "" {
		return ""
	}
	cfg, err := LoadConfig(ConfigPath(configDir))
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(cfg.Crypto.PrimaryKey)
	if name == "" || validateSafeName(name) != nil {
		return ""
	}
	return name
}

// controllerSourceID / dedicatedSourceID are the pinned identifiers recorded for
// the source that produced the derived key. Controller-key sources are
// "controller:<keyfile>"; the dedicated fallback is "dedicated:secret.key". A
// change in this value between derivations means the underlying key would differ,
// so it is REQUIRED to match.
func controllerSourceID(name string) string { return "controller:" + name }

var dedicatedSourceID = "dedicated:" + secretKeyFile

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
// dedicated key file. It is deterministic for a given config dir AND PINNED to
// the source recorded on first use: once a source produced the key, a later
// derivation REQUIRES that same source. If the pinned source is unavailable it
// returns a clear error rather than silently deriving a different (wrong) key,
// which would make every existing secret undecryptable.
//
// Rotation note: the pin records the SOURCE (which key file / dedicated file),
// not the key material, so a controller key rotation that replaces id_ed25519 in
// place keeps the same pinned source ("controller:id_ed25519") and still derives
// a new key from the new material — exactly what SecretStore.Rekey re-seals
// across. Only a source FLIP (controller<->dedicated, or id_ed25519<->id_rsa4096)
// is blocked.
func deriveSecretKey(configDir string) ([]byte, error) {
	keysDir := filepath.Join(configDir, "keys")

	pinned, hasPin, err := readPinnedSource(keysDir)
	if err != nil {
		return nil, err
	}

	if hasPin {
		return deriveFromPinnedSource(keysDir, pinned)
	}

	// No pin yet: choose a source (controller key preferred, dedicated fallback),
	// derive, then record the chosen source so all later derivations must match.
	if seed, name, ok := loadControllerKeyMaterial(keysDir, configDir); ok {
		key := hkdfKey(seed, []byte("controller-private-key"))
		wipe(seed)
		if err := writePinnedSource(keysDir, controllerSourceID(name)); err != nil {
			return nil, err
		}
		return key, nil
	}

	raw, err := loadOrCreateSecretKeyFile(keysDir)
	if err != nil {
		return nil, err
	}
	if err := writePinnedSource(keysDir, dedicatedSourceID); err != nil {
		return nil, err
	}
	// The dedicated file already holds 32 random bytes; run it through HKDF too
	// so both paths share one derivation shape and info-string binding.
	return hkdfKey(raw, []byte("dedicated-key-file")), nil
}

// deriveFromPinnedSource derives the key from EXACTLY the recorded source, or
// returns a clear "source unavailable" error. It never falls back to a different
// source, since that would derive a different key and orphan existing secrets.
func deriveFromPinnedSource(keysDir, pinned string) ([]byte, error) {
	if pinned == dedicatedSourceID {
		raw, err := loadOrCreateSecretKeyFile(keysDir)
		if err != nil {
			return nil, fmt.Errorf("secret key source %q unavailable; cannot decrypt: %w", pinned, err)
		}
		return hkdfKey(raw, []byte("dedicated-key-file")), nil
	}
	name, ok := strings.CutPrefix(pinned, "controller:")
	if !ok || validateSafeName(name) != nil {
		return nil, fmt.Errorf("secret key source %q is not a recognized source; cannot decrypt", pinned)
	}
	seed, ok := loadOneControllerKey(keysDir, name)
	if !ok {
		return nil, fmt.Errorf("secret key source %q unavailable; cannot decrypt (the pinned controller key is missing or no longer loadable without a passphrase)", pinned)
	}
	key := hkdfKey(seed, []byte("controller-private-key"))
	wipe(seed)
	return key, nil
}

// wipe zeroes secret material once it is no longer needed.
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
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
// controller key that exists and loads without a passphrase, honoring the
// configured PrimaryKey order. It also returns the file NAME of the key it used
// so the caller can pin that exact source. ok is false when no usable key is
// found, in which case the caller falls back to the dedicated key file.
func loadControllerKeyMaterial(keysDir, configDir string) (seed []byte, name string, ok bool) {
	for _, n := range controllerKeyOrder(configDir) {
		if material, ok := loadOneControllerKey(keysDir, n); ok {
			return material, n, true
		}
	}
	return nil, "", false
}

// loadOneControllerKey returns the raw private-key material for the single named
// controller key file if it exists and loads without a passphrase. It is the
// pin-respecting primitive: deriveFromPinnedSource uses it to load EXACTLY the
// recorded key and nothing else.
func loadOneControllerKey(keysDir, name string) (seed []byte, ok bool) {
	if validateSafeName(name) != nil {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(keysDir, name))
	if err != nil {
		return nil, false
	}
	raw, err := ssh.ParseRawPrivateKey(data)
	if err != nil {
		// Passphrase-protected or unparseable: core cannot unlock it here.
		return nil, false
	}
	return rawPrivateKeyBytes(raw)
}

// readPinnedSource returns the recorded source id from the pin file. hasPin is
// false (with a nil error) when no pin exists yet — the first-derivation case.
// A genuine read error (e.g. permissions) is returned so it is never mistaken
// for "no pin".
func readPinnedSource(keysDir string) (id string, hasPin bool, err error) {
	data, err := os.ReadFile(filepath.Join(keysDir, secretKeySourceFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read secret key source pin: %w", err)
	}
	id = strings.TrimSpace(string(data))
	if id == "" {
		// An empty pin file is treated as no pin so a corrupted/empty marker can
		// self-heal on the next derivation rather than wedging the store.
		return "", false, nil
	}
	return id, true, nil
}

// writePinnedSource records id as the pinned key source via an atomic
// temp+chmod 0600+rename. The keys dir is created 0700 if missing. Writing the
// same id again is harmless (idempotent), so callers may write unconditionally.
func writePinnedSource(keysDir, id string) error {
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		return fmt.Errorf("create keys dir: %w", err)
	}
	path := filepath.Join(keysDir, secretKeySourceFile)
	tmp, err := os.CreateTemp(keysDir, ".secret-keysource-*")
	if err != nil {
		return fmt.Errorf("write secret key source pin: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write secret key source pin: %w", err)
	}
	if _, err := tmp.WriteString(id + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write secret key source pin: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write secret key source pin: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("write secret key source pin: %w", err)
	}
	return nil
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
