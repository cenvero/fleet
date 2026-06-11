// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTagStoreSetGet(t *testing.T) {
	dir := t.TempDir()
	store := NewTagStore(dir)

	if err := store.SetTags("web-01", map[string]string{"role": "plesk", "env": "prod"}); err != nil {
		t.Fatalf("SetTags: %v", err)
	}

	got := store.GetTags("web-01")
	want := map[string]string{"role": "plesk", "env": "prod"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GetTags = %v, want %v", got, want)
	}

	// File must exist and be 0600.
	info, err := os.Stat(TagsPath(dir))
	if err != nil {
		t.Fatalf("stat tags.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("tags.json perm = %o, want 600", perm)
	}
}

func TestTagStoreMergeAndDelete(t *testing.T) {
	dir := t.TempDir()
	store := NewTagStore(dir)

	if err := store.SetTags("web-01", map[string]string{"role": "plesk", "env": "prod"}); err != nil {
		t.Fatal(err)
	}
	// Merge a new key; empty value deletes env.
	if err := store.SetTags("web-01", map[string]string{"region": "eu", "env": ""}); err != nil {
		t.Fatal(err)
	}
	got := store.GetTags("web-01")
	want := map[string]string{"role": "plesk", "region": "eu"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after merge/delete = %v, want %v", got, want)
	}

	// Deleting all keys removes the server entirely.
	if err := store.SetTags("web-01", map[string]string{"role": "", "region": ""}); err != nil {
		t.Fatal(err)
	}
	if got := store.GetTags("web-01"); got != nil {
		t.Fatalf("expected nil after deleting all tags, got %v", got)
	}
}

func TestTagStoreGetMissing(t *testing.T) {
	store := NewTagStore(t.TempDir())
	if got := store.GetTags("nope"); got != nil {
		t.Fatalf("expected nil for missing server, got %v", got)
	}
	if got := store.AllTags(); len(got) != 0 {
		t.Fatalf("expected empty AllTags, got %v", got)
	}
}

func TestTagStoreInvalidKey(t *testing.T) {
	store := NewTagStore(t.TempDir())
	for _, bad := range []string{"", "a=b", "a,b", " x"} {
		if err := store.SetTags("web-01", map[string]string{bad: "v"}); err == nil {
			t.Fatalf("expected error for key %q", bad)
		}
	}
}

func TestServersMatching(t *testing.T) {
	dir := t.TempDir()
	store := NewTagStore(dir)
	_ = store.SetTags("web-01", map[string]string{"role": "plesk", "env": "prod"})
	_ = store.SetTags("web-02", map[string]string{"role": "plesk", "env": "dev"})
	_ = store.SetTags("db-01", map[string]string{"role": "db", "env": "prod"})

	servers := []string{"web-01", "web-02", "db-01", "untagged"}

	cases := []struct {
		expr string
		want []string
	}{
		{"role=plesk", []string{"web-01", "web-02"}},
		{"role=plesk,env=prod", []string{"web-01"}},
		{"env=prod", []string{"db-01", "web-01"}},
		{"role=none", nil},
	}
	for _, tc := range cases {
		got, err := store.ServersMatching(tc.expr, servers)
		if err != nil {
			t.Fatalf("ServersMatching(%q): %v", tc.expr, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("ServersMatching(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestServersMatchingBadExpr(t *testing.T) {
	store := NewTagStore(t.TempDir())
	for _, bad := range []string{"", "rolePlesk", " "} {
		if _, err := store.ServersMatching(bad, []string{"a"}); err == nil {
			t.Fatalf("expected error for expr %q", bad)
		}
	}
}

func TestTagStoreCorruptFileIsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(TagsPath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewTagStore(dir)
	if err := store.SetTags("web-01", map[string]string{"a": "b"}); err == nil {
		t.Fatal("expected error writing over corrupt tags file")
	}
	// Ensure the path helper points where we expect.
	if filepath.Base(TagsPath(dir)) != "tags.json" {
		t.Fatalf("unexpected tags path %s", TagsPath(dir))
	}
}
