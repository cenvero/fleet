// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package transport

import (
	"slices"
	"testing"
)

// The cipher list is ordered for speed but pinned for security. Ordering is
// allowed to vary with the CPU; membership is not. If this test fails, someone
// has widened (or narrowed) what a fleet peer will negotiate — which is the
// thing the pin exists to prevent — rather than merely reordered it.
func TestSupportedCiphersSetIsPinned(t *testing.T) {
	want := []string{
		"aes256-gcm@openssh.com",
		"chacha20-poly1305@openssh.com",
	}
	got := slices.Clone(SupportedCiphers())
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("negotiable cipher set changed:\n got %v\nwant %v", got, want)
	}
}

// Both orderings must contain the same entries, so a controller and an agent
// that disagree about hardware AES still negotiate successfully — they just
// settle on whichever the client prefers.
func TestSupportedCiphersOrderTracksHardwareAES(t *testing.T) {
	got := SupportedCiphers()
	if len(got) < 2 {
		t.Fatalf("expected at least two ciphers, got %v", got)
	}
	first := got[0]
	if hasHardwareAES() {
		if first != "aes256-gcm@openssh.com" {
			t.Fatalf("CPU has hardware AES but prefers %q; AES-GCM is several times faster there", first)
		}
		return
	}
	if first != "chacha20-poly1305@openssh.com" {
		t.Fatalf("CPU lacks hardware AES but prefers %q; ChaCha20 is faster and constant-time there", first)
	}
}
