// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"fmt"
	"testing"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

func newTestCache() *dirListCache {
	return &dirListCache{entries: make(map[string]listCacheEntry)}
}

// The regression this guards: the cache was a sync.Map that was never evicted,
// so a session browsing many directories grew it for the life of the process.
// The cache key embeds the operator-supplied path, so its cardinality is
// effectively unbounded.
func TestListCacheStaysBounded(t *testing.T) {
	c := newTestCache()
	for i := 0; i < listCacheMaxEntries*4; i++ {
		c.put(fmt.Sprintf("server\x00/dir/%d\x000", i), proto.FileListResult{})
	}
	c.mu.Lock()
	got := len(c.entries)
	c.mu.Unlock()
	if got > listCacheMaxEntries {
		t.Errorf("cache grew past its cap: got %d entries, cap %d", got, listCacheMaxEntries)
	}
}

func TestListCacheRoundTripAndExpiry(t *testing.T) {
	c := newTestCache()
	const key = "srv\x00/tmp\x000"
	c.put(key, proto.FileListResult{Path: "/tmp"})

	got, ok := c.get(key)
	if !ok {
		t.Fatal("expected a cache hit immediately after put")
	}
	if got.Path != "/tmp" {
		t.Errorf("cached wrong value: %+v", got)
	}

	// Force expiry rather than sleeping for the real TTL.
	c.mu.Lock()
	c.entries[key] = listCacheEntry{result: got, expires: time.Now().Add(-time.Second)}
	c.mu.Unlock()

	if _, ok := c.get(key); ok {
		t.Error("expected a miss for an expired entry")
	}
	// An expired entry must be dropped on access, not merely skipped — that is
	// what stops expired keys accumulating.
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n != 0 {
		t.Errorf("expired entry was not evicted on access: %d entries remain", n)
	}
}

func TestListCacheInvalidateClearsEverything(t *testing.T) {
	c := newTestCache()
	c.put("a\x00/1\x000", proto.FileListResult{})
	c.put("b\x00/2\x000", proto.FileListResult{})
	c.invalidate()
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n != 0 {
		t.Errorf("invalidate left %d entries", n)
	}
	if _, ok := c.get("a\x00/1\x000"); ok {
		t.Error("expected a miss after invalidate")
	}
}
