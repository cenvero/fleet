// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestLoadAuthorizedKeysSkipsMalformedLineWithoutCorruptingNextKey(t *testing.T) {
	t.Parallel()

	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	sshPublicKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatalf("NewPublicKey() error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "authorized_keys")
	data := append([]byte("not-a-valid-authorized-key\n"), ssh.MarshalAuthorizedKey(sshPublicKey)...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(authorized_keys) error = %v", err)
	}

	keys, err := loadAuthorizedKeys(path)
	if err != nil {
		t.Fatalf("loadAuthorizedKeys() error = %v", err)
	}
	if _, ok := keys[string(sshPublicKey.Marshal())]; !ok {
		t.Fatalf("expected valid key after malformed line to be loaded")
	}
}
