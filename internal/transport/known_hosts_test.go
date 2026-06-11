// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testPublicKey generates a fresh ed25519 SSH public key for host-key tests.
func testPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh.NewPublicKey: %v", err)
	}
	return sshPub
}

var testRemoteAddr = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2222}

// keyToken returns the base64 key body that knownhosts.Line writes into the
// file (e.g. "AAAAC3NzaC1l..."). The file stores the marshaled public key, not
// its SHA256 fingerprint, so tests must match on this token.
func keyToken(t *testing.T, key ssh.PublicKey) string {
	t.Helper()
	fields := strings.Fields(string(ssh.MarshalAuthorizedKey(key)))
	if len(fields) < 2 {
		t.Fatalf("unexpected marshaled key %q", fields)
	}
	return fields[1]
}

// TestTOFUFirstUsePins verifies that the first connection to an unknown host
// pins its key (TOFU) and records the "pinned" outcome.
func TestTOFUFirstUsePins(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "known_hosts")
	key := testPublicKey(t)

	var state HostKeyState
	cb, err := NewTOFUHostKeyCallback(path, false, &state)
	if err != nil {
		t.Fatalf("NewTOFUHostKeyCallback: %v", err)
	}
	if err := cb("host.example:2222", testRemoteAddr, key); err != nil {
		t.Fatalf("first-use callback should pin, got error: %v", err)
	}
	if state.Outcome != "pinned" {
		t.Fatalf("outcome = %q, want pinned", state.Outcome)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "host.example") {
		t.Fatalf("known_hosts should contain the pinned host, got %q", string(data))
	}
}

// TestTOFUMismatchRejected is the core security check: once a host is pinned, a
// DIFFERENT presented key must be rejected (possible MITM) and must NOT silently
// overwrite the stored pin when forceReplace is false.
func TestTOFUMismatchRejected(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "known_hosts")
	host := "host.example:2222"
	pinned := testPublicKey(t)
	attacker := testPublicKey(t)

	// First use pins the legitimate key.
	cb, err := NewTOFUHostKeyCallback(path, false, nil)
	if err != nil {
		t.Fatalf("NewTOFUHostKeyCallback: %v", err)
	}
	if err := cb(host, testRemoteAddr, pinned); err != nil {
		t.Fatalf("pinning legitimate key: %v", err)
	}
	before, _ := os.ReadFile(path)

	// A fresh callback (re-reads the file) now sees a different key: reject.
	var state HostKeyState
	cb2, err := NewTOFUHostKeyCallback(path, false, &state)
	if err != nil {
		t.Fatalf("NewTOFUHostKeyCallback: %v", err)
	}
	err = cb2(host, testRemoteAddr, attacker)
	if err == nil {
		t.Fatal("mismatched host key must be rejected, got nil error")
	}
	msg := err.Error()
	for _, want := range []string{"host key changed", "possible MITM", "Remove the pin"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q should mention %q", msg, want)
		}
	}
	if state.Outcome != "rejected" {
		t.Fatalf("outcome = %q, want rejected", state.Outcome)
	}

	// The pin on disk must be unchanged — no silent replace.
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("known_hosts was modified on mismatch:\nbefore=%q\nafter=%q", before, after)
	}
	if strings.Contains(string(after), keyToken(t, attacker)) {
		t.Fatal("attacker key must not be written to known_hosts")
	}
}

// TestTOFUForceReplace verifies that the explicit, operator-authorized re-pin
// path (forceReplace=true) does replace a changed key — this is the only way a
// mismatch is accepted.
func TestTOFUForceReplace(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "known_hosts")
	host := "host.example:2222"
	oldKey := testPublicKey(t)
	newKey := testPublicKey(t)

	cb, err := NewTOFUHostKeyCallback(path, false, nil)
	if err != nil {
		t.Fatalf("NewTOFUHostKeyCallback: %v", err)
	}
	if err := cb(host, testRemoteAddr, oldKey); err != nil {
		t.Fatalf("pinning old key: %v", err)
	}

	var state HostKeyState
	cb2, err := NewTOFUHostKeyCallback(path, true, &state)
	if err != nil {
		t.Fatalf("NewTOFUHostKeyCallback: %v", err)
	}
	if err := cb2(host, testRemoteAddr, newKey); err != nil {
		t.Fatalf("forceReplace should accept the new key, got: %v", err)
	}
	if state.Outcome != "replaced" {
		t.Fatalf("outcome = %q, want replaced", state.Outcome)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), keyToken(t, newKey)) {
		t.Fatal("new key should be present after force-replace")
	}
	if strings.Contains(string(data), keyToken(t, oldKey)) {
		t.Fatal("old key should be gone after force-replace")
	}
}

// TestConcurrentPinNoDoublePin exercises the TOCTOU fix: many goroutines pin the
// same new host concurrently. The mutex + re-check must ensure exactly one entry
// is written, never duplicates, and the file stays parseable.
func TestConcurrentPinNoDoublePin(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "known_hosts")
	host := "race.example:2222"
	key := testPublicKey(t)
	wantToken := keyToken(t, key)

	const goroutines = 32
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each goroutine builds its own callback (fresh in-memory view of the
			// file), mirroring how independent dials race in production.
			cb, err := NewTOFUHostKeyCallback(path, false, nil)
			if err != nil {
				errs <- err
				return
			}
			<-start
			if err := cb(host, testRemoteAddr, key); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent pin returned error: %v", err)
	}

	data, _ := os.ReadFile(path)
	count := strings.Count(string(data), wantToken)
	if count != 1 {
		t.Fatalf("host key written %d times, want exactly 1\nfile:\n%s", count, data)
	}
}

// TestConcurrentDistinctHostsPin pins many DIFFERENT hosts concurrently to make
// sure the shared lock serializes appends without dropping any entry.
func TestConcurrentDistinctHostsPin(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "known_hosts")

	const hosts = 24
	keys := make([]ssh.PublicKey, hosts)
	for i := range keys {
		keys[i] = testPublicKey(t)
	}

	var wg sync.WaitGroup
	errs := make(chan error, hosts)
	start := make(chan struct{})
	for i := 0; i < hosts; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cb, err := NewTOFUHostKeyCallback(path, false, nil)
			if err != nil {
				errs <- err
				return
			}
			<-start
			host := fmt.Sprintf("h%d.example:2222", idx)
			if err := cb(host, testRemoteAddr, keys[idx]); err != nil {
				errs <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent distinct-host pin error: %v", err)
	}

	data, _ := os.ReadFile(path)
	for i := 0; i < hosts; i++ {
		token := keyToken(t, keys[i])
		if c := strings.Count(string(data), token); c != 1 {
			t.Fatalf("host h%d pinned %d times, want 1", i, c)
		}
	}
}
