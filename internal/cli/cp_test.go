// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"strings"
	"testing"
)

func TestParseDownloadArgs(t *testing.T) {
	t.Parallel()
	type want struct {
		server, remote, local string
	}
	cases := []struct {
		name string
		args []string
		want want
	}{
		{"split, no local", []string{"web-01", "/root/x.log"}, want{"web-01", "/root/x.log", ""}},
		{"split, with local", []string{"web-01", "/root/x.log", "./"}, want{"web-01", "/root/x.log", "./"}},
		{"combined, no local", []string{"web-01:/root/x.log"}, want{"web-01", "/root/x.log", ""}},
		{"combined, with local", []string{"web-01:/root/x.log", "./"}, want{"web-01", "/root/x.log", "./"}},
		{"combined keeps later colons", []string{"db:/a:b", "out"}, want{"db", "/a:b", "out"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, r, l, err := parseDownloadArgs(tc.args)
			if err != nil {
				t.Fatalf("parseDownloadArgs(%v): %v", tc.args, err)
			}
			if s != tc.want.server || r != tc.want.remote || l != tc.want.local {
				t.Fatalf("parseDownloadArgs(%v) = %q,%q,%q want %q,%q,%q",
					tc.args, s, r, l, tc.want.server, tc.want.remote, tc.want.local)
			}
		})
	}

	bad := [][]string{
		{},                               // nothing
		{"web-01"},                       // split form needs a remote
		{"web-01:/root/x.log", "a", "b"}, // combined form takes at most one extra
		{"web-01:", "out"},               // empty remote in combined form
	}
	for _, args := range bad {
		if _, _, _, err := parseDownloadArgs(args); err == nil {
			t.Fatalf("parseDownloadArgs(%v) should have errored", args)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	t.Parallel()
	if got := firstNonEmpty("", "", "x", "y"); got != "x" {
		t.Fatalf("firstNonEmpty = %q want x", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("firstNonEmpty (all empty) = %q want empty", got)
	}
}

func TestUnifiedDiffIdentical(t *testing.T) {
	t.Parallel()
	if out := unifiedDiff("a", "b", "same\ntext\n", "same\ntext\n"); out != "" {
		t.Fatalf("identical inputs should yield no diff, got:\n%s", out)
	}
}

func TestUnifiedDiffChange(t *testing.T) {
	t.Parallel()
	a := "line1\nline2\nline3\n"
	b := "line1\nCHANGED\nline3\n"
	out := unifiedDiff("A:/f", "B:/f", a, b)
	if out == "" {
		t.Fatal("expected a diff for changed content")
	}
	for _, want := range []string{"--- A:/f", "+++ B:/f", "@@", "-line2", "+CHANGED", " line1", " line3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("diff missing %q:\n%s", want, out)
		}
	}
}

func TestUnifiedDiffAddOnly(t *testing.T) {
	t.Parallel()
	// Appending lines should produce inserts with no deletions.
	out := unifiedDiff("a", "b", "x\ny\n", "x\ny\nz\nw\n")
	if !strings.Contains(out, "+z") || !strings.Contains(out, "+w") {
		t.Fatalf("expected inserted lines z and w:\n%s", out)
	}
	if strings.Contains(out, "\n-") {
		t.Fatalf("append-only diff should have no deletions:\n%s", out)
	}
}

func TestUnifiedDiffEmptyToContent(t *testing.T) {
	t.Parallel()
	out := unifiedDiff("a", "b", "", "hello\n")
	if !strings.Contains(out, "+hello") {
		t.Fatalf("empty -> content should insert the line:\n%s", out)
	}
	// The original (empty) side starts at line 0 per unified-diff convention.
	if !strings.Contains(out, "@@ -0,0 +1") {
		t.Fatalf("expected -0,0 header for an empty original side:\n%s", out)
	}
}

func TestSplitLines(t *testing.T) {
	t.Parallel()
	if got := splitLines(""); got != nil {
		t.Fatalf("splitLines(\"\") = %v want nil", got)
	}
	got := splitLines("a\nb\n")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("splitLines trailing newline = %v want [a b]", got)
	}
	got = splitLines("a\nb")
	if len(got) != 2 {
		t.Fatalf("splitLines no trailing newline = %v want 2 lines", got)
	}
}
