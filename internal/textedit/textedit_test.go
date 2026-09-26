// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package textedit

import (
	"errors"
	"strings"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

func replaceOp(old, repl string, all bool) proto.FileEditOp {
	return proto.FileEditOp{Kind: proto.FileEditOpReplace, Old: old, New: repl, All: all}
}

func insertOp(line int, text string) proto.FileEditOp {
	return proto.FileEditOp{Kind: proto.FileEditOpInsert, Line: line, Text: text}
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not a *textedit.Error", err)
	}
	return e.Code
}

func TestApplyReplaceUnique(t *testing.T) {
	t.Parallel()
	in := []byte("listen 80;\nserver_name a;\n")
	out, n, err := Apply(in, []proto.FileEditOp{replaceOp("listen 80;", "listen 8080;", false)}, 0)
	if err != nil || n != 1 {
		t.Fatalf("Apply = %d, %v", n, err)
	}
	if string(out) != "listen 8080;\nserver_name a;\n" {
		t.Fatalf("out = %q", out)
	}
	if string(in) != "listen 80;\nserver_name a;\n" {
		t.Fatal("Apply modified its input")
	}
}

func TestApplyReplaceRefusesAmbiguousAndMissing(t *testing.T) {
	t.Parallel()
	in := []byte("a=1\nb=1\n")
	if _, _, err := Apply(in, []proto.FileEditOp{replaceOp("=1", "=2", false)}, 0); codeOf(t, err) != CodeNotUnique {
		t.Fatalf("ambiguous replace: %v", err)
	}
	if _, _, err := Apply(in, []proto.FileEditOp{replaceOp("c=1", "c=2", false)}, 0); codeOf(t, err) != CodeNotFound {
		t.Fatalf("missing replace: %v", err)
	}
	out, n, err := Apply(in, []proto.FileEditOp{replaceOp("=1", "=2", true)}, 0)
	if err != nil || n != 2 || string(out) != "a=2\nb=2\n" {
		t.Fatalf("replace all = %q, %d, %v", out, n, err)
	}
}

func TestApplyNotFoundHintsAtWhitespace(t *testing.T) {
	t.Parallel()
	in := []byte("server {\n    listen 80;\n}\n")
	_, _, err := Apply(in, []proto.FileEditOp{replaceOp("server {\nlisten 80;", "x", false)}, 0)
	if codeOf(t, err) != CodeNotFound || !strings.Contains(err.Error(), "indentation") {
		t.Fatalf("expected an indentation hint, got %v", err)
	}
}

func TestApplyIsAllOrNothing(t *testing.T) {
	t.Parallel()
	in := []byte("one\ntwo\n")
	_, _, err := Apply(in, []proto.FileEditOp{replaceOp("one", "1", false), replaceOp("three", "3", false)}, 0)
	if codeOf(t, err) != CodeNotFound || !strings.HasPrefix(err.Error(), "edit 2 of 2:") {
		t.Fatalf("second op should fail and be named: %v", err)
	}
}

func TestApplyOpsSeeEarlierOps(t *testing.T) {
	t.Parallel()
	out, n, err := Apply([]byte("a\n"), []proto.FileEditOp{replaceOp("a", "b", false), replaceOp("b", "c", false)}, 0)
	if err != nil || n != 2 || string(out) != "c\n" {
		t.Fatalf("chained ops = %q, %d, %v", out, n, err)
	}
}

func TestApplyKeepsCRLF(t *testing.T) {
	t.Parallel()
	in := []byte("a\r\nb\r\nc\r\n")
	out, _, err := Apply(in, []proto.FileEditOp{replaceOp("a\nb", "x\ny", false), insertOp(3, "d")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "x\r\ny\r\nc\r\nd\r\n" {
		t.Fatalf("out = %q", out)
	}
}

func TestApplyInsert(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		line int
		text string
		want string
	}{
		{"a\nb\n", 0, "top", "top\na\nb\n"},
		{"a\nb\n", 1, "mid\n", "a\nmid\nb\n"},
		{"a\nb\n", 2, "end", "a\nb\nend\n"},
		{"a\nb", 2, "end", "a\nb\nend"},
		{"", 0, "first", "first\n"},
		{"a\n", 1, "x\ny", "a\nx\ny\n"},
	}
	for _, c := range cases {
		out, n, err := Apply([]byte(c.in), []proto.FileEditOp{insertOp(c.line, c.text)}, 0)
		if err != nil || n != 1 || string(out) != c.want {
			t.Errorf("insert %q after %d into %q = %q, %v; want %q", c.text, c.line, c.in, out, err, c.want)
		}
	}
	if _, _, err := Apply([]byte("a\n"), []proto.FileEditOp{insertOp(5, "x")}, 0); codeOf(t, err) != CodeInvalidEdit {
		t.Fatalf("out-of-range insert: %v", err)
	}
}

func TestApplyRefusesBadInput(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in   string
		ops  []proto.FileEditOp
		code string
	}{
		"no ops":       {"a", nil, CodeInvalidEdit},
		"empty old":    {"a", []proto.FileEditOp{replaceOp("", "b", false)}, CodeInvalidEdit},
		"no-op":        {"a", []proto.FileEditOp{replaceOp("a", "a", false)}, CodeInvalidEdit},
		"unknown kind": {"a", []proto.FileEditOp{{Kind: "delete"}}, CodeInvalidEdit},
		"empty insert": {"a", []proto.FileEditOp{insertOp(0, "")}, CodeInvalidEdit},
		"binary":       {"a\x00b", []proto.FileEditOp{replaceOp("a", "b", false)}, CodeBinaryFile},
	} {
		if _, _, err := Apply([]byte(tc.in), tc.ops, 0); err == nil || codeOf(t, err) != tc.code {
			t.Errorf("%s: err = %v, want code %s", name, err, tc.code)
		}
	}
}

func TestApplyHonoursMaxBytes(t *testing.T) {
	t.Parallel()
	_, _, err := Apply([]byte("a\n"), []proto.FileEditOp{replaceOp("a", strings.Repeat("x", 100), false)}, 50)
	if codeOf(t, err) != CodeFileTooLarge {
		t.Fatalf("oversized result: %v", err)
	}
}

func TestApplyRefusesHugeReplaceAllBeforeAllocating(t *testing.T) {
	t.Parallel()
	// 100k occurrences × 1 MiB each would need ~100 GiB; it must be refused
	// from the arithmetic, not by trying.
	in := []byte(strings.Repeat("a", 100_000))
	_, _, err := Apply(in, []proto.FileEditOp{replaceOp("a", strings.Repeat("b", 1<<20), true)}, proto.MaxEditFileBytes)
	if codeOf(t, err) != CodeFileTooLarge {
		t.Fatalf("err = %v", err)
	}
}

func TestCountLines(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int{"": 0, "a": 1, "a\n": 1, "a\nb": 2, "a\nb\n": 2, "\n": 1} {
		if got := CountLines([]byte(in)); got != want {
			t.Errorf("CountLines(%q) = %d want %d", in, got, want)
		}
	}
}
