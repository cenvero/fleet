// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// TagStore is a small standalone key=value tag store for servers. Tags are
// persisted as a single JSON document at <configDir>/tags.json (0600). It is a
// read/modify/write store opened from a config dir and kept off *App so it does
// not require touching app.go.
type TagStore struct {
	path string
	mu   sync.Mutex
}

// tagsDocument is the on-disk JSON shape: server name -> tag key -> value.
type tagsDocument struct {
	Servers map[string]map[string]string `json:"servers"`
}

// NewTagStore opens (without reading) a tag store rooted at configDir. If
// configDir is empty the default config dir is used.
func NewTagStore(configDir string) *TagStore {
	if configDir == "" {
		configDir = DefaultConfigDir("")
	}
	return &TagStore{path: TagsPath(configDir)}
}

// TagsPath returns the on-disk location of the tags document for a config dir.
func TagsPath(configDir string) string {
	return filepath.Join(configDir, "tags.json")
}

func (s *TagStore) read() (tagsDocument, error) {
	doc := tagsDocument{Servers: map[string]map[string]string{}}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return doc, fmt.Errorf("read tags: %w", err)
	}
	if len(data) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("decode tags: %w", err)
	}
	if doc.Servers == nil {
		doc.Servers = map[string]map[string]string{}
	}
	return doc, nil
}

func (s *TagStore) write(doc tagsDocument) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode tags: %w", err)
	}
	// Atomic write: temp file in the same dir -> chmod 0600 -> rename.
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".tags-*.json")
	if err != nil {
		return fmt.Errorf("write tags: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write tags: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write tags: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write tags: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("write tags: %w", err)
	}
	return nil
}

// validateTagKey ensures a tag key is a non-empty, reasonable identifier so the
// `key=value` filter expression syntax stays unambiguous.
func validateTagKey(key string) error {
	if key == "" {
		return fmt.Errorf("tag key must not be empty")
	}
	if strings.ContainsAny(key, "=,") {
		return fmt.Errorf("tag key %q must not contain '=' or ','", key)
	}
	if strings.TrimSpace(key) != key {
		return fmt.Errorf("tag key %q must not have leading/trailing spaces", key)
	}
	return nil
}

// SetTags merges the given key=value pairs into the server's tags. A pair whose
// value is empty deletes that key. Passing an empty map is a no-op.
func (s *TagStore) SetTags(server string, kv map[string]string) error {
	if server == "" {
		return fmt.Errorf("server name is required")
	}
	for k := range kv {
		if err := validateTagKey(k); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return err
	}
	tags := doc.Servers[server]
	if tags == nil {
		tags = map[string]string{}
	}
	for k, v := range kv {
		if v == "" {
			delete(tags, k)
			continue
		}
		tags[k] = v
	}
	if len(tags) == 0 {
		delete(doc.Servers, server)
	} else {
		doc.Servers[server] = tags
	}
	return s.write(doc)
}

// GetTags returns a copy of the tags for a server (nil if none).
func (s *TagStore) GetTags(server string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return nil
	}
	tags := doc.Servers[server]
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(tags))
	for k, v := range tags {
		out[k] = v
	}
	return out
}

// AllTags returns a copy of all server tags, keyed by server name.
func (s *TagStore) AllTags() map[string]map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return map[string]map[string]string{}
	}
	out := make(map[string]map[string]string, len(doc.Servers))
	for server, tags := range doc.Servers {
		copyTags := make(map[string]string, len(tags))
		for k, v := range tags {
			copyTags[k] = v
		}
		out[server] = copyTags
	}
	return out
}

// tagFilter is a single parsed key=value condition.
type tagFilter struct {
	key   string
	value string
}

// parseTagExpr parses a filter expression like "role=plesk,env=prod" into a
// conjunction (comma = AND) of key=value conditions.
func parseTagExpr(expr string) ([]tagFilter, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("empty filter expression")
	}
	var filters []tagFilter
	for _, part := range strings.Split(expr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("invalid filter %q: expected key=value", part)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" {
			return nil, fmt.Errorf("invalid filter %q: empty key", part)
		}
		filters = append(filters, tagFilter{key: k, value: v})
	}
	if len(filters) == 0 {
		return nil, fmt.Errorf("empty filter expression")
	}
	return filters, nil
}

// matches reports whether a server's tags satisfy every condition (AND).
func (f tagFilter) matches(tags map[string]string) bool {
	got, ok := tags[f.key]
	return ok && got == f.value
}

// ServersMatching resolves a filter expression (e.g. "role=plesk" or
// "role=plesk,env=prod", comma = AND) against the given server names and the
// stored tags. The returned names are sorted and limited to names in `servers`.
func (s *TagStore) ServersMatching(expr string, servers []string) ([]string, error) {
	filters, err := parseTagExpr(expr)
	if err != nil {
		return nil, err
	}
	all := s.AllTags()
	var matched []string
	for _, name := range servers {
		tags := all[name]
		ok := true
		for _, f := range filters {
			if !f.matches(tags) {
				ok = false
				break
			}
		}
		if ok {
			matched = append(matched, name)
		}
	}
	sort.Strings(matched)
	return matched, nil
}
