// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// hasHiddenRunes reports whether s would not display as itself on a terminal:
// invalid UTF-8, control characters (C0 — including tab, newline and carriage
// return — DEL and C1) or Unicode format characters such as bidi overrides and
// zero-width spaces. Any of these can move the cursor, erase, recolour or
// reorder what an operator sees, so text built from them can pose as other text.
func hasHiddenRunes(s string) bool {
	for i, r := range s {
		if r == utf8.RuneError {
			if _, size := utf8.DecodeRuneInString(s[i:]); size <= 1 {
				return true
			}
		}
		if r == ' ' {
			continue
		}
		if !strconv.IsPrint(r) || unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
}

// displayExact renders s for an operator who must see exactly what it is (an
// approval review, say): unchanged when it displays as itself, otherwise
// Go-quoted so every hidden character shows as a visible escape.
func displayExact(s string) string {
	if !hasHiddenRunes(s) {
		return s
	}
	return strconv.Quote(s)
}

// displayArgs is shellJoin for review output: an argument with hidden
// characters is Go-quoted instead of shell-quoted, so nothing in it reaches the
// terminal verbatim.
func displayArgs(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if hasHiddenRunes(a) {
			parts[i] = strconv.Quote(a)
		} else {
			parts[i] = shellJoin([]string{a})
		}
	}
	return strings.Join(parts, " ")
}
