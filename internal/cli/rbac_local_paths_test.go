// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
)

// TestScopedTokenCannotReachProtectedLocalPaths: a token scoped to one server
// (even with --destructive) must not use the local side of a transfer to read
// or write the controller's own protected files — uploading the controller's
// private key to its server (and using it from there against every other
// server), or downloading an attacker-made tokens.json / secrets / cmd-policy
// over the real one. The same locations the web UI's Local source refuses.
func TestScopedTokenCannotReachProtectedLocalPaths(t *testing.T) {
	dir, fake := setupExecFanout(t, map[string]fakeExecBehavior{"srv-01": {}}, nil)
	tok, err := core.NewTokenStore(dir).Create(core.Token{Name: "agent", Servers: []string{"srv-01"}, DestructiveAllowed: true})
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "keys", "id_ed25519")
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("controller key missing: %v", err)
	}
	parent := filepath.Dir(dir)

	denied := []struct {
		name string
		args []string
	}{
		{"upload controller key", []string{"file", "upload", "srv-01", key, "/tmp/k"}},
		{"upload config dir tree", []string{"file", "upload", "-r", "srv-01", dir, "/tmp/k"}},
		{"upload tree containing config dir", []string{"file", "upload", "-r", "srv-01", parent, "/tmp/k"}},
		{"download over tokens.json", []string{"file", "download", "srv-01", "/tmp/t.json", filepath.Join(dir, "tokens.json")}},
		{"download into keys dir", []string{"file", "download", "srv-01", "/tmp/id_ed25519", filepath.Join(dir, "keys")}},
		{"download tree over config dir parent", []string{"file", "download", "-r", "srv-01", "/tmp/x", parent}},
		{"sync push of config dir", []string{"sync", "srv-01", dir, "/tmp/mirror"}},
		{"sync pull over config dir parent", []string{"sync", "--from", "remote", "srv-01", parent, "/tmp/x"}},
		{"service log export over known_hosts", []string{"service", "logs", "srv-01", "nginx", "--export", filepath.Join(dir, "keys", "known_hosts")}},
	}
	for _, c := range denied {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("FLEET_TOKEN", tok.ID)
			before := fake.calls.Load()
			res := runExecFleet(t, dir, c.args...)
			if res.err == nil || !strings.Contains(res.err.Error(), "denied") {
				t.Errorf("err = %v, want a scoped-token denial (stderr=%q)", res.err, res.stderr)
			}
			if n := fake.calls.Load() - before; n != 0 {
				t.Errorf("%d RPC(s) reached the server before the denial", n)
			}
		})
	}

	// A path outside the protected locations is still fine for the token.
	plain := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(plain, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_TOKEN", tok.ID)
	if res := runExecFleet(t, dir, "file", "upload", "srv-01", plain, "/tmp/notes.txt"); res.err != nil && strings.Contains(res.err.Error(), "denied") {
		t.Fatalf("an ordinary local file was denied to the scoped token: %v", res.err)
	}

	// Without a token (or with an unscoped one) nothing changes.
	t.Setenv("FLEET_TOKEN", "")
	before := fake.calls.Load()
	if res := runExecFleet(t, dir, "file", "upload", "srv-01", key, "/tmp/k"); res.err != nil && strings.Contains(res.err.Error(), "denied") {
		t.Fatalf("an unscoped upload was denied: %v", res.err)
	}
	if fake.calls.Load() == before {
		t.Fatal("an unscoped upload never reached the server")
	}
}
