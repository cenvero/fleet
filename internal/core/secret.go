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
// At-rest storage choice (v1): the document is written 0600 (owner read/write
// only) via an atomic temp+rename, and its parent dir is created 0700. Values
// are stored AT-REST-PLAINTEXT inside that 0600 file. internal/crypto offers no
// simple symmetric encrypt-at-rest helper keyed off a controller key file in
// this version, so encryption-at-rest is intentionally deferred: the security
// boundary in v1 is filesystem permissions (0600/0700) plus the guarantee that
// values are NEVER printed, returned by List/meta, logged, or echoed. When a
// symmetric helper lands, Set/Get become the single place to wrap/unwrap.
type SecretStore struct {
	path string
	mu   sync.Mutex
}

// secretRecord is the on-disk shape of a single secret. The Value is the secret
// material; it is NEVER exposed by List or any meta accessor.
type secretRecord struct {
	Value   string    `json:"value"`
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
	doc.Secrets[name] = secretRecord{Value: value, Created: created}
	return s.write(doc)
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
	return rec.Value, nil
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
	doc.Secrets[name] = secretRecord{Value: value, Created: time.Now().UTC()}
	return s.write(doc)
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
	return s.write(doc)
}
