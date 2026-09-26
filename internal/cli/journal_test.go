// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

// fakeJournalctl stands in for journalctl on PATH. It serves the entries in
// $FAKE_JOURNAL ("<seq>\t<message>" per line), records every argv (one
// "<arg>" per argument) in $FAKE_JOURNAL_ARGV, and mimics systemd 245:
// -n without a cursor takes the last n entries (then --grep filters them),
// -n after --after-cursor takes the first n entries after it, --show-cursor
// prints the last entry examined, --grep matches the message only.
const fakeJournalctl = `#!/bin/sh
{ for a in "$@"; do printf '<%s>' "$a"; done; printf '\n'; } >> "$FAKE_JOURNAL_ARGV"
after= n= pat= show=0 quiet=0 unit=
while [ $# -gt 0 ]; do
  case "$1" in
    --version) printf 'systemd %s (%s-1)\n%s\n' "$FAKE_JOURNAL_VERSION" "$FAKE_JOURNAL_VERSION" "${FAKE_JOURNAL_FEATURES:-+PAM +PCRE2}"; exit 0 ;;
    -u) unit=$2; shift ;;
    --since) shift ;;
    -n) n=$2; shift ;;
    --no-pager) ;;
    -q) quiet=1 ;;
    --after-cursor=*) after=${1#--after-cursor=} ;;
    --show-cursor)
      if [ -n "$FAKE_NO_SHOW_CURSOR" ]; then echo "journalctl: unrecognized option '--show-cursor'" >&2; exit 1; fi
      show=1 ;;
    --grep=*)
      if [ -n "$FAKE_NO_PCRE" ]; then echo "Compiled without pattern matching support" >&2; exit 1; fi
      pat=${1#--grep=} ;;
    --case-sensitive=false) ;;
    *) echo "journalctl: unexpected argument: $1" >&2; exit 1 ;;
  esac
  shift
done
[ "$unit" = "$FAKE_JOURNAL_UNIT" ] || { echo "unexpected unit: $unit" >&2; exit 1; }
if [ -n "$after" ]; then
  if [ -n "$FAKE_BAD_CURSOR" ]; then echo "Failed to seek to cursor: Invalid argument" >&2; exit 1; fi
  case "$after" in s=*) after=${after#s=} ;; *) echo "Failed to seek to cursor: Invalid argument" >&2; exit 1 ;; esac
fi
exec awk -v after="$after" -v n="$n" -v pat="$pat" -v show="$show" -v quiet="$quiet" -v banner="$FAKE_BANNER" '
BEGIN {
  FS = "\t"; lit = ""
  for (i = 1; i <= length(pat); i++) { c = substr(pat, i, 1); if (c == "\\") { i++; c = substr(pat, i, 1) }; lit = lit c }
  lit = tolower(lit)
}
{ seq[NR] = $1; msg[NR] = $2; count = NR }
END {
  start = 1; limit = -1
  if (after != "") {
    start = count + 1
    for (i = 1; i <= count; i++) if (seq[i] + 0 > after + 0) { start = i; break }
    if (n != "") limit = n + 0
  } else if (n != "") {
    start = count - n + 1; if (start < 1) start = 1
  }
  if (banner != "" && quiet == 0 && count > 0) print "-- Logs begin at Sat 2026-09-26 00:00:00 UTC, end at " seq[count] ". --"
  shown = 0; pos = 0
  for (i = start; i <= count; i++) {
    if (limit >= 0 && shown >= limit) break
    pos = i
    if (lit != "" && index(tolower(msg[i]), lit) == 0) continue
    print "Sep 26 12:00:00 host app[42]: " msg[i]
    shown++
  }
  if (shown == 0 && quiet == 0) print "-- No entries --"
  if (show == 1) {
    if (pos > 0) print "-- cursor: s=" seq[pos]
    else if (after != "") print "-- cursor: s=" after
  }
}' "$FAKE_JOURNAL"
`

type fakeJournal struct {
	t       *testing.T
	dir     string
	entries string
	argvLog string
	env     []string
	seq     int
}

func newFakeJournal(t *testing.T) *fakeJournal {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake journalctl needs a POSIX shell")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "journalctl"), []byte(fakeJournalctl), 0o700); err != nil { // #nosec G306 -- test helper script must be executable
		t.Fatal(err)
	}
	f := &fakeJournal{t: t, dir: dir, entries: filepath.Join(dir, "entries"), argvLog: filepath.Join(dir, "argv")}
	if err := os.WriteFile(f.entries, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.env = []string{
		"FAKE_JOURNAL=" + f.entries,
		"FAKE_JOURNAL_ARGV=" + f.argvLog,
		"FAKE_JOURNAL_UNIT=nginx.service",
		"FAKE_JOURNAL_VERSION=245",
	}
	return f
}

func (f *fakeJournal) setenv(kv ...string) { f.env = append(f.env, kv...) }

// add appends journal entries.
func (f *fakeJournal) add(msgs ...string) {
	f.t.Helper()
	file, err := os.OpenFile(f.entries, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	defer file.Close()
	for _, m := range msgs {
		f.seq++
		fmt.Fprintf(file, "%d\t%s\n", f.seq, m)
	}
}

func (f *fakeJournal) addN(prefix string, from, to int) {
	msgs := make([]string, 0, to-from+1)
	for i := from; i <= to; i++ {
		msgs = append(msgs, fmt.Sprintf("%s%d", prefix, i))
	}
	f.add(msgs...)
}

// exec runs a command the way the agent's shell.exec does: /bin/sh -c.
func (f *fakeJournal) exec(command string) (proto.ExecResult, error) {
	cmd := exec.Command("/bin/sh", "-c", command) // #nosec G204 -- test runs the command under test against a fake journalctl
	cmd.Env = append(append(os.Environ(), f.env...), "PATH="+f.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := proto.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		err = nil
	}
	return res, err
}

// invocations returns the recorded argv of every journalctl run.
func (f *fakeJournal) invocations() []string {
	data, err := os.ReadFile(f.argvLog)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func msgs(out string) []string {
	var got []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line != "" {
			got = append(got, strings.TrimPrefix(line, "Sep 26 12:00:00 host app[42]: "))
		}
	}
	return got
}

func seqMsgs(prefix string, from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf("%s%d", prefix, i))
	}
	return out
}

func TestJournalQueryCommand(t *testing.T) {
	cases := []struct {
		q    journalQuery
		want string
	}{
		{journalQuery{unit: "nginx", since: "1h", tail: 50}, "journalctl -u 'nginx' --no-pager --since '1h' -n 50"},
		{journalQuery{unit: "nginx"}, "journalctl -u 'nginx' --no-pager"},
		{journalQuery{unit: "nginx", since: "1h", tail: 200, showCursor: true, pattern: `err\ or`},
			`journalctl -u 'nginx' --no-pager --since '1h' -n 200 '--grep=err\ or' --case-sensitive=false --show-cursor`},
		// A cursor replaces --since (journalctl refuses both).
		{journalQuery{unit: "nginx", since: "1h", cursor: "s=ab;i=1", tail: 1000, quiet: true, showCursor: true},
			"journalctl -u 'nginx' --no-pager -q '--after-cursor=s=ab;i=1' -n 1000 --show-cursor"},
	}
	for _, tc := range cases {
		if got := tc.q.command(); got != tc.want {
			t.Errorf("command() = %s\nwant       %s", got, tc.want)
		}
	}
}

func TestJournalPattern(t *testing.T) {
	cases := map[string]string{
		"error":         "error",
		"ERROR 500":     `error\ 500`,
		"a.b*c":         `a\.b\*c`,
		`it's $(id)`:    `it\'s\ \$\(id\)`,
		"--output=json": `\-\-output\=json`,
		"\\":            `\\`,
		"":              "",
		"naïve":         "", // non-ASCII: client-side only
		"tab\there":     "", // control characters: client-side only
	}
	for in, want := range cases {
		if got := journalPattern(in); got != want {
			t.Errorf("journalPattern(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJournalSupportsGrep(t *testing.T) {
	for _, tc := range []struct {
		out  string
		code int
		want bool
	}{
		{"systemd 245 (245.4-4ubuntu3)\n+PAM +PCRE2\n", 0, true},
		{"systemd 237\n", 0, true},
		{"systemd 236\n", 0, false},
		{"systemd 249 (249.11)\n+PAM -PCRE2\n", 0, false},
		{"garbage", 0, false},
		{"", 127, false},
	} {
		exec := func(string) (proto.ExecResult, error) {
			return proto.ExecResult{Stdout: tc.out, ExitCode: tc.code}, nil
		}
		if got := journalSupportsGrep(exec); got != tc.want {
			t.Errorf("journalSupportsGrep(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

// TestJournalCommandsAreInjectionSafe runs hostile --grep values through the
// real shell: journalctl must receive each as one literal --grep argument and
// nothing else may run.
func TestJournalCommandsAreInjectionSafe(t *testing.T) {
	fj := newFakeJournal(t)
	canary := filepath.Join(fj.dir, "pwned")
	fj.add("hello")
	for _, grep := range []string{
		"'; touch " + canary + "; echo '",
		"$(touch " + canary + ")",
		"`touch " + canary + "`",
		"x\" ; touch " + canary + " ; \"",
		"--output=json",
		"-n 1",
	} {
		var out bytes.Buffer
		if err := pageJournalTo(&out, fj.exec, "web-01", "nginx.service", "1h", grep, 10); err != nil {
			t.Fatalf("grep %q: %v", grep, err)
		}
		if _, err := os.Stat(canary); err == nil {
			t.Fatalf("grep %q executed a command", grep)
		}
		calls := fj.invocations()
		last := calls[len(calls)-1]
		want := "<-u><nginx.service><--no-pager><--since><1h><-n><10><--grep=" + journalPattern(grep) + "><--case-sensitive=false>"
		if last != want {
			t.Fatalf("grep %q: journalctl argv\n got %s\nwant %s", grep, last, want)
		}
	}
}

func TestPageJournalServerSideGrep(t *testing.T) {
	fj := newFakeJournal(t)
	fj.add("GET / 200", "upstream ERROR timeout", "GET /x 200", "error: disk")
	var out bytes.Buffer
	if err := pageJournalTo(&out, fj.exec, "web-01", "nginx.service", "", "error", 200); err != nil {
		t.Fatal(err)
	}
	if got := msgs(out.String()); strings.Join(got, "|") != "upstream ERROR timeout|error: disk" {
		t.Fatalf("output %q", got)
	}
	calls := fj.invocations()
	if len(calls) != 2 || calls[0] != "<--version>" || !strings.Contains(calls[1], "<--grep=error>") {
		t.Fatalf("invocations %v", calls)
	}
}

func TestPageJournalGrepFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{"old systemd", []string{"FAKE_JOURNAL_VERSION=229"}},
		{"no PCRE2 at runtime", []string{"FAKE_NO_PCRE=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fj := newFakeJournal(t)
			fj.setenv(tc.env...)
			fj.add("GET / 200", "upstream ERROR timeout", "GET /x 200")
			var out bytes.Buffer
			if err := pageJournalTo(&out, fj.exec, "web-01", "nginx.service", "", "error", 200); err != nil {
				t.Fatal(err)
			}
			if got := msgs(out.String()); strings.Join(got, "|") != "upstream ERROR timeout" {
				t.Fatalf("output %q", got)
			}
			calls := fj.invocations()
			if last := calls[len(calls)-1]; strings.Contains(last, "--grep") {
				t.Fatalf("fallback must filter client-side, last call %s", last)
			}
		})
	}
}

// TestJournalFollowUsesCursorAndLosesNothing: bursts much larger than the
// window, and larger than a page, arrive between polls.
func TestJournalFollowUsesCursorAndLosesNothing(t *testing.T) {
	saved := journalFollowPage
	journalFollowPage = 50
	defer func() { journalFollowPage = saved }()

	fj := newFakeJournal(t)
	fj.setenv("FAKE_BANNER=1")
	fj.addN("m", 1, 30)
	var out bytes.Buffer
	f := newJournalFollower(fj.exec, &out, "web-01", "nginx.service", "", "", 10)
	if err := f.start(); err != nil {
		t.Fatal(err)
	}
	want := append([]string{"-- Logs begin at Sat 2026-09-26 00:00:00 UTC, end at 30. --"}, seqMsgs("m", 21, 30)...)

	drain := func() {
		t.Helper()
		for i := 0; ; i++ {
			more, err := f.poll()
			if err != nil {
				t.Fatal(err)
			}
			if !more {
				return
			}
			if i > 100 {
				t.Fatal("poll never caught up")
			}
		}
	}
	drain() // nothing new
	fj.addN("m", 31, 530)
	drain()
	want = append(want, seqMsgs("m", 31, 530)...)
	fj.addN("m", 531, 531)
	drain()
	drain()
	want = append(want, "m531")
	if got := msgs(out.String()); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("followed output differs:\n got %d lines, first %v last %v\nwant %d lines", len(got), got[:3], got[len(got)-3:], len(want))
	}
	for _, call := range fj.invocations()[1:] {
		if !strings.Contains(call, "<--after-cursor=s=") || !strings.Contains(call, "<-q>") || !strings.Contains(call, "<-n><50>") {
			t.Fatalf("poll did not resume from the cursor: %s", call)
		}
	}
}

func TestJournalFollowServerGrep(t *testing.T) {
	fj := newFakeJournal(t)
	fj.add("ok 1", "ERROR 1", "ok 2")
	var out bytes.Buffer
	f := newJournalFollower(fj.exec, &out, "web-01", "nginx.service", "", "error", 200)
	if err := f.start(); err != nil {
		t.Fatal(err)
	}
	fj.add("ok 3", "error 2", "ok 4", "ok 5")
	if _, err := f.poll(); err != nil {
		t.Fatal(err)
	}
	fj.add("ok 6") // the cursor must still advance past non-matching entries
	if _, err := f.poll(); err != nil {
		t.Fatal(err)
	}
	if got := msgs(out.String()); strings.Join(got, "|") != "ERROR 1|error 2" {
		t.Fatalf("output %q", got)
	}
	calls := fj.invocations()
	if last := calls[len(calls)-1]; last != "<-u><nginx.service><--no-pager><-q><--after-cursor=s=7><-n><1000><--grep=error><--case-sensitive=false><--show-cursor>" {
		t.Fatalf("last poll argv %s", last)
	}
}

// TestJournalFollowEmptyJournalThenEntries: no cursor exists until the unit
// logs something; the follower re-reads the tail until one does.
func TestJournalFollowEmptyJournalThenEntries(t *testing.T) {
	fj := newFakeJournal(t)
	var out bytes.Buffer
	f := newJournalFollower(fj.exec, &out, "web-01", "nginx.service", "", "", 200)
	if err := f.start(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.poll(); err != nil {
		t.Fatal(err)
	}
	fj.add("first", "second")
	for range 3 {
		if _, err := f.poll(); err != nil {
			t.Fatal(err)
		}
	}
	fj.add("third")
	if _, err := f.poll(); err != nil {
		t.Fatal(err)
	}
	if got := msgs(out.String()); strings.Join(got, "|") != "-- No entries --|first|second|third" {
		t.Fatalf("output %q", got)
	}
}

// TestJournalFollowFallbacks: a journalctl without --show-cursor is followed
// exactly as before, and a rejected cursor falls back to the de-duplicated
// tail without losing the follow.
func TestJournalFollowFallbacks(t *testing.T) {
	t.Run("no show-cursor", func(t *testing.T) {
		fj := newFakeJournal(t)
		fj.setenv("FAKE_NO_SHOW_CURSOR=1")
		fj.add("a", "b")
		var out bytes.Buffer
		f := newJournalFollower(fj.exec, &out, "web-01", "nginx.service", "1h", "", 200)
		if err := f.start(); err != nil {
			t.Fatal(err)
		}
		fj.add("c")
		if _, err := f.poll(); err != nil {
			t.Fatal(err)
		}
		if got := msgs(out.String()); strings.Join(got, "|") != "a|b|c" {
			t.Fatalf("output %q", got)
		}
		calls := fj.invocations()
		if last := calls[len(calls)-1]; last != "<-u><nginx.service><--no-pager><--since><1h><-n><200>" {
			t.Fatalf("legacy poll argv %s", last)
		}
	})
	t.Run("cursor rejected", func(t *testing.T) {
		fj := newFakeJournal(t)
		fj.add("a", "b")
		var out bytes.Buffer
		f := newJournalFollower(fj.exec, &out, "web-01", "nginx.service", "", "", 200)
		if err := f.start(); err != nil {
			t.Fatal(err)
		}
		fj.setenv("FAKE_BAD_CURSOR=1")
		fj.add("c")
		if _, err := f.poll(); err != nil {
			t.Fatal(err)
		}
		fj.add("d")
		if _, err := f.poll(); err != nil {
			t.Fatal(err)
		}
		if got := msgs(out.String()); strings.Join(got, "|") != "a|b|c|d" {
			t.Fatalf("output %q", got)
		}
	})
}
