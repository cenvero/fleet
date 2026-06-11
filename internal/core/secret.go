// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// SecretStore is a small standalone named-secret store (FL-004, controller-side
// v1). Secrets are persisted as a single JSON document at
// <configDir>/secrets.json. It is a read/modify/write store opened from a config
// dir and kept off *App so it does not require touching app.go (matching the
// TagStore / RedactStore pattern).
//
// At-rest storage choice: the document is written 0600 (owner read/write only)
// via an atomic temp+rename, and its parent dir is created 0700. Each secret
// VALUE is encrypted at rest with AES-256-GCM (a fresh random 12-byte nonce per
// value; nonce||ciphertext base64-encoded; record marker "enc":"aesgcm-v1") — no
// longer plaintext-0600. The AES key is a stable 32-byte key derived via
// HKDF-SHA256 (see secret_crypto.go): preferring the controller's existing
// primary private key under <configDir>/keys (binding secrets to controller
// identity, no new key file) and falling back to a dedicated 0600
// <configDir>/keys/secret.key when no usable controller key is present. The
// remaining boundary is still filesystem permissions plus the guarantee that
// values are NEVER printed, returned by List/meta, logged, or echoed. Set/Get
// are the single place that wrap/unwrap.
//
// Migration: a legacy secrets.json with plaintext values (records without the
// "enc" marker) is read as-is for back-compat, and every value is transparently
// re-encrypted on the next write (Set/Generate/Rotate/Remove all rewrite the
// whole document), so no secret is ever lost.
type SecretStore struct {
	path string
	mu   sync.Mutex

	// key is the lazily-derived 32-byte AES key, cached after first use. keyErr
	// records a derivation failure so it surfaces consistently.
	key    []byte
	keyErr error
}

// secretRecord is the on-disk shape of a single secret. The Value holds the
// secret material — AES-256-GCM ciphertext (base64 nonce||ciphertext) when Enc
// is set, or legacy plaintext when Enc is empty. It is NEVER exposed by List or
// any meta accessor.
type secretRecord struct {
	Value   string    `json:"value"`
	Enc     string    `json:"enc,omitempty"`
	Created time.Time `json:"created"`
}

// secretsDocument is the on-disk JSON shape: secret name -> record.
type secretsDocument struct {
	Secrets map[string]secretRecord `json:"secrets"`
}

// SecretMeta is the safe, value-free view of a secret returned by List. It
// deliberately carries the name and creation time only — never the value.
type SecretMeta struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
}

// secretNameMaxLen bounds secret names to keep on-disk keys and any echoed
// `VAR=@name` references reasonable.
const secretNameMaxLen = 128

// NewSecretStore opens (without reading) a secret store rooted at configDir. If
// configDir is empty the default config dir is used.
func NewSecretStore(configDir string) *SecretStore {
	if configDir == "" {
		configDir = DefaultConfigDir("")
	}
	return &SecretStore{path: SecretsPath(configDir)}
}

// SecretsPath returns the on-disk location of the secrets document for a config
// dir.
func SecretsPath(configDir string) string {
	return filepath.Join(configDir, "secrets.json")
}

// encKey lazily derives and caches the store's 32-byte AES key. The caller must
// hold s.mu. Derivation is deterministic for a config dir, so caching is safe
// across reads and writes of the same store instance.
func (s *SecretStore) encKey() ([]byte, error) {
	if s.key != nil || s.keyErr != nil {
		return s.key, s.keyErr
	}
	configDir := filepath.Dir(s.path)
	key, err := deriveSecretKey(configDir)
	if err != nil {
		s.keyErr = err
		return nil, err
	}
	s.key = key
	return key, nil
}

// sealRecord returns a secretRecord with value encrypted at rest under the
// store's AES key. The caller must hold s.mu.
func (s *SecretStore) sealRecord(value string, created time.Time) (secretRecord, error) {
	key, err := s.encKey()
	if err != nil {
		return secretRecord{}, err
	}
	enc, err := encryptValue(key, value)
	if err != nil {
		return secretRecord{}, err
	}
	return secretRecord{Value: enc, Enc: secretEncMarker, Created: created}, nil
}

// openRecord returns the plaintext value of rec, decrypting AES-256-GCM records
// and passing legacy plaintext (no Enc marker) through unchanged. The caller
// must hold s.mu.
func (s *SecretStore) openRecord(rec secretRecord) (string, error) {
	if rec.Enc == "" {
		// Legacy plaintext record (pre-encryption file): read as-is. It will be
		// re-encrypted on the next write of the document.
		return rec.Value, nil
	}
	if rec.Enc != secretEncMarker {
		return "", fmt.Errorf("unsupported secret encryption format %q", rec.Enc)
	}
	key, err := s.encKey()
	if err != nil {
		return "", err
	}
	return decryptValue(key, rec.Value)
}

// ValidateSecretName enforces a charset-safe name with no path separators or
// traversal. The accepted charset is [A-Za-z0-9._-]; a leading "." (e.g. "..")
// is rejected so a name can never escape or shadow the store file.
func ValidateSecretName(name string) error {
	if name == "" {
		return fmt.Errorf("secret name must not be empty")
	}
	if len(name) > secretNameMaxLen {
		return fmt.Errorf("secret name %q is too long (max %d)", name, secretNameMaxLen)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("secret name %q must not start with '.'", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return fmt.Errorf("secret name %q contains an invalid character %q (allowed: A-Za-z0-9._-)", name, string(r))
		}
	}
	// Defense in depth: reject anything the OS could read as a path component.
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return fmt.Errorf("secret name %q must not contain path separators or '..'", name)
	}
	return nil
}

func (s *SecretStore) read() (secretsDocument, error) {
	doc := secretsDocument{Secrets: map[string]secretRecord{}}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return doc, fmt.Errorf("read secrets: %w", err)
	}
	if len(data) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("decode secrets: %w", err)
	}
	if doc.Secrets == nil {
		doc.Secrets = map[string]secretRecord{}
	}
	return doc, nil
}

func (s *SecretStore) write(doc secretsDocument) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode secrets: %w", err)
	}
	// Atomic write: temp file in the same dir -> chmod 0600 -> rename.
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".secrets-*.json")
	if err != nil {
		return fmt.Errorf("write secrets: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write secrets: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write secrets: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write secrets: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("write secrets: %w", err)
	}
	return nil
}

// upgradeAndWrite encrypts any legacy plaintext records in doc (records without
// the Enc marker, left over from a pre-encryption secrets.json) before writing,
// so that every persisted value ends up AES-256-GCM sealed. This makes any
// mutating call (Set/Generate/Rotate/Remove) transparently migrate the whole
// document forward with no risk of losing a secret. The caller must hold s.mu.
func (s *SecretStore) upgradeAndWrite(doc secretsDocument) error {
	for name, rec := range doc.Secrets {
		if rec.Enc != "" {
			continue // already encrypted
		}
		sealed, err := s.sealRecord(rec.Value, rec.Created)
		if err != nil {
			return err
		}
		doc.Secrets[name] = sealed
	}
	return s.write(doc)
}

// Set stores (or replaces) a secret value under name. The creation time is
// preserved across a replace so List meta stays stable. The value is never
// returned or logged by this call.
func (s *SecretStore) Set(name, value string) error {
	if err := ValidateSecretName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return err
	}
	created := time.Now().UTC()
	if existing, ok := doc.Secrets[name]; ok && !existing.Created.IsZero() {
		created = existing.Created
	}
	rec, err := s.sealRecord(value, created)
	if err != nil {
		return err
	}
	doc.Secrets[name] = rec
	return s.upgradeAndWrite(doc)
}

// secretAlphabet is the alphanumeric charset used by Generate/Rotate. It is
// URL/shell-safe and avoids visually ambiguous separators.
const secretAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// generateValue returns a cryptographically-random alphanumeric string of the
// given length, drawn uniformly from secretAlphabet (rejection sampling avoids
// modulo bias).
func generateValue(length int) (string, error) {
	if length <= 0 {
		return "", fmt.Errorf("length must be greater than 0")
	}
	out := make([]byte, length)
	// Read a generous buffer of randomness and refill if rejection consumes it.
	buf := make([]byte, length*2)
	bi := len(buf)
	const max = 256 - (256 % len(secretAlphabet)) // largest multiple of the alphabet < 256
	for i := 0; i < length; {
		if bi >= len(buf) {
			if _, err := rand.Read(buf); err != nil {
				return "", fmt.Errorf("read random bytes: %w", err)
			}
			bi = 0
		}
		b := buf[bi]
		bi++
		if int(b) >= max {
			continue // reject to keep the distribution uniform
		}
		out[i] = secretAlphabet[int(b)%len(secretAlphabet)]
		i++
	}
	return string(out), nil
}

// Generate creates a new random alphanumeric secret of the given length and
// stores it under name. The generated value is stored and NEVER returned or
// printed — callers learn only that it succeeded (and may print the length).
func (s *SecretStore) Generate(name string, length int) error {
	if err := ValidateSecretName(name); err != nil {
		return err
	}
	value, err := generateValue(length)
	if err != nil {
		return err
	}
	return s.Set(name, value)
}

// Get returns the secret value for name. This is the ONLY accessor that exposes
// a value; callers (exec --secret resolution) must add the value to the
// redaction set so it can never leak into output.
func (s *SecretStore) Get(name string) (string, error) {
	if err := ValidateSecretName(name); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return "", err
	}
	rec, ok := doc.Secrets[name]
	if !ok {
		return "", fmt.Errorf("secret %q not found", name)
	}
	return s.openRecord(rec)
}

// List returns value-free metadata (name + created) for every stored secret,
// sorted by name. It NEVER exposes secret values.
func (s *SecretStore) List() ([]SecretMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return nil, err
	}
	metas := make([]SecretMeta, 0, len(doc.Secrets))
	for name, rec := range doc.Secrets {
		metas = append(metas, SecretMeta{Name: name, Created: rec.Created})
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Name < metas[j].Name })
	return metas, nil
}

// Rotate replaces an existing secret with a freshly generated random value of
// the given length. The secret must already exist; the new value is stored and
// NEVER returned or printed.
func (s *SecretStore) Rotate(name string, length int) error {
	if err := ValidateSecretName(name); err != nil {
		return err
	}
	value, err := generateValue(length)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return err
	}
	if _, ok := doc.Secrets[name]; !ok {
		return fmt.Errorf("secret %q not found", name)
	}
	// Rotation refreshes both the value and the creation/rotation timestamp.
	rec, err := s.sealRecord(value, time.Now().UTC())
	if err != nil {
		return err
	}
	doc.Secrets[name] = rec
	return s.upgradeAndWrite(doc)
}

// Rekey re-seals every stored secret from oldKey to newKey, used when the
// controller's primary private key (the HKDF input for the derived secret key)
// is rotated. Without this, the post-rotation derived key no longer matches the
// ciphertexts and every Get fails permanently — a silent data-loss bug.
//
// Crash-safety: every value is decrypted under oldKey into memory FIRST and only
// then re-encrypted under newKey and written via the atomic temp+rename in
// write(). If a value cannot be opened under oldKey the whole operation aborts
// before any write, so a half-rotated store is never persisted. Legacy plaintext
// records (no Enc marker) are simply sealed under newKey. The caller passes the
// keys explicitly because the on-disk controller key — and therefore the result
// of deriveSecretKey — changes across the rotation; this method must not re-derive
// from disk.
func (s *SecretStore) Rekey(oldKey, newKey []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.read()
	if err != nil {
		return err
	}
	if len(doc.Secrets) == 0 {
		return nil // nothing to re-seal
	}

	// Phase 1: decrypt every value under the OLD key into memory. Abort on the
	// first failure so nothing is written if the old key is wrong.
	plaintexts := make(map[string]string, len(doc.Secrets))
	for name, rec := range doc.Secrets {
		if rec.Enc == "" {
			// Legacy plaintext: carry the value forward to be sealed under newKey.
			plaintexts[name] = rec.Value
			continue
		}
		if rec.Enc != secretEncMarker {
			return fmt.Errorf("rekey secret %q: unsupported encryption format %q", name, rec.Enc)
		}
		plain, err := decryptValue(oldKey, rec.Value)
		if err != nil {
			return fmt.Errorf("rekey secret %q: decrypt under old key: %w", name, err)
		}
		plaintexts[name] = plain
	}

	// Phase 2: re-seal every value under the NEW key, preserving timestamps.
	for name, plain := range plaintexts {
		enc, err := encryptValue(newKey, plain)
		if err != nil {
			return fmt.Errorf("rekey secret %q: encrypt under new key: %w", name, err)
		}
		rec := doc.Secrets[name]
		doc.Secrets[name] = secretRecord{Value: enc, Enc: secretEncMarker, Created: rec.Created}
	}

	// Phase 3: atomic write. Update the cached key so subsequent in-process reads
	// use the new key (matching what is now on disk after promotion).
	if err := s.write(doc); err != nil {
		return err
	}
	s.key = append([]byte(nil), newKey...)
	s.keyErr = nil
	return nil
}

// Remove deletes a secret. Removing an unknown secret is an error so callers get
// clear feedback.
func (s *SecretStore) Remove(name string) error {
	if err := ValidateSecretName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return err
	}
	if _, ok := doc.Secrets[name]; !ok {
		return fmt.Errorf("secret %q not found", name)
	}
	delete(doc.Secrets, name)
	return s.upgradeAndWrite(doc)
}
