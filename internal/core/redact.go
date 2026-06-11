// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// RedactPlaceholder replaces every match of a redaction pattern in exec output.
const RedactPlaceholder = "***REDACTED***"

// defaultSecretPatterns are conservative, secret-ish defaults that callers may
// opt into via SetRedactDefaults(true). They aim to catch obvious credentials
// without being so broad that they mangle ordinary output.
var defaultSecretPatterns = []string{
	`(?i)(password|passwd|pwd)\s*[:=]\s*\S+`,
	`(?i)(secret|api[_-]?key|token)\s*[:=]\s*\S+`,
	`(?i)authorization:\s*bearer\s+\S+`,
	`-----BEGIN [A-Z ]*PRIVATE KEY-----`,
}

// RedactStore holds output-redaction policy, persisted as a single JSON
// document at <configDir>/policy.json (0600). User patterns are compiled once
// on load/set; bad regexes are rejected at set-time with a clear error. It is a
// read/modify/write store opened from a config dir and kept off *App so it does
// not require touching app.go.
type RedactStore struct {
	path string

	mu           sync.RWMutex
	patterns     []string         // raw user patterns (source of truth)
	useDefaults  bool             // include defaultSecretPatterns when redacting
	compiled     []*regexp.Regexp // compiled user patterns
	compiledDefs []*regexp.Regexp // compiled defaults (lazy, cached)
}

// redactDocument is the on-disk JSON shape.
type redactDocument struct {
	RedactPatterns []string `json:"redact_patterns"`
	UseDefaults    bool     `json:"use_default_patterns"`
}

// NewRedactStore opens and loads a redact store rooted at configDir. If
// configDir is empty the default config dir is used. A missing or empty policy
// file yields an empty store (no error). A corrupt or uncompilable file yields
// an error so callers don't silently run with no redaction.
func NewRedactStore(configDir string) (*RedactStore, error) {
	if configDir == "" {
		configDir = DefaultConfigDir("")
	}
	s := &RedactStore{path: PolicyPath(configDir)}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// PolicyPath returns the on-disk location of the policy document for a config dir.
func PolicyPath(configDir string) string {
	return filepath.Join(configDir, "policy.json")
}

// load reads and compiles the policy document, replacing in-memory state.
func (s *RedactStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.mu.Lock()
			s.patterns = nil
			s.compiled = nil
			s.useDefaults = false
			s.mu.Unlock()
			return nil
		}
		return fmt.Errorf("read policy: %w", err)
	}
	doc := redactDocument{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("decode policy: %w", err)
		}
	}
	compiled, err := compilePatterns(doc.RedactPatterns)
	if err != nil {
		return fmt.Errorf("policy %s: %w", s.path, err)
	}
	s.mu.Lock()
	s.patterns = append([]string(nil), doc.RedactPatterns...)
	s.compiled = compiled
	s.useDefaults = doc.UseDefaults
	s.mu.Unlock()
	return nil
}

// compilePatterns compiles each pattern, returning a clear error naming the
// first bad regex. Empty patterns are skipped.
func compilePatterns(patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if p == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid redact pattern %q: %w", p, err)
		}
		compiled = append(compiled, re)
	}
	return compiled, nil
}

// SetPatterns replaces the user redaction patterns. Bad regexes are rejected
// (nothing is persisted) with a clear error. Empty entries are dropped.
func (s *RedactStore) SetPatterns(patterns []string) error {
	cleaned := make([]string, 0, len(patterns))
	for _, p := range patterns {
		if p != "" {
			cleaned = append(cleaned, p)
		}
	}
	compiled, err := compilePatterns(cleaned)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.patterns = cleaned
	s.compiled = compiled
	useDefaults := s.useDefaults
	s.mu.Unlock()
	return s.persist(cleaned, useDefaults)
}

// SetDefaults toggles redaction of the known secret-ish default patterns.
func (s *RedactStore) SetDefaults(enabled bool) error {
	s.mu.Lock()
	s.useDefaults = enabled
	patterns := append([]string(nil), s.patterns...)
	s.mu.Unlock()
	return s.persist(patterns, enabled)
}

// Patterns returns a copy of the configured user patterns.
func (s *RedactStore) Patterns() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.patterns...)
}

// DefaultsEnabled reports whether the secret-ish default patterns are active.
func (s *RedactStore) DefaultsEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.useDefaults
}

// Redact replaces every match of every active pattern with RedactPlaceholder.
// With no patterns configured it returns text unchanged.
func (s *RedactStore) Redact(text string) string {
	if text == "" {
		return text
	}
	s.mu.RLock()
	compiled := s.compiled
	useDefaults := s.useDefaults
	if useDefaults && s.compiledDefs == nil {
		s.mu.RUnlock()
		s.ensureDefaultsCompiled()
		s.mu.RLock()
	}
	defs := s.compiledDefs
	s.mu.RUnlock()

	for _, re := range compiled {
		text = re.ReplaceAllString(text, RedactPlaceholder)
	}
	if useDefaults {
		for _, re := range defs {
			text = re.ReplaceAllString(text, RedactPlaceholder)
		}
	}
	return text
}

// ensureDefaultsCompiled lazily compiles the built-in default patterns once.
func (s *RedactStore) ensureDefaultsCompiled() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.compiledDefs != nil {
		return
	}
	// defaultSecretPatterns are constants known to compile; ignore the error.
	defs, _ := compilePatterns(defaultSecretPatterns)
	s.compiledDefs = defs
}

// persist writes the policy document atomically (0600).
func (s *RedactStore) persist(patterns []string, useDefaults bool) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	doc := redactDocument{RedactPatterns: patterns, UseDefaults: useDefaults}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode policy: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".policy-*.json")
	if err != nil {
		return fmt.Errorf("write policy: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write policy: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write policy: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write policy: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("write policy: %w", err)
	}
	return nil
}
