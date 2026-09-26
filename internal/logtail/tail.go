// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

// Package logtail reads the end of line-oriented log files without scanning
// them from the start, and reads forward from a remembered position.
//
// It reproduces bufio.Scanner/bufio.ScanLines line semantics exactly — lines
// split on '\n', one trailing '\r' dropped, a final unterminated line is still
// a line, and a line of MaxLineBytes or more is an error — so callers that used
// to scan the whole file get the same lines and the same absolute (1-based)
// line numbers. Line numbers are kept exact by counting the newlines in the
// skipped prefix with bytes.Count over large reads, which allocates nothing and
// is an order of magnitude cheaper than tokenizing every line.
//
// One deliberate difference: only the part of the file that is actually
// walked (the returned lines and whatever had to be examined to find them) is
// checked against MaxLineBytes; an over-long line far before the tail no
// longer makes the whole read fail.
package logtail

import (
	"bufio"
	"bytes"
	"io"
	"slices"
	"sync"
)

// MaxLineBytes is the longest line, excluding its '\n', that can be read. It
// matches the 1 MiB bufio.Scanner buffer the log readers always used: a line
// of MaxLineBytes bytes or more fails with ErrLineTooLong.
const MaxLineBytes = 1024 * 1024

// ErrLineTooLong is bufio.ErrTooLong so callers keep surfacing the same
// "bufio.Scanner: token too long" message as before.
var ErrLineTooLong = bufio.ErrTooLong

// chunkSize is the unit of IO. Big enough that a tail of a few hundred lines
// is usually one read, small enough to stay cache-resident while it is
// counted or searched. It is a variable only so tests can shrink it to
// exercise chunk boundaries.
var chunkSize = 256 << 10

// scratch holds the reusable buffers of one read.
type scratch struct {
	buf   []byte // IO buffer, chunkSize bytes
	lower []byte // ASCII-lowered copy of a region being searched
	spans []span
}

var scratchPool = sync.Pool{New: func() any { return new(scratch) }}

// getScratch returns pooled buffers; hand them back with putScratch.
func getScratch() *scratch {
	s := scratchPool.Get().(*scratch)
	if len(s.buf) != chunkSize {
		s.buf = make([]byte, chunkSize)
	}
	return s
}

// putScratch keeps the (possibly grown) helper buffers for reuse unless a
// giant line made them unusually large.
func putScratch(s *scratch, lower []byte, spans []span) {
	if cap(lower) <= 4*chunkSize {
		s.lower = lower[:0]
	}
	if cap(spans) <= chunkSize {
		s.spans = spans[:0]
	}
	scratchPool.Put(s)
}

// newline is the separator for bytes.Count, which counts a single byte with
// SIMD and without allocating.
var newline = []byte{'\n'}

// Line is one log line.
type Line struct {
	Number int    // absolute 1-based line number in the file
	Text   string // the line without '\n' and without one trailing '\r'
}

// Position is a line boundary in a file.
type Position struct {
	Offset int64 // byte offset just past a '\n' (or 0)
	Lines  int   // number of '\n' bytes in [0, Offset)
}

// TailResult is the outcome of Tail.
type TailResult struct {
	Lines      []Line   // the last matching lines, oldest first
	Truncated  bool     // more lines matched than were returned
	End        Position // just past the file's last '\n'
	TotalLines int      // lines in the file, including an unterminated last line
}

// ForwardLimits bounds the work of one Forward call. Zero means unlimited.
type ForwardLimits struct {
	MaxLines int   // stop before returning more than this many lines
	MaxBytes int   // stop once the returned text reaches this many bytes
	MaxScan  int64 // stop once this many bytes have been scanned
}

// ForwardResult is the outcome of Forward.
type ForwardResult struct {
	Lines []Line   // matching lines after the start position, oldest first
	Next  Position // where the next Forward call should resume
	More  bool     // stopped at a limit before reaching the end of the file
}

// span locates one line inside a region of whole lines.
type span struct {
	start, end int // text bytes: region[start:end]
	idx        int // 0-based line index within the region
}

// matchRegion appends to spans the lines of region matched by m (all lines
// when m is nil) and returns the number of lines in region. region must hold
// whole lines: each ends in '\n', except that the final one may be
// unterminated when region runs to the end of the file.
func matchRegion(region []byte, m *Matcher, lower *[]byte, spans []span) ([]span, int, error) {
	lines := bytes.Count(region, newline)
	if len(region) > 0 && region[len(region)-1] != '\n' {
		lines++
	}
	// A region shorter than MaxLineBytes cannot contain an over-long line, so
	// it can be searched as one block instead of line by line.
	if m != nil && len(region) < MaxLineBytes {
		if m.never {
			return spans, lines, nil
		}
		hay := region
		ok := true
		if !m.literal {
			*lower, ok = lowerASCIIBlock(*lower, region)
			hay = *lower
		}
		if ok {
			return matchBlock(region, hay, m.needle, spans), lines, nil
		}
	}
	idx := 0
	for off := 0; off < len(region); idx++ {
		le, next := len(region), len(region)
		if nl := bytes.IndexByte(region[off:], '\n'); nl >= 0 {
			le = off + nl
			next = le + 1
		}
		if le-off >= MaxLineBytes {
			return spans, 0, ErrLineTooLong
		}
		end := le
		if end > off && region[end-1] == '\r' {
			end--
		}
		if m.Match(region[off:end]) {
			spans = append(spans, span{start: off, end: end, idx: idx})
		}
		off = next
	}
	return spans, lines, nil
}

// matchBlock finds needle in hay (region itself, or its ASCII-lowered copy
// with identical offsets) and maps each hit to its line. needle never contains
// '\n' (so a hit never spans lines) and never ends in '\r' (it was trimmed), so
// a hit is always inside the line's text after the trailing '\r' is dropped.
func matchBlock(region, hay, needle []byte, spans []span) []span {
	pos, counted, idx := 0, 0, 0
	for pos < len(hay) {
		j := bytes.Index(hay[pos:], needle)
		if j < 0 {
			break
		}
		p := pos + j
		ls := pos + bytes.LastIndexByte(region[pos:p], '\n') + 1
		idx += bytes.Count(region[counted:ls], newline)
		counted = ls
		le, next := len(region), len(region)
		if nl := bytes.IndexByte(region[p:], '\n'); nl >= 0 {
			le = p + nl
			next = le + 1
		}
		end := le
		if end > ls && region[end-1] == '\r' {
			end--
		}
		spans = append(spans, span{start: ls, end: end, idx: idx})
		pos = next
	}
	return spans
}

// readFull reads exactly len(p) bytes at off.
func readFull(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// countNewlines counts '\n' bytes in [start, end) using buf for IO.
func countNewlines(r io.ReaderAt, buf []byte, start, end int64) (int, error) {
	n := 0
	for start < end {
		k := min(int64(chunkSize), int64(len(buf)), end-start)
		if err := readFull(r, buf[:k], start); err != nil {
			return n, err
		}
		n += bytes.Count(buf[:k], newline)
		start += k
	}
	return n, nil
}

// CountLines returns the number of lines a line scanner would produce for the
// first size bytes of r: its '\n' count plus one for an unterminated last line.
func CountLines(r io.ReaderAt, size int64) (int, error) {
	if size <= 0 {
		return 0, nil
	}
	s := getScratch()
	defer scratchPool.Put(s)
	buf := s.buf
	n, err := countNewlines(r, buf, 0, size)
	if err != nil {
		return 0, err
	}
	if err := readFull(r, buf[:1], size-1); err != nil {
		return 0, err
	}
	if buf[0] != '\n' {
		n++
	}
	return n, nil
}

type keptLine struct {
	text    string
	fromEnd int // 1 for the file's last line
}

// Tail returns the last n lines of the first size bytes of r that m matches
// (every line when m is nil), with their absolute line numbers, reading the
// file backwards from the end in chunks and stopping as soon as it has n+1
// matches (the extra one only decides Truncated). The newlines of the skipped
// prefix are then counted to number the lines.
func Tail(r io.ReaderAt, size int64, n int, m *Matcher) (TailResult, error) {
	return tail(r, size, n, m, -1)
}

// TailKnown is Tail for content whose line count is already known (the
// TotalLines of an earlier Tail, or CountLines, over the same unchanged
// bytes): it skips counting the prefix, so it reads only the tail.
func TailKnown(r io.ReaderAt, size int64, n int, m *Matcher, totalLines int) (TailResult, error) {
	return tail(r, size, n, m, max(totalLines, 0))
}

func tail(r io.ReaderAt, size int64, n int, m *Matcher, knownLines int) (TailResult, error) {
	var res TailResult
	if size <= 0 {
		return res, nil
	}
	n = max(n, 0)
	sc := getScratch()
	var (
		buf        = sc.buf
		pos        = size // file offset of the earliest byte read
		boundary   = size // file offset of the earliest processed line start
		carry      int    // file[pos, boundary): the tail of a line whose start is not read yet, kept at the end of buf
		endOffset  = int64(-1)
		linesAfter int // lines at or after boundary
		matched    int
		kept       []keptLine
		spans      = sc.spans
		lower      = sc.lower
	)
	defer func() { putScratch(sc, lower, spans) }()
	for boundary > 0 && matched <= n {
		if carry > 0 {
			raw := carry
			if buf[len(buf)-1] == '\n' {
				raw--
			}
			if raw >= MaxLineBytes {
				return TailResult{}, ErrLineTooLong
			}
		}
		if room := len(buf) - carry; room == 0 || room < len(buf)/2 {
			// A very long line has taken over the buffer.
			grown := make([]byte, 2*len(buf))
			copy(grown[len(grown)-carry:], buf[len(buf)-carry:])
			buf = grown
		}
		toRead := min(int64(len(buf)-carry), int64(chunkSize), pos)
		dataStart := len(buf) - carry - int(toRead)
		if err := readFull(r, buf[dataStart:len(buf)-carry], pos-toRead); err != nil {
			return TailResult{}, err
		}
		pos -= toRead
		data := buf[dataStart:] // file[pos, boundary)
		regionOff := 0
		if pos > 0 {
			// The bytes up to the first '\n' belong to a line that starts
			// before pos; everything after it is whole lines.
			if i := bytes.IndexByte(data, '\n'); i >= 0 {
				regionOff = i + 1
			} else {
				regionOff = len(data)
			}
		}
		if region := data[regionOff:]; len(region) > 0 {
			var lines int
			var err error
			spans, lines, err = matchRegion(region, m, &lower, spans[:0])
			if err != nil {
				return TailResult{}, err
			}
			if endOffset < 0 {
				// The first region processed holds the file's last line.
				endOffset = size
				if region[len(region)-1] != '\n' {
					endOffset = pos + int64(regionOff) + int64(bytes.LastIndexByte(region, '\n')+1)
				}
			}
			for k := len(spans) - 1; k >= 0 && len(kept) < n; k-- {
				s := spans[k]
				kept = append(kept, keptLine{text: string(region[s.start:s.end]), fromEnd: linesAfter + lines - s.idx})
			}
			matched += len(spans)
			linesAfter += lines
		}
		carry = regionOff
		copy(buf[len(buf)-carry:], data[:regionOff])
		boundary = pos + int64(regionOff)
	}

	res.TotalLines = linesAfter
	switch {
	case boundary == 0:
	case knownLines > linesAfter:
		// A non-empty prefix holds at least one line, so a smaller known
		// count cannot describe this content and is ignored.
		res.TotalLines = knownLines
	default:
		prefix, err := countNewlines(r, buf, 0, boundary)
		if err != nil {
			return TailResult{}, err
		}
		res.TotalLines += prefix
	}
	res.End = Position{Offset: endOffset, Lines: res.TotalLines}
	if endOffset < size {
		res.End.Lines-- // the last line is unterminated
	}
	res.Truncated = matched > n
	res.Lines = make([]Line, len(kept))
	for i, k := range kept {
		res.Lines[len(kept)-1-i] = Line{Number: res.TotalLines - k.fromEnd + 1, Text: k.text}
	}
	return res, nil
}

// Forward returns the lines m matches (every line when m is nil) between
// from and the first size bytes of r. from must be a line boundary (Offset 0
// or just past a '\n', with Lines the newline count before it). Next advances
// only over complete lines: an unterminated last line is returned (when it
// matches) but not consumed, so it is read again, complete, next time.
func Forward(r io.ReaderAt, from Position, size int64, m *Matcher, lim ForwardLimits) (ForwardResult, error) {
	res := ForwardResult{Next: from}
	if from.Offset >= size {
		return res, nil
	}
	sc := getScratch()
	var (
		buf      = sc.buf
		off      = from.Offset // next byte to read
		carry    int           // buf[:carry] = file[res.Next.Offset, off), an incomplete line
		returned int
		spans    = sc.spans
		lower    = sc.lower
	)
	defer func() { putScratch(sc, lower, spans) }()
	full := func() bool {
		return (lim.MaxLines > 0 && len(res.Lines) >= lim.MaxLines) || (lim.MaxBytes > 0 && returned >= lim.MaxBytes)
	}
	for off < size {
		if carry == len(buf) {
			buf = slices.Grow(buf[:carry], carry)[:2*carry]
		}
		k := min(int64(chunkSize), int64(len(buf)-carry), size-off)
		if err := readFull(r, buf[carry:carry+int(k)], off); err != nil {
			return ForwardResult{}, err
		}
		off += k
		data := buf[:carry+int(k)]
		last := bytes.LastIndexByte(data, '\n')
		if last < 0 {
			carry = len(data)
			if carry >= MaxLineBytes {
				return ForwardResult{}, ErrLineTooLong
			}
			continue
		}
		region := data[:last+1]
		var lines int
		var err error
		spans, lines, err = matchRegion(region, m, &lower, spans[:0])
		if err != nil {
			return ForwardResult{}, err
		}
		for _, s := range spans {
			if full() {
				res.Next = Position{Offset: res.Next.Offset + int64(s.start), Lines: res.Next.Lines + s.idx}
				res.More = true
				return res, nil
			}
			res.Lines = append(res.Lines, Line{Number: res.Next.Lines + s.idx + 1, Text: string(region[s.start:s.end])})
			returned += s.end - s.start
		}
		res.Next = Position{Offset: res.Next.Offset + int64(len(region)), Lines: res.Next.Lines + lines}
		carry = copy(buf, data[last+1:])
		if lim.MaxScan > 0 && res.Next.Offset-from.Offset >= lim.MaxScan && res.Next.Offset < size {
			res.More = true
			return res, nil
		}
	}
	if carry > 0 {
		// An unterminated last line: report it, but leave Next before it.
		if carry >= MaxLineBytes {
			return ForwardResult{}, ErrLineTooLong
		}
		line := buf[:carry]
		if line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if m.Match(line) {
			if full() {
				res.More = true
			} else {
				res.Lines = append(res.Lines, Line{Number: res.Next.Lines + 1, Text: string(line)})
			}
		}
	}
	return res, nil
}
