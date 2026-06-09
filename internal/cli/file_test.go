// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import "testing"

func TestParseSize(t *testing.T) {
	ok := map[string]int64{
		"":     0,
		"1024": 1024,
		"4M":   4 * 1024 * 1024,
		"8m":   8 * 1024 * 1024,
		"2G":   2 * 1024 * 1024 * 1024,
		"512K": 512 * 1024,
		"10MB": 10 * 1024 * 1024,
	}
	for in, want := range ok {
		got, err := parseSize(in)
		if err != nil {
			t.Fatalf("parseSize(%q) error: %v", in, err)
		}
		if got != want {
			t.Fatalf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}

	// Overflow and negative inputs must error, not wrap.
	for _, bad := range []string{"9223372036854775807M", "-5M", "99999999999999999999G", "abc"} {
		if _, err := parseSize(bad); err == nil {
			t.Fatalf("parseSize(%q) expected error", bad)
		}
	}
}
