// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

// Package safetext neutralises untrusted text — anything a managed server or
// its users control, such as agent error messages, agent self-descriptions and
// log lines — before it reaches an operator's terminal or a controller record.
package safetext

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Terminal returns s with every character that could drive a terminal replaced
// by a visible escape: C0 control characters other than tab (and newline when
// keepNewline is set), DEL, C1 control characters, Unicode format characters
// (bidi overrides, zero-width characters) and bytes that are not valid UTF-8.
// Escape sequences therefore show up as text ("\x1b[2J") instead of clearing a
// screen, rewriting a line, setting the clipboard or reordering what is shown.
// Text without any such character is returned unchanged.
func Terminal(s string, keepNewline bool) string {
	if isClean(s, keepNewline) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case unsafeRune(r, keepNewline):
			if r < 0x100 {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func isClean(s string, keepNewline bool) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 {
			// Non-ASCII: decide rune by rune.
			for _, r := range s[i:] {
				if r == utf8.RuneError || unsafeRune(r, keepNewline) {
					return false
				}
			}
			return true
		}
		if unsafeRune(rune(c), keepNewline) {
			return false
		}
	}
	return true
}

func unsafeRune(r rune, keepNewline bool) bool {
	switch {
	case r == '\t', keepNewline && r == '\n':
		return false
	case r < 0x20, r == 0x7f, r >= 0x80 && r < 0xa0:
		return true
	case r < 0x80:
		return false
	}
	return unicode.Is(unicode.Cf, r)
}

// Bound truncates s to at most max bytes without splitting a UTF-8 sequence.
func Bound(s string, max int) string {
	if max < 0 {
		max = 0
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
