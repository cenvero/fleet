// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// IdempotencyStore is a controller-side cache that maps an idempotency key to a
// previously-computed result, with a per-entry expiry. It lets a retried
// operation (e.g. `fleet exec --idempotency-key K ...`) return the cached
// result instead of re-running the side effect.
//
// Entries are persisted as a single JSON document at
// <configDir>/data/idempotency.json (0600). It is a read/modify/write store
// opened from a config dir and kept off *App so it does not require touching
// app.go.
type IdempotencyStore struct {
	path string
	mu   sync.Mutex
}

// idempotencyEntry is the on-disk shape of a single cached result. ExpiresAt is
// stored in UTC; an entry is a miss once time.Now() is at or past it.
type idempotencyEntry struct {
	Result    string    `json:"result"`
	ExpiresAt time.Time `json:"expires_at"`
}

// idempotencyDocument is the on-disk JSON shape: key -> entry.
type idempotencyDocument struct {
	Entries map[string]idempotencyEntry `json:"entries"`
}

// NewIdempotencyStore opens (without reading) an idempotency store rooted at
// configDir. If configDir is empty the default config dir is used.
func NewIdempotencyStore(configDir string) *IdempotencyStore {
	if configDir == "" {
		configDir = DefaultConfigDir("")
	}
	return &IdempotencyStore{path: IdempotencyPath(configDir)}
}

// IdempotencyPath returns the on-disk location of the idempotency document for a
// config dir.
func IdempotencyPath(configDir string) string {
	return filepath.Join(configDir, "data", "idempotency.json")
}

func (s *IdempotencyStore) read() (idempotencyDocument, error) {
	doc := idempotencyDocument{Entries: map[string]idempotencyEntry{}}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return doc, fmt.Errorf("read idempotency cache: %w", err)
	}
	if len(data) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("decode idempotency cache: %w", err)
	}
	if doc.Entries == nil {
		doc.Entries = map[string]idempotencyEntry{}
	}
	return doc, nil
}

func (s *IdempotencyStore) write(doc idempotencyDocument) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode idempotency cache: %w", err)
	}
	// Atomic write: temp file in the same dir -> chmod 0600 -> rename.
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".idempotency-*.json")
	if err != nil {
		return fmt.Errorf("write idempotency cache: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write idempotency cache: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write idempotency cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write idempotency cache: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("write idempotency cache: %w", err)
	}
	return nil
}

// Get returns the cached result for key. The second return value is false on a
// miss: the key is absent, or its entry has expired. Expired entries are pruned
// lazily so the cache file does not grow without bound.
func (s *IdempotencyStore) Get(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return "", false
	}
	entry, ok := doc.Entries[key]
	if !ok {
		return "", false
	}
	if s.expired(entry) {
		// Prune the stale entry; ignore write errors so a read-only cache still
		// reports the miss correctly.
		delete(doc.Entries, key)
		_ = s.write(doc)
		return "", false
	}
	return entry.Result, true
}

// Put stores result under key with the given time-to-live. A non-positive ttl
// is rejected so callers cannot accidentally insert an already-expired (or
// never-expiring) entry. An empty key is also rejected.
func (s *IdempotencyStore) Put(key, result string, ttl time.Duration) error {
	if key == "" {
		return fmt.Errorf("idempotency key must not be empty")
	}
	if ttl <= 0 {
		return fmt.Errorf("idempotency ttl must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return err
	}
	// Opportunistically drop any other expired entries while we hold the lock.
	for k, e := range doc.Entries {
		if k != key && s.expired(e) {
			delete(doc.Entries, k)
		}
	}
	doc.Entries[key] = idempotencyEntry{
		Result:    result,
		ExpiresAt: time.Now().UTC().Add(ttl),
	}
	return s.write(doc)
}

// expired reports whether an entry is at or past its expiry as of now.
func (s *IdempotencyStore) expired(e idempotencyEntry) bool {
	return !time.Now().UTC().Before(e.ExpiresAt)
}
