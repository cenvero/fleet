// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// BaselineStore is a small standalone per-server config baseline store. Each
// server's baseline is persisted as a single JSON document at
// <configDir>/baselines/<server>.json (0600). It is opened from a config dir
// and kept off *App so it does not require touching app.go.
type BaselineStore struct {
	dir string
	mu  sync.Mutex
}

// BaselineEntry captures the recorded content (and its sha256) for one path.
type BaselineEntry struct {
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}

// Baseline is the on-disk JSON shape: the captured path -> entry map plus a
// little metadata for display.
type Baseline struct {
	Server     string                   `json:"server"`
	CapturedAt string                   `json:"captured_at,omitempty"`
	Paths      map[string]BaselineEntry `json:"paths"`
}

// NewBaselineStore opens (without reading) a baseline store rooted at configDir.
// If configDir is empty the default config dir is used.
func NewBaselineStore(configDir string) *BaselineStore {
	if configDir == "" {
		configDir = DefaultConfigDir("")
	}
	return &BaselineStore{dir: BaselinesDir(configDir)}
}

// BaselinesDir returns the directory holding per-server baseline documents.
func BaselinesDir(configDir string) string {
	return filepath.Join(configDir, "baselines")
}

// path returns the on-disk location for a server's baseline document.
func (s *BaselineStore) path(server string) (string, error) {
	if err := validateSafeName(server); err != nil {
		return "", fmt.Errorf("invalid server name: %w", err)
	}
	return filepath.Join(s.dir, server+".json"), nil
}

// HashContent returns the lowercase hex sha256 of the given content.
func HashContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// Get returns the stored baseline for a server. ok is false if none exists.
func (s *BaselineStore) Get(server string) (Baseline, bool, error) {
	path, err := s.path(server)
	if err != nil {
		return Baseline{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Baseline{}, false, nil
		}
		return Baseline{}, false, fmt.Errorf("read baseline: %w", err)
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return Baseline{}, false, fmt.Errorf("decode baseline: %w", err)
	}
	if b.Paths == nil {
		b.Paths = map[string]BaselineEntry{}
	}
	return b, true, nil
}

// Save persists a server's baseline document atomically (0600).
func (s *BaselineStore) Save(b Baseline) error {
	path, err := s.path(b.Server)
	if err != nil {
		return err
	}
	if b.Paths == nil {
		b.Paths = map[string]BaselineEntry{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create baselines dir: %w", err)
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("encode baseline: %w", err)
	}
	// Atomic write: temp file in the same dir -> chmod 0600 -> rename.
	tmp, err := os.CreateTemp(s.dir, ".baseline-*.json")
	if err != nil {
		return fmt.Errorf("write baseline: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write baseline: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write baseline: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write baseline: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("write baseline: %w", err)
	}
	return nil
}

// SortedPaths returns the captured paths of a baseline in stable order.
func (b Baseline) SortedPaths() []string {
	paths := make([]string, 0, len(b.Paths))
	for p := range b.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}
