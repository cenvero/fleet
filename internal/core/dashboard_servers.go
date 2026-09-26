// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

// serverCacheRacyWindow is how long after a file's modification time a decode
// must have happened before the decoded record is trusted on a stat match. It
// covers filesystems with coarse (1-2s) mtime granularity: a rewrite within
// the same mtime tick as the decode could otherwise go unnoticed.
const serverCacheRacyWindow = 2 * time.Second

// ServerListCache lets a long-running reader such as the live dashboard list
// the fleet repeatedly without TOML-decoding every server file each time. A
// cached record is reused only while the file is the same file (os.SameFile —
// server records are always replaced by rename, so every write is a new file),
// with the same size and modification time, and was decoded comfortably after
// that modification time. Anything else is decoded again, exactly as
// ListServers would.
//
// Records returned through the cache are copies; callers may not observe each
// other's mutations. A zero ServerListCache is ready to use and it is safe for
// concurrent use.
type ServerListCache struct {
	mu      sync.Mutex
	entries map[string]serverCacheEntry
	decodes int // files decoded (not served from the cache); for tests
}

type serverCacheEntry struct {
	info    os.FileInfo
	decoded time.Time
	record  ServerRecord
}

// NewServerListCache returns an empty cache.
func NewServerListCache() *ServerListCache { return &ServerListCache{} }

func (e serverCacheEntry) fresh(info os.FileInfo) bool {
	return e.info != nil && info != nil &&
		os.SameFile(e.info, info) &&
		e.info.Size() == info.Size() &&
		e.info.ModTime().Equal(info.ModTime()) &&
		e.decoded.Sub(info.ModTime()) > serverCacheRacyWindow
}

// listServersCached is ListServers served through a cache. With a nil cache it
// is ListServers.
func (a *App) listServersCached(c *ServerListCache) ([]ServerRecord, error) {
	if c == nil {
		return a.ListServers()
	}
	// Same locking as ListServers: no in-process writer can replace a file
	// between the directory listing and the reads.
	a.serverMu.RLock()
	defer a.serverMu.RUnlock()

	dir := filepath.Join(a.ConfigDir, "servers")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read servers directory: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]serverCacheEntry, len(entries))
	}
	seen := make(map[string]struct{}, len(entries))
	servers := make([]ServerRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".toml" {
			continue
		}
		name := entry.Name()
		seen[name] = struct{}{}
		path := filepath.Join(dir, name)
		info, statErr := os.Stat(path)
		if statErr == nil {
			if cached, ok := c.entries[name]; ok && cached.fresh(info) {
				servers = append(servers, cloneServerRecord(cached.record))
				continue
			}
		}
		decodedAt := time.Now()
		var server ServerRecord
		if _, err := toml.DecodeFile(path, &server); err != nil {
			delete(c.entries, name)
			return nil, fmt.Errorf("decode server %s: %w", name, err)
		}
		c.decodes++
		if statErr == nil {
			c.entries[name] = serverCacheEntry{info: info, decoded: decodedAt, record: server}
		} else {
			delete(c.entries, name)
		}
		servers = append(servers, cloneServerRecord(server))
	}
	for name := range c.entries {
		if _, ok := seen[name]; !ok {
			delete(c.entries, name)
		}
	}
	slices.SortFunc(servers, func(a, b ServerRecord) int {
		return strings.Compare(a.Name, b.Name)
	})
	return servers, nil
}

// cloneServerRecord copies the slices of a record so a cached record is never
// shared with a caller.
func cloneServerRecord(r ServerRecord) ServerRecord {
	r.Capabilities = slices.Clone(r.Capabilities)
	r.Services = slices.Clone(r.Services)
	r.OpenPorts = slices.Clone(r.OpenPorts)
	r.Firewall.Rules = slices.Clone(r.Firewall.Rules)
	return r
}
