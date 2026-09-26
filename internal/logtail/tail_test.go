// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package logtail

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"unicode"
)

// refLine / refTail are the historical implementation (a full forward
// bufio.Scanner pass with a ring of the last n matches) that Tail must
// reproduce exactly.
type refResult struct {
	lines     []Line
	truncated bool
	total     int
	err       error
}

func refTail(data []byte, n int, search string) refResult {
	search = strings.ToLower(strings.TrimSpace(search))
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var all []Line
	number := 0
	for scanner.Scan() {
		number++
		line := scanner.Text()
		if search != "" && !strings.Contains(strings.ToLower(line), search) {
			continue
		}
		all = append(all, Line{Number: number, Text: line})
	}
	if err := scanner.Err(); err != nil {
		return refResult{err: err}
	}
	res := refResult{total: number}
	if len(all) > n {
		res.truncated = true
		all = all[len(all)-n:]
	}
	res.lines = all
	return res
}

// withChunkSize runs fn with a tiny IO chunk so chunk boundaries fall inside
// lines, inside "\r\n" pairs and inside multi-byte runes.
func withChunkSize(t *testing.T, size int, fn func()) {
	t.Helper()
	saved := chunkSize
	chunkSize = size
	defer func() { chunkSize = saved }()
	fn()
}

var fragments = []string{
	"INFO request ok", "ERROR disk full", "error: retry", "Warn slow", "", " ", "\r",
	"x", "GET /api/v1/items?id=42 200", "İstanbul", "Kelvin", "kelvin", "ǅungla",
	"\xff\xfe broken utf8", "� replacement", "tab\there", "Straße", "ΣΊΣΥΦΟΣ",
	"mixed ErRoR case", "trailing space ", "cr\rinside", "中文日志 错误",
}

func randomLog(rng *rand.Rand, lines int) []byte {
	var b bytes.Buffer
	for i := 0; i < lines; i++ {
		switch rng.IntN(10) {
		case 0:
			b.WriteString(strings.Repeat("a", rng.IntN(300)))
		case 1:
			b.WriteString(fragments[rng.IntN(len(fragments))] + fragments[rng.IntN(len(fragments))])
		default:
			b.WriteString(fragments[rng.IntN(len(fragments))])
		}
		switch rng.IntN(8) {
		case 0:
			b.WriteString("\r\n")
		default:
			b.WriteByte('\n')
		}
	}
	data := b.Bytes()
	switch rng.IntN(4) {
	case 0: // unterminated last line
		data = append(data, []byte(fragments[rng.IntN(len(fragments))]+"tail")...)
	case 1: // last line is a lone "\r"
		data = append(data, '\r')
	}
	return data
}

var searches = []string{"", "error", "ERROR", "  error  ", "e", "kelvin", "istanbul", "i̇stanbul",
	"straße", "σίσυφος", "200", "/api", " ", "�", "tab\th", "cr\ri", "x\ny", "错误", "zzz"}

func compareTail(t *testing.T, data []byte, n int, search string) {
	t.Helper()
	want := refTail(data, n, search)
	got, err := Tail(bytes.NewReader(data), int64(len(data)), n, NewMatcher(search))
	if want.err != nil {
		if !errors.Is(err, ErrLineTooLong) {
			t.Fatalf("n=%d search=%q: want error %v, got %v", n, search, want.err, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("n=%d search=%q: unexpected error %v", n, search, err)
	}
	if got.Truncated != want.truncated || got.TotalLines != want.total || !equalLines(got.Lines, want.lines) {
		t.Fatalf("n=%d search=%q mismatch\n got: trunc=%v total=%d %v\nwant: trunc=%v total=%d %v\ndata=%q",
			n, search, got.Truncated, got.TotalLines, got.Lines, want.truncated, want.total, want.lines, data)
	}
	// End is just past the last '\n' and counts the newlines before it.
	end := int64(bytes.LastIndexByte(data, '\n') + 1)
	if got.End.Offset != end || got.End.Lines != bytes.Count(data, []byte{'\n'}) {
		t.Fatalf("End = %+v, want offset %d lines %d", got.End, end, bytes.Count(data, []byte{'\n'}))
	}
	// With the line count supplied, the prefix is not counted but every
	// result is the same.
	known, err := TailKnown(bytes.NewReader(data), int64(len(data)), n, NewMatcher(search), want.total)
	if err != nil || !reflect.DeepEqual(known, got) {
		t.Fatalf("TailKnown = %+v, %v; want %+v", known, err, got)
	}
}

func equalLines(a, b []Line) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTailMatchesScannerOnFixedCases(t *testing.T) {
	cases := []string{
		"", "\n", "\n\n", "a", "a\n", "a\r", "a\r\n", "\r\n", "\r", "a\nb", "a\nb\n", "a\r\r\n", "\n\na\n\n",
		"one\ntwo\r\nthree", "ERROR\nerror\nErRoR\n", "İ\nK\n\xff\n",
	}
	for _, size := range []int{1, 2, 3, 5, 256 << 10} {
		withChunkSize(t, size, func() {
			for _, data := range cases {
				for _, n := range []int{0, 1, 2, 3, 200} {
					for _, search := range searches {
						compareTail(t, []byte(data), n, search)
					}
				}
			}
		})
	}
}

func TestTailMatchesScannerOnRandomLogs(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for iter := 0; iter < 300; iter++ {
		data := randomLog(rng, rng.IntN(400))
		size := []int{1, 3, 7, 64, 1000, 256 << 10}[rng.IntN(6)]
		withChunkSize(t, size, func() {
			for _, n := range []int{0, 1, 5, 50, 1000} {
				compareTail(t, data, n, searches[rng.IntN(len(searches))])
			}
		})
	}
}

func TestTailLongLines(t *testing.T) {
	long := func(n int) string { return strings.Repeat("L", n) }
	okLine := long(MaxLineBytes - 1)
	badLine := long(MaxLineBytes)

	cases := []struct {
		name    string
		data    string
		n       int
		wantErr bool
	}{
		{"max-1 terminated", "a\n" + okLine + "\nb\n", 5, false},
		{"max-1 unterminated", "a\n" + okLine, 5, false},
		{"max-1 with cr", "a\n" + long(MaxLineBytes-2) + "\r\n", 5, false},
		{"max terminated in window", "a\n" + badLine + "\nb\n", 5, true},
		{"max unterminated", "a\n" + badLine, 5, true},
		{"max with cr counts the cr", "a\n" + okLine + "\r\nb\n", 5, true},
		{"huge in window", "a\n" + long(3*MaxLineBytes) + "\nb\n", 5, true},
	}
	for _, tc := range cases {
		for _, size := range []int{4096, 256 << 10} {
			withChunkSize(t, size, func() {
				_, err := Tail(strings.NewReader(tc.data), int64(len(tc.data)), tc.n, nil)
				if tc.wantErr != (err != nil) {
					t.Fatalf("%s (chunk %d): err = %v, wantErr %v", tc.name, size, err, tc.wantErr)
				}
				if err != nil && !errors.Is(err, ErrLineTooLong) {
					t.Fatalf("%s: unexpected error %v", tc.name, err)
				}
				if !tc.wantErr {
					compareTail(t, []byte(tc.data), tc.n, "")
					compareTail(t, []byte(tc.data), tc.n, "l")
				}
			})
		}
	}

	// An over-long line far before the requested tail is skipped, not fatal,
	// and the tail still carries exact absolute numbers.
	data := "first\n" + badLine + "\n" + "x\ny\nz\n"
	got, err := Tail(strings.NewReader(data), int64(len(data)), 2, nil)
	if err != nil {
		t.Fatalf("long line outside the window must not fail the tail: %v", err)
	}
	if !equalLines(got.Lines, []Line{{4, "y"}, {5, "z"}}) || !got.Truncated {
		t.Fatalf("unexpected tail %+v", got)
	}
}

// forwardAll pages through data with Forward from `from` until it stops
// making progress, the way a follow loop would.
func forwardAll(t *testing.T, data []byte, from Position, m *Matcher, lim ForwardLimits) ([]Line, Position) {
	t.Helper()
	var out []Line
	for i := 0; ; i++ {
		if i > 100000 {
			t.Fatal("Forward did not converge")
		}
		res, err := Forward(bytes.NewReader(data), from, int64(len(data)), m, lim)
		if err != nil {
			t.Fatalf("Forward: %v", err)
		}
		if !res.More {
			out = append(out, res.Lines...)
			return out, res.Next
		}
		if res.Next.Offset <= from.Offset && len(res.Lines) == 0 {
			t.Fatalf("Forward made no progress at %+v", from)
		}
		out = append(out, res.Lines...)
		from = res.Next
	}
}

func TestForwardMatchesScanner(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for iter := 0; iter < 300; iter++ {
		data := randomLog(rng, rng.IntN(300))
		search := searches[rng.IntN(len(searches))]
		size := []int{1, 3, 7, 64, 1000, 256 << 10}[rng.IntN(6)]
		lim := []ForwardLimits{{}, {MaxLines: 1}, {MaxLines: 7}, {MaxBytes: 10}, {MaxScan: 5}, {MaxLines: 3, MaxBytes: 40, MaxScan: 100}}[rng.IntN(6)]
		withChunkSize(t, size, func() {
			want := refTail(data, 1<<30, search)
			// Start at a random line boundary.
			starts := []int{0}
			for i, c := range data {
				if c == '\n' {
					starts = append(starts, i+1)
				}
			}
			start := starts[rng.IntN(len(starts))]
			from := Position{Offset: int64(start), Lines: bytes.Count(data[:start], []byte{'\n'})}
			got, next := forwardAll(t, data, from, NewMatcher(search), lim)
			var expect []Line
			for _, l := range want.lines {
				if l.Number > from.Lines {
					expect = append(expect, l)
				}
			}
			if !equalLines(got, expect) {
				t.Fatalf("search=%q from=%+v lim=%+v chunk=%d\n got %v\nwant %v\ndata=%q", search, from, lim, size, got, expect, data)
			}
			end := int64(bytes.LastIndexByte(data, '\n') + 1)
			if end < from.Offset {
				end = from.Offset
			}
			if next.Offset != end || next.Lines != bytes.Count(data[:end], []byte{'\n'}) {
				t.Fatalf("next = %+v, want offset %d", next, end)
			}
		})
	}
}

func TestForwardHoldsBackUnterminatedLine(t *testing.T) {
	data := []byte("a\nb\npart")
	res, err := Forward(bytes.NewReader(data), Position{}, int64(len(data)), nil, ForwardLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if !equalLines(res.Lines, []Line{{1, "a"}, {2, "b"}, {3, "part"}}) {
		t.Fatalf("lines = %v", res.Lines)
	}
	if res.Next != (Position{Offset: 4, Lines: 2}) || res.More {
		t.Fatalf("next = %+v more=%v; the unterminated line must not be consumed", res.Next, res.More)
	}
	data = append(data, []byte("ial\nc\n")...)
	res, err = Forward(bytes.NewReader(data), res.Next, int64(len(data)), nil, ForwardLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if !equalLines(res.Lines, []Line{{3, "partial"}, {4, "c"}}) || res.Next.Offset != int64(len(data)) || res.Next.Lines != 4 {
		t.Fatalf("second read = %+v", res)
	}
}

func TestForwardLongLine(t *testing.T) {
	data := []byte("ok\n" + strings.Repeat("x", MaxLineBytes) + "\n")
	if _, err := Forward(bytes.NewReader(data), Position{}, int64(len(data)), nil, ForwardLimits{}); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
	data = []byte("ok\n" + strings.Repeat("x", MaxLineBytes-1) + "\n")
	res, err := Forward(bytes.NewReader(data), Position{}, int64(len(data)), NewMatcher("xx"), ForwardLimits{})
	if err != nil || len(res.Lines) != 1 || res.Lines[0].Number != 2 {
		t.Fatalf("max-1 line: res=%d lines err=%v", len(res.Lines), err)
	}
}

// TestShrunkFileReportsUnexpectedEOF: a file that is shorter than the size it
// was stat'ed at (truncated mid-read) surfaces io.ErrUnexpectedEOF, which the
// agent retries at the new size.
func TestShrunkFileReportsUnexpectedEOF(t *testing.T) {
	r := strings.NewReader("abc\ndef\n")
	if _, err := Tail(r, 1<<20, 5, nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Tail err = %v", err)
	}
	if _, err := Forward(r, Position{}, 1<<20, nil, ForwardLimits{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Forward err = %v", err)
	}
	if _, err := CountLines(r, 1<<20); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("CountLines err = %v", err)
	}
}

func TestCountLines(t *testing.T) {
	for _, data := range []string{"", "a", "a\n", "\n", "a\nb", "a\nb\n", "\r"} {
		want := refTail([]byte(data), 0, "").total
		got, err := CountLines(strings.NewReader(data), int64(len(data)))
		if err != nil || got != want {
			t.Fatalf("CountLines(%q) = %d, %v; want %d", data, got, err, want)
		}
	}
}

func TestMatcherEqualsToLowerContains(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	alphabet := []string{"a", "A", "z", "Z", "@", "[", "`", "{", "0", " ", "\r", "\t", "İ", "i", "K", "k", "K",
		"ß", "ẞ", "Σ", "σ", "ς", "\xff", "\xc3", "�", "Ⱥ", "ⱥ", "ǅ", "ǆ", "é", "É", "-", "\x7f"}
	gen := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		return b.String()
	}
	for i := 0; i < 20000; i++ {
		line := gen(rng.IntN(24))
		search := gen(1 + rng.IntN(3))
		if rng.IntN(4) == 0 && len(line) > 2 {
			search = line[1 : 1+rng.IntN(len(line)-1)] // a real substring, maybe mid-rune
		}
		want := strings.Contains(strings.ToLower(line), strings.ToLower(strings.TrimSpace(search)))
		if strings.TrimSpace(search) == "" {
			want = true
		}
		if got := NewMatcher(search).Match([]byte(line)); got != want {
			t.Fatalf("Match(%q, %q) = %v, want %v", line, search, got, want)
		}
		// appendLower must be byte-identical to strings.ToLower.
		if got := string(appendLower(nil, []byte(line))); got != strings.ToLower(line) {
			t.Fatalf("appendLower(%q) = %q, want %q", line, got, strings.ToLower(line))
		}
	}
}

// TestLiteralNeedleInvariant guards the Matcher.literal shortcut: lowering
// must never turn a non-ASCII rune into an ASCII non-letter.
func TestLiteralNeedleInvariant(t *testing.T) {
	for r := rune(0x80); r <= unicode.MaxRune; r++ {
		l := unicode.ToLower(r)
		if l < 0x80 && !(('a' <= l && l <= 'z') || ('A' <= l && l <= 'Z')) {
			t.Fatalf("unicode.ToLower(%U) = %q is an ASCII non-letter", r, l)
		}
	}
}

func TestLowerASCII8(t *testing.T) {
	for c := 0; c < 0x80; c++ {
		x := uint64(c) * loBits
		want := byte(c)
		if 'A' <= c && c <= 'Z' {
			want += 'a' - 'A'
		}
		if got := lowerASCII8(x); got != uint64(want)*loBits {
			t.Fatalf("lowerASCII8(%#x) = %#x", c, got)
		}
	}
}

func ExampleTail() {
	data := "one\ntwo\nthree\nfour\n"
	res, _ := Tail(strings.NewReader(data), int64(len(data)), 2, nil)
	for _, l := range res.Lines {
		fmt.Println(l.Number, l.Text)
	}
	fmt.Println(res.Truncated, res.End.Offset, res.End.Lines)
	// Output:
	// 3 three
	// 4 four
	// true 19 4
}
