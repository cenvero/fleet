// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

// ============================================================================
// Display-width aware text helpers
//
// File names are untrusted input: they come from the local disk or from a
// remote agent, may contain wide (CJK, emoji) characters that occupy two
// terminal cells, combining marks that occupy none, and even raw control
// characters. Everything the file manager draws goes through these helpers so
// that (a) a row is always exactly as wide as the layout says, and (b) no name
// can smuggle a terminal escape sequence onto the operator's screen.
// ============================================================================

// fmIsASCII reports whether s is printable 7-bit ASCII, the overwhelmingly
// common case, where byte length equals display width.
func fmIsASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

// fmSanitize replaces control characters (C0, DEL, C1 and the bidi/zero-width
// formatting characters that can visually reorder text) with visible
// placeholders so untrusted names, paths, error messages and file contents can
// never inject escape sequences or spoof what is on screen. Tabs become a
// single space; callers that want tab stops expand them first.
func fmSanitize(s string) string {
	if fmIsASCII(s) {
		return s
	}
	needs := false
	for _, r := range s {
		if fmUnsafeRune(r) || r == utf8.RuneError {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteByte(' ')
		case r == utf8.RuneError:
			// Undecodable bytes (and a genuine U+FFFD) render as U+FFFD.
			b.WriteRune('\uFFFD')
		case fmUnsafeRune(r):
			b.WriteRune(fmControlPicture(r))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fmUnsafeRune reports runes that must not reach the terminal verbatim.
func fmUnsafeRune(r rune) bool {
	if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
		return true
	}
	switch r {
	case 0x200e, 0x200f, // LRM / RLM
		0x202a, 0x202b, 0x202c, 0x202d, 0x202e, // embeddings / overrides
		0x2066, 0x2067, 0x2068, 0x2069: // isolates
		return true
	}
	return false
}

// fmControlPicture maps a control rune to a visible stand-in: the Unicode
// "control pictures" block for C0 (␛ for ESC, ␊ for LF, …) and ␡ for DEL;
// everything else becomes the replacement character.
func fmControlPicture(r rune) rune {
	switch {
	case r < 0x20:
		return 0x2400 + r
	case r == 0x7f:
		return '\u2421'
	default:
		return '\uFFFD'
	}
}

// fmWidth is the terminal display width of s (no ANSI expected).
func fmWidth(s string) int {
	if fmIsASCII(s) {
		return len(s)
	}
	return lipgloss.Width(s)
}

// fmFit truncates s to at most w display columns, ending with "…" when it
// had to cut. It never splits a multi-byte rune and it measures wide runes
// correctly, so the result is always <= w columns.
func fmFit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if fmWidth(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return fmPrefixWithin(s, w-1) + "…"
}

// fmPrefixWithin returns the longest rune prefix of s whose display width is
// <= w.
func fmPrefixWithin(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if fmIsASCII(s) {
		if len(s) <= w {
			return s
		}
		return s[:w]
	}
	// Walk runes accumulating width. Measuring each rune on its own slightly
	// over-counts some grapheme clusters (e.g. ZWJ emoji), which only makes the
	// cut conservative — the final width is re-measured by callers that pad.
	width := 0
	for i, r := range s {
		rw := fmRuneWidth(r)
		if width+rw > w {
			return s[:i]
		}
		width += rw
	}
	return s
}

// fmSuffixWithin returns the longest rune suffix of s whose width is <= w.
func fmSuffixWithin(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if fmIsASCII(s) {
		if len(s) <= w {
			return s
		}
		return s[len(s)-w:]
	}
	width := 0
	i := len(s)
	for i > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:i])
		rw := fmRuneWidth(r)
		if width+rw > w {
			break
		}
		width += rw
		i -= size
	}
	return s[i:]
}

// fmRuneWidth is the display width of a single rune. ASCII is answered inline;
// anything else defers to lipgloss (which uses the same grapheme width tables
// as the rest of the renderer), with zero-width marks reported as 0.
func fmRuneWidth(r rune) int {
	if r < 0x7f {
		if r < 0x20 {
			return 0
		}
		return 1
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == '\u200D' {
		return 0
	}
	return lipgloss.Width(string(r))
}

// fmFitName truncates a file name to w columns, keeping its extension (and a
// little context before it) visible: "a-very-long-na…going.log" reads better
// than "a-very-long-name-that-k…" when scanning a listing.
func fmFitName(name string, w int) string {
	if w <= 0 {
		return ""
	}
	if fmWidth(name) <= w {
		return name
	}
	if strings.HasSuffix(name, "/") && len(name) > 1 {
		// Folders: keep the trailing slash, cut the end of the name.
		return fmFit(name[:len(name)-1], w-1) + "/"
	}
	if w < 10 {
		return fmFit(name, w)
	}
	ext := ""
	if dot := strings.LastIndexByte(name, '.'); dot > 0 && len(name)-dot <= 8 {
		ext = name[dot:]
	}
	if ext == "" {
		return fmFit(name, w)
	}
	tailW := fmWidth(ext) + 3
	if tailW > w/2 {
		tailW = w / 2
	}
	tail := fmSuffixWithin(name, tailW)
	head := fmPrefixWithin(name, w-1-fmWidth(tail))
	return head + "…" + tail
}

// fmPadRight pads (or truncates with "…") s to exactly w display columns.
func fmPadRight(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = fmFit(s, w)
	if gap := w - fmWidth(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// fmPadLeft right-aligns s in exactly w display columns.
func fmPadLeft(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = fmFit(s, w)
	if gap := w - fmWidth(s); gap > 0 {
		return strings.Repeat(" ", gap) + s
	}
	return s
}

// fmFitPathLeft keeps the END of a path visible ("…/deep/folder") when it does
// not fit, which is the part an operator actually needs.
func fmFitPathLeft(p string, w int) string {
	if w <= 0 {
		return ""
	}
	if fmWidth(p) <= w {
		return p
	}
	if w == 1 {
		return "…"
	}
	return "…" + fmSuffixWithin(p, w-1)
}

// fmWrap hard-wraps plain text to lines of at most w columns, breaking at
// spaces where possible. Used for error messages and long values in dialogs.
func fmWrap(s string, w int) []string {
	if w <= 0 {
		return nil
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		if para == "" {
			out = append(out, "")
			continue
		}
		line := ""
		for _, word := range strings.Split(para, " ") {
			for fmWidth(word) > w {
				// A single word longer than the line: split it.
				if line != "" {
					out = append(out, line)
					line = ""
				}
				head := fmPrefixWithin(word, w)
				if head == "" {
					head = word[:1]
				}
				out = append(out, head)
				word = word[len(head):]
			}
			switch {
			case line == "":
				line = word
			case fmWidth(line)+1+fmWidth(word) <= w:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
		}
		out = append(out, line)
	}
	return out
}

// fmExpandTabs replaces tabs with spaces to the next multiple of width.
func fmExpandTabs(s string, width int) string {
	if !strings.ContainsRune(s, '\t') {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		if r == '\t' {
			n := width - col%width
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteRune(r)
		col += fmRuneWidth(r)
	}
	return b.String()
}

// ============================================================================
// Fuzzy matching (quick jump)
// ============================================================================

// fmFuzzyScore scores how well query matches candidate (both already lower
// case) as an in-order subsequence. It returns ok=false when not every query
// rune appears in order. Higher is better: a contiguous prefix match beats a
// word-boundary match beats a scattered match, and shorter names win ties.
func fmFuzzyScore(query, candidate string) (int, bool) {
	if query == "" {
		return 0, true
	}
	score := 0
	qi := 0
	q := []rune(query)
	prev := rune(0)
	lastMatch := -2
	pos := 0
	for _, r := range candidate {
		if qi < len(q) && r == q[qi] {
			bonus := 1
			if pos == 0 {
				bonus += 12
			} else if fmWordBoundary(prev) {
				bonus += 8
			}
			if lastMatch == pos-1 {
				bonus += 6
			}
			score += bonus
			lastMatch = pos
			qi++
		}
		prev = r
		pos++
	}
	if qi < len(q) {
		return 0, false
	}
	// Exact substring bonus and a mild preference for shorter candidates.
	if strings.Contains(candidate, query) {
		score += 10
	}
	score -= pos / 8
	return score, true
}

func fmWordBoundary(r rune) bool {
	switch r {
	case '-', '_', '.', ' ', '/', '\\':
		return true
	}
	return false
}
