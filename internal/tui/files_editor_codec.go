// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textarea"
)

// The editor's textarea (bubbles v1.0.0) passes everything it is given through
// an input sanitizer that cannot be configured: SetValue turns a tab into four
// spaces and a CR into a newline (so CRLF becomes two), and drops other
// control characters, invalid UTF-8 and U+FFFD. It also keeps at most
// textareaMaxLines lines. Saving its Value() as-is would rewrite the file.
//
// editorCodec maps file text onto text the textarea keeps verbatim, and back:
//
//   - a tab is shown as tabGlyph (one cell) and saved as a tab;
//   - a file whose every line break is CRLF is edited with plain newlines and
//     saved with CRLF again, including lines typed into it.
//
// Nothing else the sanitizer touches has a faithful stand-in, so such a file
// opens read-only. newEditorArea does not rely on that list alone: it loads
// the encoded text and checks that decoding Value() returns the exact bytes,
// so an unforeseen loss also ends in read-only rather than in a save.
//
// Text typed or pasted in edit mode still goes through the sanitizer: a pasted
// tab becomes four spaces, as it always has. A pasted tabGlyph is saved as a
// tab.

// tabGlyph stands in for a tab inside the textarea. A file that already
// contains it opens read-only, since it would be saved back as a tab.
const tabGlyph = '⇥' // ⇥ RIGHTWARDS ARROW TO BAR

// textareaMaxLines mirrors the textarea's fixed line limit (its unexported
// maxLines); it is used only to explain a read-only open.
const textareaMaxLines = 10000

type editorCodec struct {
	crlf bool // the file's line breaks are all CRLF
}

// encode turns file text into textarea text.
func (c editorCodec) encode(raw string) string {
	s := raw
	if c.crlf {
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	return strings.ReplaceAll(s, "\t", string(tabGlyph))
}

// decode turns textarea text back into file text.
func (c editorCodec) decode(text string) string {
	s := strings.ReplaceAll(text, string(tabGlyph), "\t")
	if c.crlf {
		s = strings.ReplaceAll(s, "\n", "\r\n")
	}
	return s
}

// chooseEditorCodec picks the codec for raw, or says briefly why raw has no
// exact textarea form.
func chooseEditorCodec(raw string) (editorCodec, string) {
	if !utf8.ValidString(raw) {
		return editorCodec{}, "not valid UTF-8"
	}
	var c editorCodec
	if cr := strings.Count(raw, "\r"); cr > 0 {
		crlf := strings.Count(raw, "\r\n")
		switch {
		case crlf != cr:
			return editorCodec{}, "has a CR outside CRLF"
		case crlf != strings.Count(raw, "\n"):
			return editorCodec{}, "mixes CRLF and LF"
		}
		c.crlf = true
	}
	for _, r := range raw {
		switch {
		case r == '\t', r == '\n', r == '\r':
		case r == tabGlyph:
			return editorCodec{}, "contains " + string(tabGlyph)
		case r == utf8.RuneError:
			return editorCodec{}, "contains U+FFFD"
		case unicode.IsControl(r):
			return editorCodec{}, "has control characters"
		}
	}
	return c, ""
}

// newEditorArea returns a textarea holding raw and the codec that maps it
// back. When raw cannot round-trip exactly, why is non-empty, the textarea is
// empty, and the file must be shown read-only.
func newEditorArea(raw string) (ta textarea.Model, c editorCodec, why string) {
	ta = textarea.New()
	c, why = chooseEditorCodec(raw)
	if why != "" {
		return ta, editorCodec{}, why
	}
	ta.SetValue(c.encode(raw))
	if c.decode(ta.Value()) == raw {
		return ta, c, ""
	}
	why = "can't be held exactly"
	if n := strings.Count(raw, "\n") + 1; n > textareaMaxLines {
		why = fmt.Sprintf("over %d lines", textareaMaxLines)
	}
	ta.Reset()
	return ta, editorCodec{}, why
}

// readOnlyStatus is the footer message for a file opened read-only.
func readOnlyStatus(why, source string) string {
	if source != "" {
		return "read-only: " + why + " — use `fleet file edit` or $EDITOR"
	}
	return "read-only: " + why + " — edit it in $EDITOR"
}
