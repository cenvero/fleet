// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"strings"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

func TestParseTargetArgs(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"web-01:/etc/x"}, {"web-01", "/etc/x"}} {
		s, p, err := parseTargetArgs(args)
		if err != nil || s != "web-01" || p != "/etc/x" {
			t.Fatalf("parseTargetArgs(%v) = %q %q %v", args, s, p, err)
		}
	}
	if _, _, err := parseTargetArgs([]string{"no-colon"}); err == nil {
		t.Fatal("a lone argument without a colon must be refused")
	}
}

func TestParseLineRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		spec       string
		n          int
		start, end int
		ok         bool
	}{
		{"", 10, 1, 10, true},
		{"3:5", 10, 3, 5, true},
		{"8:", 10, 8, 10, true},
		{":2", 10, 1, 2, true},
		{"4", 10, 4, 4, true},
		{"5:99", 10, 5, 10, true},
		{"", 0, 0, 0, true},
		{"0:3", 10, 0, 0, false},
		{"x:3", 10, 0, 0, false},
		{"20:30", 10, 0, 0, false},
	}
	for _, c := range cases {
		start, end, err := parseLineRange(c.spec, c.n)
		if (err == nil) != c.ok || (c.ok && (start != c.start || end != c.end)) {
			t.Errorf("parseLineRange(%q, %d) = %d, %d, %v", c.spec, c.n, start, end, err)
		}
	}
}

func TestParseEditList(t *testing.T) {
	t.Parallel()
	ops, err := parseEditList([]byte(`[{"old":"a","new":"b","all":true},{"kind":"insert","line":3,"text":"c"}]`))
	if err != nil || len(ops) != 2 || !ops[0].All || ops[1].Kind != proto.FileEditOpInsert || ops[1].Line != 3 {
		t.Fatalf("ops = %+v, %v", ops, err)
	}
	for _, bad := range []string{`[]`, `{"old":"a"}`, `[{"old":"a","neww":"b"}]`, `not json`} {
		if _, err := parseEditList([]byte(bad)); err == nil {
			t.Errorf("parseEditList(%s) should fail", bad)
		}
	}
}

func TestSplitViewLinesKeepsEndings(t *testing.T) {
	t.Parallel()
	got := splitViewLines([]byte("a\r\nb\nc"))
	if len(got) != 3 || got[0] != "a\r\n" || got[1] != "b\n" || got[2] != "c" {
		t.Fatalf("splitViewLines = %q", got)
	}
	if splitViewLines(nil) != nil {
		t.Fatal("empty content has no lines")
	}
}

func TestFileEditFlagValidation(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"two modes":           {"web-01:/f", "--old", "a", "--new", "b", "--insert-after", "1", "--text", "x"},
		"old without new":     {"web-01:/f", "--old", "a"},
		"all without old":     {"web-01:/f", "--all", "--content", "x"},
		"text without insert": {"web-01:/f", "--text", "x"},
		"create without body": {"web-01:/f", "--create"},
		"mode without create": {"web-01:/f", "--content", "x", "--mode", "0644", "--force"},
		"undo with dry run":   {"web-01:/f", "--undo", "--dry-run"},
	}
	for name, args := range cases {
		cmd := newFileEditCommand(new(string))
		cmd.SetArgs(args)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		err := cmd.Execute()
		if err == nil || strings.Contains(err.Error(), "not initialized") {
			t.Errorf("%s: expected a flag validation error before any app access, got %v", name, err)
		}
	}
}

func TestIsHexSHA256(t *testing.T) {
	t.Parallel()
	good := strings.Repeat("ab", 32)
	if !isHexSHA256(good) || !isHexSHA256(strings.ToUpper(good)) || isHexSHA256(good[:63]) || isHexSHA256(strings.Repeat("zz", 32)) {
		t.Fatal("isHexSHA256 misclassifies")
	}
}

func TestTerminalSafe(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"plain\ttext":           "plain\ttext",
		"\x1b]0;pwned\x07title": `\x1b]0;pwned\x07title`,
		"a\u009bb":              `a\x9bb`,
		"raw\x9bbyte":           "raw\ufffdbyte",
		"bad\xffbyte":           "bad\ufffdbyte",
		"ünïcödé ok":            "ünïcödé ok",
		"del\x7f":               `del\x7f`,
	}
	for in, want := range cases {
		if got := terminalSafe(in); got != want {
			t.Errorf("terminalSafe(%q) = %q, want %q", in, got, want)
		}
	}
	if got := terminalSafeLines("-a\r\n+b\x1b[2J\r\n"); got != "-a\n+b\\x1b[2J\n" {
		t.Errorf("terminalSafeLines = %q", got)
	}
}
