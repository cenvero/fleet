// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"testing"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

func TestEditResultCacheBoundsEntriesAndOriginals(t *testing.T) {
	t.Parallel()
	now := time.Now()
	c := newEditResultCache(3, 10, time.Minute)
	c.put("a", "/f", proto.FileEditResult{NewSHA256: "1", Original: []byte("123456")}, now)
	c.put("b", "/f", proto.FileEditResult{NewSHA256: "2", Original: []byte("123456")}, now)
	// Over the 10-byte budget: a's original is dropped, its result is kept.
	a, ok := c.get("a", "/f", now)
	if !ok || a.NewSHA256 != "1" || a.Original != nil {
		t.Fatalf("a = %+v, %v", a, ok)
	}
	if b, _ := c.get("b", "/f", now); string(b.Original) != "123456" {
		t.Fatalf("b lost its original: %+v", b)
	}
	c.put("big", "/f", proto.FileEditResult{Original: make([]byte, 11)}, now)
	if big, _ := c.get("big", "/f", now); big.Original != nil {
		t.Fatal("an original over the whole budget must not be kept")
	}
	c.put("d", "/f", proto.FileEditResult{}, now)
	if _, ok := c.get("a", "/f", now); ok {
		t.Fatal("the oldest entry should be evicted past 3 entries")
	}
	if _, ok := c.get("b", "/other", now); ok {
		t.Fatal("an id must only match the path it was used with")
	}
	if _, ok := c.get("b", "/f", now.Add(2*time.Minute)); ok {
		t.Fatal("entries must expire")
	}
	if c.origBytes < 0 || c.origBytes > 10 {
		t.Fatalf("origBytes = %d", c.origBytes)
	}
}
