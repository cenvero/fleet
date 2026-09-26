// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package logtail

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Matcher is a case-insensitive substring filter over raw line bytes. It gives
// exactly the same answer as the historical
//
//	strings.Contains(strings.ToLower(line), strings.ToLower(strings.TrimSpace(search)))
//
// but without allocating: lines are lowered into a reusable buffer (8 bytes at
// a time while they are ASCII), and needles that case folding cannot affect are
// matched against the raw bytes directly. A Matcher is not safe for concurrent
// use. A nil *Matcher matches every line.
type Matcher struct {
	needle []byte
	// never: the needle contains '\n', which no scanned line (split on '\n')
	// can contain, so nothing matches.
	never bool
	// literal: the needle is ASCII without letters. Lowering only rewrites
	// letters (and turns invalid UTF-8 into U+FFFD, which is non-ASCII), and
	// never produces an ASCII non-letter from anything else, so matching the
	// raw bytes is exact.
	literal bool
	lower   []byte
}

// NewMatcher returns a matcher for search, or nil when search is blank (which
// matches everything).
func NewMatcher(search string) *Matcher {
	needle := strings.ToLower(strings.TrimSpace(search))
	if needle == "" {
		return nil
	}
	m := &Matcher{needle: []byte(needle), literal: true}
	m.never = strings.IndexByte(needle, '\n') >= 0
	for i := 0; i < len(needle); i++ {
		c := needle[i]
		if c >= utf8.RuneSelf || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') {
			m.literal = false
			break
		}
	}
	return m
}

// Match reports whether line (without its terminator) contains the needle,
// ignoring case exactly as strings.ToLower does.
func (m *Matcher) Match(line []byte) bool {
	if m == nil {
		return true
	}
	if m.never {
		return false
	}
	if m.literal {
		return bytes.Contains(line, m.needle)
	}
	m.lower = appendLower(m.lower[:0], line)
	return bytes.Contains(m.lower, m.needle)
}

const (
	loBits = 0x0101010101010101
	hiBits = 0x8080808080808080
)

// lowerASCII8 lowercases eight packed ASCII bytes (every byte < 0x80).
func lowerASCII8(x uint64) uint64 {
	// Adding 0x3F sets bit 7 of every byte >= 'A'; adding 0x25 sets it for
	// every byte > 'Z'. Neither addition can carry into the next byte because
	// each byte is < 0x80.
	ge := x + (0x80-'A')*loBits
	gt := x + (0x80-'Z'-1)*loBits
	upper := ge &^ gt & hiBits
	return x | upper>>2 // 0x80>>2 == 0x20, the ASCII case bit
}

// appendLower appends the lowercase form of src to dst, byte-for-byte identical
// to strings.ToLower(string(src)): ASCII is lowered directly, other runes go
// through unicode.ToLower and invalid UTF-8 bytes become U+FFFD.
func appendLower(dst, src []byte) []byte {
	i := 0
	for i < len(src) {
		if i+8 <= len(src) {
			x := binary.LittleEndian.Uint64(src[i:])
			if x&hiBits == 0 {
				dst = binary.LittleEndian.AppendUint64(dst, lowerASCII8(x))
				i += 8
				continue
			}
		}
		c := src[i]
		if c < utf8.RuneSelf {
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			dst = append(dst, c)
			i++
			continue
		}
		r, w := utf8.DecodeRune(src[i:])
		dst = utf8.AppendRune(dst, unicode.ToLower(r))
		i += w
	}
	return dst
}

// lowerASCIIBlock lowers src into dst when src is entirely ASCII, in which
// case offsets in the result line up with offsets in src. It returns false
// (and a garbage dst) as soon as it meets a non-ASCII byte.
func lowerASCIIBlock(dst, src []byte) ([]byte, bool) {
	dst = dst[:0]
	i := 0
	for ; i+8 <= len(src); i += 8 {
		x := binary.LittleEndian.Uint64(src[i:])
		if x&hiBits != 0 {
			return dst, false
		}
		dst = binary.LittleEndian.AppendUint64(dst, lowerASCII8(x))
	}
	for ; i < len(src); i++ {
		c := src[i]
		if c >= utf8.RuneSelf {
			return dst, false
		}
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst = append(dst, c)
	}
	return dst, true
}
