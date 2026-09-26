// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package safetext

import (
	"testing"
	"unicode/utf8"
)

func TestTerminal(t *testing.T) {
	cases := []struct {
		in, want    string
		keepNewline bool
	}{
		{"plain text: é ü 日本", "plain text: é ü 日本", false},
		{"tab\tstays", "tab\tstays", false},
		{"\x1b]52;c;ZXZpbA==\x07", `\x1b]52;c;ZXZpbA==\x07`, false},
		{"hide\r\x1b[2Kshown", `hide\x0d\x1b[2Kshown`, false},
		{"two\nlines", `two\x0alines`, false},
		{"two\nlines", "two\nlines", true},
		{"c1 \u009b31m", `c1 \x9b31m`, false},
		{"del\x7f", `del\x7f`, false},
		{"bidi \u202eevil", "bidi \\u202eevil", false},
		{"zero\u200bwidth", "zero\\u200bwidth", false},
		{"bad \xff byte", `bad \xff byte`, false},
	}
	for _, c := range cases {
		if got := Terminal(c.in, c.keepNewline); got != c.want {
			t.Errorf("Terminal(%q, %v) = %q, want %q", c.in, c.keepNewline, got, c.want)
		}
	}
}

func TestBound(t *testing.T) {
	if got := Bound("日本語", 4); got != "日" || !utf8.ValidString(got) {
		t.Fatalf("Bound split a rune: %q", got)
	}
	if got := Bound("short", 10); got != "short" {
		t.Fatalf("Bound changed short text: %q", got)
	}
	if got := Bound("abc", -1); got != "" {
		t.Fatalf("Bound with negative max = %q", got)
	}
}
