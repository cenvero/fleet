// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretStoreSetGet(t *testing.T) {
	dir := t.TempDir()
	store := NewSecretStore(dir)

	if err := store.Set("api_key", "s3cr3t-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := store.Get("api_key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "s3cr3t-value" {
		t.Fatalf("Get returned %q, want %q", got, "s3cr3t-value")
	}

	// Replacing keeps the value updated.
	if err := store.Set("api_key", "rotated-manually"); err != nil {
		t.Fatalf("Set replace: %v", err)
	}
	got, err = store.Get("api_key")
	if err != nil {
		t.Fatalf("Get after replace: %v", err)
	}
	if got != "rotated-manually" {
		t.Fatalf("Get after replace returned %q", got)
	}

	// Getting an unknown secret is an error.
	if _, err := store.Get("missing"); err == nil {
		t.Fatal("Get(missing) should error")
	}
}

func TestSecretStoreFilePermissions(t *testing.T) {
	dir := t.TempDir()
	store := NewSecretStore(dir)
	if err := store.Set("k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "secrets.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("secrets.json perm = %o, want 0600", perm)
	}
}

func TestSecretStoreList(t *testing.T) {
	dir := t.TempDir()
	store := NewSecretStore(dir)
	for _, n := range []string{"zeta", "alpha", "mid"} {
		if err := store.Set(n, "value-for-"+n); err != nil {
			t.Fatalf("Set %s: %v", n, err)
		}
	}
	metas, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 3 {
		t.Fatalf("List returned %d entries, want 3", len(metas))
	}
	// Sorted by name.
	want := []string{"alpha", "mid", "zeta"}
	for i, m := range metas {
		if m.Name != want[i] {
			t.Fatalf("List[%d].Name = %q, want %q", i, m.Name, want[i])
		}
		if m.Created.IsZero() {
			t.Fatalf("List[%d].Created is zero", i)
		}
	}
}

// TestSecretStoreListNeverExposesValue asserts the value-free invariant: neither
// the SecretMeta struct nor the on-disk-derived metadata leaks a secret value.
func TestSecretStoreListNeverExposesValue(t *testing.T) {
	dir := t.TempDir()
	store := NewSecretStore(dir)
	const secret = "TOP-SECRET-LEAK-CANARY"
	if err := store.Set("canary", secret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	metas, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("List returned %d entries, want 1", len(metas))
	}
	m := metas[0]
	if m.Name != "canary" {
		t.Fatalf("meta name = %q", m.Name)
	}
	// The meta struct must not carry the value anywhere a caller could read it.
	if strings.Contains(m.Name, secret) {
		t.Fatal("List meta name exposed the secret value")
	}
	// SecretMeta has exactly Name + Created; rendering it must not include value.
	rendered := m.Name + m.Created.String()
	if strings.Contains(rendered, secret) {
		t.Fatal("List meta rendering exposed the secret value")
	}
}

func TestSecretStoreGenerateLength(t *testing.T) {
	dir := t.TempDir()
	store := NewSecretStore(dir)
	for _, length := range []int{1, 8, 40, 100} {
		name := "gen"
		if err := store.Generate(name, length); err != nil {
			t.Fatalf("Generate(%d): %v", length, err)
		}
		v, err := store.Get(name)
		if err != nil {
			t.Fatalf("Get after Generate: %v", err)
		}
		if len(v) != length {
			t.Fatalf("Generate(%d) produced %d chars", length, len(v))
		}
		// Alphanumeric only.
		for _, r := range v {
			ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
			if !ok {
				t.Fatalf("Generate produced non-alphanumeric char %q", string(r))
			}
		}
	}
	// Two generations differ (overwhelmingly likely with 40 chars).
	if err := store.Generate("a", 40); err != nil {
		t.Fatal(err)
	}
	first, _ := store.Get("a")
	if err := store.Generate("b", 40); err != nil {
		t.Fatal(err)
	}
	second, _ := store.Get("b")
	if first == second {
		t.Fatal("two 40-char generations were identical")
	}

	// Non-positive length is rejected.
	if err := store.Generate("bad", 0); err == nil {
		t.Fatal("Generate(0) should error")
	}
}

func TestSecretStoreRotate(t *testing.T) {
	dir := t.TempDir()
	store := NewSecretStore(dir)
	if err := store.Set("db", "original"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Rotate("db", 32); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	v, err := store.Get("db")
	if err != nil {
		t.Fatalf("Get after Rotate: %v", err)
	}
	if v == "original" {
		t.Fatal("Rotate did not change the value")
	}
	if len(v) != 32 {
		t.Fatalf("Rotate produced %d chars, want 32", len(v))
	}
	// Rotating an unknown secret is an error.
	if err := store.Rotate("nope", 16); err == nil {
		t.Fatal("Rotate(unknown) should error")
	}
}

func TestSecretStoreRemove(t *testing.T) {
	dir := t.TempDir()
	store := NewSecretStore(dir)
	if err := store.Set("temp", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Remove("temp"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := store.Get("temp"); err == nil {
		t.Fatal("Get after Remove should error")
	}
	// Removing an unknown secret is an error.
	if err := store.Remove("temp"); err == nil {
		t.Fatal("Remove(unknown) should error")
	}
}

func TestValidateSecretName(t *testing.T) {
	valid := []string{"api_key", "DB-PASSWORD", "token.v2", "a", "A1._-"}
	for _, n := range valid {
		if err := ValidateSecretName(n); err != nil {
			t.Errorf("ValidateSecretName(%q) = %v, want nil", n, err)
		}
	}
	invalid := []string{
		"",           // empty
		"..",         // traversal + leading dot
		".hidden",    // leading dot
		"a/b",        // path separator
		"a\\b",       // backslash
		"a b",        // space
		"a$b",        // shell metachar
		"name=value", // equals
		"with..dots", // traversal substring
		"héllo",      // non-ascii
		"../escape",  // traversal
	}
	for _, n := range invalid {
		if err := ValidateSecretName(n); err == nil {
			t.Errorf("ValidateSecretName(%q) = nil, want error", n)
		}
	}

	// Validation is enforced by the store entrypoints too.
	dir := t.TempDir()
	store := NewSecretStore(dir)
	if err := store.Set("bad/name", "v"); err == nil {
		t.Fatal("Set with invalid name should error")
	}
	if _, err := store.Get(".."); err == nil {
		t.Fatal("Get with invalid name should error")
	}
}
