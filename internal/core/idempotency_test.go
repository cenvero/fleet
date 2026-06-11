// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIdempotencyPutGet(t *testing.T) {
	store := NewIdempotencyStore(t.TempDir())

	if _, ok := store.Get("missing"); ok {
		t.Fatalf("expected miss for unknown key")
	}

	if err := store.Put("k1", "result-1", time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok := store.Get("k1")
	if !ok {
		t.Fatalf("expected hit for k1")
	}
	if got != "result-1" {
		t.Fatalf("got %q, want %q", got, "result-1")
	}
}

func TestIdempotencyOverwrite(t *testing.T) {
	store := NewIdempotencyStore(t.TempDir())
	if err := store.Put("k", "first", time.Hour); err != nil {
		t.Fatalf("Put first: %v", err)
	}
	if err := store.Put("k", "second", time.Hour); err != nil {
		t.Fatalf("Put second: %v", err)
	}
	got, ok := store.Get("k")
	if !ok || got != "second" {
		t.Fatalf("got %q ok=%v, want second", got, ok)
	}
}

func TestIdempotencyExpiry(t *testing.T) {
	dir := t.TempDir()
	store := NewIdempotencyStore(dir)
	if err := store.Put("k", "value", time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Rewrite the on-disk entry so it is already expired, then confirm Get
	// treats it as a miss and prunes it.
	expirePast(t, IdempotencyPath(dir), "k")

	if _, ok := store.Get("k"); ok {
		t.Fatalf("expected miss for expired key")
	}

	// The expired entry should have been pruned from disk on the missed Get.
	doc := readDoc(t, IdempotencyPath(dir))
	if _, present := doc.Entries["k"]; present {
		t.Fatalf("expected expired entry to be pruned")
	}
}

func TestIdempotencyPutRejectsBadInput(t *testing.T) {
	store := NewIdempotencyStore(t.TempDir())
	if err := store.Put("", "x", time.Hour); err == nil {
		t.Fatalf("expected error for empty key")
	}
	if err := store.Put("k", "x", 0); err == nil {
		t.Fatalf("expected error for non-positive ttl")
	}
	if err := store.Put("k", "x", -time.Second); err == nil {
		t.Fatalf("expected error for negative ttl")
	}
}

func TestIdempotencyGetEmptyKey(t *testing.T) {
	store := NewIdempotencyStore(t.TempDir())
	if _, ok := store.Get(""); ok {
		t.Fatalf("expected miss for empty key")
	}
}

func TestIdempotencyPersistsAcrossStores(t *testing.T) {
	dir := t.TempDir()
	if err := NewIdempotencyStore(dir).Put("k", "persisted", time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// A fresh store over the same dir reads the persisted file.
	got, ok := NewIdempotencyStore(dir).Get("k")
	if !ok || got != "persisted" {
		t.Fatalf("got %q ok=%v, want persisted", got, ok)
	}
}

func TestIdempotencyPutPrunesOtherExpired(t *testing.T) {
	dir := t.TempDir()
	store := NewIdempotencyStore(dir)
	if err := store.Put("stale", "old", time.Hour); err != nil {
		t.Fatalf("Put stale: %v", err)
	}
	expirePast(t, IdempotencyPath(dir), "stale")

	if err := store.Put("fresh", "new", time.Hour); err != nil {
		t.Fatalf("Put fresh: %v", err)
	}
	doc := readDoc(t, IdempotencyPath(dir))
	if _, present := doc.Entries["stale"]; present {
		t.Fatalf("expected stale entry pruned on Put")
	}
	if _, present := doc.Entries["fresh"]; !present {
		t.Fatalf("expected fresh entry retained")
	}
}

func TestIdempotencyFilePermissions(t *testing.T) {
	dir := t.TempDir()
	store := NewIdempotencyStore(dir)
	if err := store.Put("k", "v", time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, err := os.Stat(IdempotencyPath(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("got perm %o, want 0600", perm)
	}
}

// expirePast rewrites the entry for key on disk so its ExpiresAt is in the past.
func expirePast(t *testing.T, path, key string) {
	t.Helper()
	doc := readDoc(t, path)
	entry := doc.Entries[key]
	entry.ExpiresAt = time.Now().UTC().Add(-time.Hour)
	doc.Entries[key] = entry
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readDoc(t *testing.T, path string) idempotencyDocument {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	var doc idempotencyDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Entries == nil {
		doc.Entries = map[string]idempotencyEntry{}
	}
	return doc
}
