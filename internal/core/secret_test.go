// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleetcrypto "github.com/cenvero/fleet/internal/crypto"
)

// writeControllerKey generates a fresh controller ed25519 key set under
// <configDir>/keys, which is the HKDF input for the derived secret-encryption
// key (see secret_crypto.go). Returns the keys dir.
func writeControllerKey(t *testing.T, configDir string) string {
	t.Helper()
	keysDir := filepath.Join(configDir, "keys")
	if err := fleetcrypto.GenerateKeySet(keysDir, fleetcrypto.AlgorithmEd25519, nil); err != nil {
		t.Fatalf("GenerateKeySet: %v", err)
	}
	return keysDir
}

// promoteFreshControllerKey simulates the controller key promotion performed by
// RotateKeys: it generates a brand-new ed25519 key set in a temp dir and copies
// id_ed25519/id_ed25519.pub over the active ones, changing the HKDF input and
// therefore the derived secret-encryption key.
func promoteFreshControllerKey(t *testing.T, keysDir string) {
	t.Helper()
	tmp := t.TempDir()
	if err := fleetcrypto.GenerateKeySet(tmp, fleetcrypto.AlgorithmEd25519, nil); err != nil {
		t.Fatalf("GenerateKeySet (new): %v", err)
	}
	for _, name := range []string{"id_ed25519", "id_ed25519.pub"} {
		data, err := os.ReadFile(filepath.Join(tmp, name))
		if err != nil {
			t.Fatalf("read new key %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(keysDir, name), data, 0o600); err != nil {
			t.Fatalf("promote key %s: %v", name, err)
		}
	}
}

// TestSecretStoreRekeyAcrossControllerKeyRotation is the regression test for the
// HIGH data-loss bug: the secret-encryption AES key is HKDF-derived from the
// controller private key, so rotating that key would orphan every ciphertext
// unless secrets are re-sealed. It mirrors the RotateKeys ordering: snapshot the
// old derived key, promote a new controller key, derive the new key, Rekey, then
// confirm Get still returns the original plaintext.
func TestSecretStoreRekeyAcrossControllerKeyRotation(t *testing.T) {
	dir := t.TempDir()
	keysDir := writeControllerKey(t, dir)

	store := NewSecretStore(dir)
	const (
		name  = "db_password"
		value = "correct horse battery staple"
	)
	if err := store.Set(name, value); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// 1. Snapshot the pre-rotation derived key (controller key still active).
	oldKey, err := DeriveSecretKey(dir)
	if err != nil {
		t.Fatalf("DeriveSecretKey (old): %v", err)
	}

	// 2. Promote a new controller key — this is what would break decryption.
	promoteFreshControllerKey(t, keysDir)

	// 3. Derive the new key (now reads the promoted controller key).
	newKey, err := DeriveSecretKey(dir)
	if err != nil {
		t.Fatalf("DeriveSecretKey (new): %v", err)
	}
	if string(oldKey) == string(newKey) {
		t.Fatal("derived secret key did not change across controller key rotation; test would not exercise the bug")
	}

	// 4. Re-seal under the new key.
	if err := store.Rekey(oldKey, newKey); err != nil {
		t.Fatalf("Rekey: %v", err)
	}

	// 5. A FRESH store (cold read, new derived key from disk) must still decrypt.
	got, err := NewSecretStore(dir).Get(name)
	if err != nil {
		t.Fatalf("Get after rotation+Rekey: %v", err)
	}
	if got != value {
		t.Fatalf("Get after rotation = %q, want %q", got, value)
	}
}

// TestSecretStoreRotationWithoutRekeyOrphansSecrets documents the bug the fix
// guards against: promoting a new controller key WITHOUT a Rekey makes every
// secret undecryptable.
func TestSecretStoreRotationWithoutRekeyOrphansSecrets(t *testing.T) {
	dir := t.TempDir()
	keysDir := writeControllerKey(t, dir)

	if err := NewSecretStore(dir).Set("k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	promoteFreshControllerKey(t, keysDir)

	// Without Rekey, a fresh store derives the NEW key and cannot open the old
	// ciphertext.
	if _, err := NewSecretStore(dir).Get("k"); err == nil {
		t.Fatal("expected Get to fail after controller key rotation without Rekey (orphaned ciphertext)")
	}
}

// TestSecretStoreRekeyNoSecrets is a no-op safety check: rekeying an empty store
// must not error.
func TestSecretStoreRekeyNoSecrets(t *testing.T) {
	dir := t.TempDir()
	writeControllerKey(t, dir)
	store := NewSecretStore(dir)
	oldKey, err := DeriveSecretKey(dir)
	if err != nil {
		t.Fatalf("DeriveSecretKey: %v", err)
	}
	if err := store.Rekey(oldKey, oldKey); err != nil {
		t.Fatalf("Rekey on empty store: %v", err)
	}
}

// TestSecretStoreRekeyWrongOldKeyAborts confirms Rekey fails cleanly (no write)
// when the old key cannot decrypt the existing ciphertext, so a bad key can never
// corrupt the store.
func TestSecretStoreRekeyWrongOldKeyAborts(t *testing.T) {
	dir := t.TempDir()
	writeControllerKey(t, dir)
	store := NewSecretStore(dir)
	if err := store.Set("k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	correctKey, err := DeriveSecretKey(dir)
	if err != nil {
		t.Fatalf("DeriveSecretKey: %v", err)
	}
	wrongOld := make([]byte, 32) // all-zero key, will not decrypt
	newKey := make([]byte, 32)
	for i := range newKey {
		newKey[i] = 0xAB
	}
	if err := store.Rekey(wrongOld, newKey); err == nil {
		t.Fatal("Rekey with wrong old key should fail")
	}
	// The store must be untouched: the original (correct-key) value still opens.
	got, err := NewSecretStore(dir).Get("k")
	if err != nil {
		t.Fatalf("Get after failed Rekey: %v", err)
	}
	if got != "v" {
		t.Fatalf("Get after failed Rekey = %q, want %q", got, "v")
	}
	_ = correctKey
}

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

// TestSecretStoreEncryptedAtRest asserts that after Set, the raw on-disk bytes
// of secrets.json never contain the plaintext value, carry the aesgcm-v1 marker,
// and that Get round-trips back to the exact original value.
func TestSecretStoreEncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	store := NewSecretStore(dir)

	const plaintext = "PLAINTEXT-CANARY-do-not-store-on-disk-12345"
	if err := store.Set("api_key", plaintext); err != nil {
		t.Fatalf("Set: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "secrets.json"))
	if err != nil {
		t.Fatalf("read secrets.json: %v", err)
	}
	if strings.Contains(string(raw), plaintext) {
		t.Fatal("secrets.json contains the plaintext secret value at rest")
	}
	if !strings.Contains(string(raw), "aesgcm-v1") {
		t.Fatalf("secrets.json missing encryption marker; got:\n%s", raw)
	}

	// Get returns the exact original value (decrypts correctly).
	got, err := store.Get("api_key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != plaintext {
		t.Fatalf("Get returned %q, want %q", got, plaintext)
	}

	// A fresh store over the same dir derives the same key and decrypts too.
	fresh := NewSecretStore(dir)
	got, err = fresh.Get("api_key")
	if err != nil {
		t.Fatalf("Get from fresh store: %v", err)
	}
	if got != plaintext {
		t.Fatalf("fresh store Get returned %q, want %q", got, plaintext)
	}
}

// TestSecretStoreRotateEncrypted asserts a rotated value is stored encrypted
// (not present in plaintext on disk) and round-trips through Get.
func TestSecretStoreRotateEncrypted(t *testing.T) {
	dir := t.TempDir()
	store := NewSecretStore(dir)
	if err := store.Set("db", "original"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Rotate("db", 48); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	rotated, err := store.Get("db")
	if err != nil {
		t.Fatalf("Get after Rotate: %v", err)
	}
	if rotated == "original" || len(rotated) != 48 {
		t.Fatalf("Rotate produced unexpected value %q (len %d)", rotated, len(rotated))
	}
	raw, err := os.ReadFile(filepath.Join(dir, "secrets.json"))
	if err != nil {
		t.Fatalf("read secrets.json: %v", err)
	}
	if strings.Contains(string(raw), rotated) {
		t.Fatal("secrets.json contains the rotated plaintext value at rest")
	}
}

// TestSecretStoreLegacyPlaintextMigration writes a legacy plaintext secrets.json
// (no encryption marker), confirms it is read back correctly for back-compat,
// and confirms the next write transparently re-encrypts every value with no loss.
func TestSecretStoreLegacyPlaintextMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")

	// Hand-craft a legacy document: records carry only value+created, no "enc".
	legacy := map[string]any{
		"secrets": map[string]any{
			"legacy_a": map[string]any{"value": "legacy-value-A", "created": time.Now().UTC()},
			"legacy_b": map[string]any{"value": "legacy-value-B", "created": time.Now().UTC()},
		},
	}
	data, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	// Sanity: the legacy file really is plaintext.
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "legacy-value-A") {
		t.Fatal("test setup: legacy file is not plaintext as expected")
	}

	store := NewSecretStore(dir)

	// Back-compat: legacy plaintext reads correctly.
	if got, err := store.Get("legacy_a"); err != nil || got != "legacy-value-A" {
		t.Fatalf("Get(legacy_a) = %q, %v; want legacy-value-A, nil", got, err)
	}
	if got, err := store.Get("legacy_b"); err != nil || got != "legacy-value-B" {
		t.Fatalf("Get(legacy_b) = %q, %v; want legacy-value-B, nil", got, err)
	}

	// The next mutating write must upgrade EVERY record, including the untouched
	// legacy_b, not just the one being added.
	if err := store.Set("new_c", "new-value-C"); err != nil {
		t.Fatalf("Set new_c: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read upgraded file: %v", err)
	}
	for _, plaintext := range []string{"legacy-value-A", "legacy-value-B", "new-value-C"} {
		if strings.Contains(string(raw), plaintext) {
			t.Fatalf("after upgrade, secrets.json still contains plaintext %q", plaintext)
		}
	}
	// Every record must now be encrypted: confirm via the marker count.
	if n := strings.Count(string(raw), "aesgcm-v1"); n != 3 {
		t.Fatalf("expected 3 encrypted records after upgrade, found %d markers", n)
	}
	// All values still readable after the upgrade — nothing lost.
	for name, want := range map[string]string{
		"legacy_a": "legacy-value-A",
		"legacy_b": "legacy-value-B",
		"new_c":    "new-value-C",
	} {
		if got, err := store.Get(name); err != nil || got != want {
			t.Fatalf("after upgrade Get(%s) = %q, %v; want %q", name, got, err, want)
		}
	}
}

// TestSecretKeySourcePinnedToControllerKey is the regression for the MED finding:
// once the secret key is derived from the controller key, the SOURCE is pinned.
// Deleting the controller key (so derivation would otherwise silently fall back
// to a fresh dedicated key file and a DIFFERENT key) must instead fail with a
// clear "source unavailable" error rather than bricking decryption silently.
func TestSecretKeySourcePinnedToControllerKey(t *testing.T) {
	dir := t.TempDir()
	keysDir := writeControllerKey(t, dir)

	store := NewSecretStore(dir)
	if err := store.Set("k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// The pin file must now record the controller source.
	pin, err := os.ReadFile(filepath.Join(keysDir, secretKeySourceFile))
	if err != nil {
		t.Fatalf("read pin: %v", err)
	}
	if got := strings.TrimSpace(string(pin)); got != "controller:id_ed25519" {
		t.Fatalf("pinned source = %q, want controller:id_ed25519", got)
	}

	// Remove the controller private key. A fresh derivation must NOT silently flip
	// to the dedicated key file; it must error clearly.
	if err := os.Remove(filepath.Join(keysDir, "id_ed25519")); err != nil {
		t.Fatalf("remove controller key: %v", err)
	}
	_, err = deriveSecretKey(dir)
	if err == nil {
		t.Fatal("deriveSecretKey should fail when the pinned controller key is gone")
	}
	if !strings.Contains(err.Error(), "unavailable") || !strings.Contains(err.Error(), "controller:id_ed25519") {
		t.Fatalf("error = %q, want a clear 'source unavailable' for controller:id_ed25519", err)
	}
}

// TestSecretKeySourcePinnedToDedicatedFile asserts the pin also covers the
// dedicated-file path: a bare dir (no controller key) pins to the dedicated file,
// and the recorded source is honored on later reads.
func TestSecretKeySourcePinnedToDedicatedFile(t *testing.T) {
	dir := t.TempDir() // no controller key present
	store := NewSecretStore(dir)
	if err := store.Set("k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	pin, err := os.ReadFile(filepath.Join(dir, "keys", secretKeySourceFile))
	if err != nil {
		t.Fatalf("read pin: %v", err)
	}
	if got := strings.TrimSpace(string(pin)); got != "dedicated:secret.key" {
		t.Fatalf("pinned source = %q, want dedicated:secret.key", got)
	}
	// A fresh store still reads it back (dedicated key file is stable).
	if got, err := NewSecretStore(dir).Get("k"); err != nil || got != "v" {
		t.Fatalf("Get from fresh store = %q, %v; want v, nil", got, err)
	}
}

// TestSecretKeyPinBlocksControllerToDedicatedFlip proves the exact silent-flip
// the finding warns about is now blocked: with secrets pinned to the controller
// key, even if a dedicated secret.key is ALSO present on disk, removing the
// controller key must error rather than decrypt-with-the-wrong-(dedicated)-key.
func TestSecretKeyPinBlocksControllerToDedicatedFlip(t *testing.T) {
	dir := t.TempDir()
	keysDir := writeControllerKey(t, dir)

	store := NewSecretStore(dir)
	if err := store.Set("k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Stage a dedicated key file too, so a naive fallback WOULD find a usable key.
	if _, err := loadOrCreateSecretKeyFile(keysDir); err != nil {
		t.Fatalf("stage dedicated key: %v", err)
	}
	// Remove the pinned controller key.
	if err := os.Remove(filepath.Join(keysDir, "id_ed25519")); err != nil {
		t.Fatalf("remove controller key: %v", err)
	}

	// Get must fail clearly, NOT silently return garbage from the dedicated key.
	if _, err := NewSecretStore(dir).Get("k"); err == nil {
		t.Fatal("Get should fail (pinned source gone), not flip to the dedicated key file")
	}
}

// TestSecretKeyHonorsConfigPrimaryKey covers the LOW finding: derivation must
// honor Config.Crypto.PrimaryKey when choosing the controller key source. With a
// config that names id_rsa4096 as primary AND both key types present, the pinned
// source must be id_rsa4096, not the hard-coded id_ed25519 default.
func TestSecretKeyHonorsConfigPrimaryKey(t *testing.T) {
	dir := t.TempDir()
	keysDir := filepath.Join(dir, "keys")
	// Generate BOTH an ed25519 and an rsa4096 controller key set.
	if err := fleetcrypto.GenerateKeySet(keysDir, fleetcrypto.AlgorithmEd25519, nil); err != nil {
		t.Fatalf("GenerateKeySet ed25519: %v", err)
	}
	if err := fleetcrypto.GenerateKeySet(keysDir, fleetcrypto.AlgorithmRSA4096, nil); err != nil {
		t.Fatalf("GenerateKeySet rsa: %v", err)
	}
	// Write a config that selects id_rsa4096 as the primary key.
	cfg := DefaultConfig(dir)
	cfg.Crypto.PrimaryKey = "id_rsa4096"
	cfg.Crypto.Algorithm = "rsa"
	if err := SaveConfig(ConfigPath(dir), cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	if order := controllerKeyOrder(dir); len(order) == 0 || order[0] != "id_rsa4096" {
		t.Fatalf("controllerKeyOrder = %v, want id_rsa4096 first", order)
	}

	store := NewSecretStore(dir)
	if err := store.Set("k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	pin, err := os.ReadFile(filepath.Join(keysDir, secretKeySourceFile))
	if err != nil {
		t.Fatalf("read pin: %v", err)
	}
	if got := strings.TrimSpace(string(pin)); got != "controller:id_rsa4096" {
		t.Fatalf("pinned source = %q, want controller:id_rsa4096 (PrimaryKey honored)", got)
	}
}
