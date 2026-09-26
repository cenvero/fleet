// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

// legacyRead is the historical log.read (a full forward scan) that the
// offset-based reader must reproduce for every plain read.
func legacyRead(t *testing.T, path, search string, tail int) ([]proto.LogLine, bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if tail <= 0 {
		tail = 200
	}
	lines, truncated, err := scanLogStream(f, search, min(tail, maxLogTailLines))
	if err != nil {
		t.Fatal(err)
	}
	return lines, truncated
}

func readLog(t *testing.T, payload proto.LogReadPayload) proto.LogReadResult {
	t.Helper()
	res, err := defaultLogReader().Read(context.Background(), payload)
	if err != nil {
		t.Fatalf("Read(%+v) error = %v", payload, err)
	}
	return res
}

func writeLog(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendLog(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func texts(lines []proto.LogLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = fmt.Sprintf("%d:%s", l.Number, l.Text)
	}
	return out
}

func TestLogReadMatchesLegacyScan(t *testing.T) {
	dir := t.TempDir()
	rng := rand.New(rand.NewPCG(7, 8))
	words := []string{"INFO ok", "ERROR boom", "error again", "", "\r", "İ", "\u212A", "x\xff", "GET /a 200", "warn"}
	for iter := 0; iter < 60; iter++ {
		var b strings.Builder
		for n := rng.IntN(3000); n > 0; n-- {
			b.WriteString(words[rng.IntN(len(words))])
			b.WriteString(strings.Repeat("y", rng.IntN(120)))
			if rng.IntN(6) == 0 {
				b.WriteString("\r")
			}
			b.WriteString("\n")
		}
		if rng.IntN(3) == 0 {
			b.WriteString("unterminated")
		}
		path := filepath.Join(dir, fmt.Sprintf("app-%d.log", iter))
		writeLog(t, path, b.String())
		for _, tail := range []int{0, 1, 7, 200, 5000} {
			for _, search := range []string{"", "error", " ERROR ", "i", "k", "200", "zzz"} {
				want, wantTrunc := legacyRead(t, path, search, tail)
				got := readLog(t, proto.LogReadPayload{Path: path, TailLines: tail, Search: search})
				if !reflect.DeepEqual(got.Lines, want) || got.Truncated != wantTrunc {
					t.Fatalf("iter %d tail %d search %q:\n got %v (trunc %v)\nwant %v (trunc %v)",
						iter, tail, search, texts(got.Lines), got.Truncated, texts(want), wantTrunc)
				}
				if got.Path != path || got.Cursor == nil || got.Reset || got.More {
					t.Fatalf("unexpected result metadata %+v", got)
				}
			}
		}
	}
}

func TestLogReadEmptyResultEncodesEmptyLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.log")
	writeLog(t, path, "")
	res := readLog(t, proto.LogReadPayload{Path: path})
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"lines":[]`) {
		t.Fatalf("empty result must encode lines as [], got %s", data)
	}
	// An old controller's view of the result: unknown fields are ignored and
	// the known ones are exactly what it always got.
	var old struct {
		Path      string          `json:"path"`
		Lines     []proto.LogLine `json:"lines"`
		Truncated bool            `json:"truncated,omitempty"`
	}
	if err := json.Unmarshal(data, &old); err != nil || old.Path != path || old.Lines == nil {
		t.Fatalf("old controller decode: %+v err=%v", old, err)
	}
}

func TestLogReadCursorFollowsAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	writeLog(t, path, "one\ntwo\nthree\n")

	first := readLog(t, proto.LogReadPayload{Path: path, TailLines: 2})
	if got := texts(first.Lines); !reflect.DeepEqual(got, []string{"2:two", "3:three"}) || !first.Truncated {
		t.Fatalf("tail = %v truncated=%v", got, first.Truncated)
	}
	if first.Cursor == nil || first.Cursor.Offset != 14 || first.Cursor.Line != 3 {
		t.Fatalf("cursor = %+v", first.Cursor)
	}

	// Nothing new: no lines, same position.
	idle := readLog(t, proto.LogReadPayload{Path: path, TailLines: 2, Cursor: first.Cursor})
	if len(idle.Lines) != 0 || idle.Reset || idle.More || *idle.Cursor != *first.Cursor {
		t.Fatalf("idle poll = %+v", idle)
	}

	// More lines than TailLines arrive between polls: all are returned.
	appendLog(t, path, "four\nfive\r\nsix\nsev")
	next := readLog(t, proto.LogReadPayload{Path: path, TailLines: 2, Cursor: idle.Cursor})
	if got := texts(next.Lines); !reflect.DeepEqual(got, []string{"4:four", "5:five", "6:six", "7:sev"}) {
		t.Fatalf("poll = %v", got)
	}
	// The unterminated "sev" is reported but not consumed.
	if next.Cursor.Line != 6 || next.Reset {
		t.Fatalf("cursor after partial line = %+v", next.Cursor)
	}
	appendLog(t, path, "en\n")
	next = readLog(t, proto.LogReadPayload{Path: path, TailLines: 2, Cursor: next.Cursor})
	if got := texts(next.Lines); !reflect.DeepEqual(got, []string{"7:seven"}) || next.Cursor.Line != 7 {
		t.Fatalf("completed line poll = %v cursor %+v", got, next.Cursor)
	}

	// A search applies to cursor reads too, with absolute numbers.
	appendLog(t, path, "ERROR a\nfine\nerror b\n")
	next = readLog(t, proto.LogReadPayload{Path: path, Search: "error", Cursor: next.Cursor})
	if got := texts(next.Lines); !reflect.DeepEqual(got, []string{"8:ERROR a", "10:error b"}) || next.Cursor.Line != 10 {
		t.Fatalf("search poll = %v cursor %+v", got, next.Cursor)
	}
}

func TestLogReadCursorResetsOnTruncationAndRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	writeLog(t, path, "a1\na2\na3\na4\n")
	cur := readLog(t, proto.LogReadPayload{Path: path}).Cursor

	// copytruncate: same inode, shorter file.
	writeLog(t, path, "b1\n")
	res := readLog(t, proto.LogReadPayload{Path: path, Cursor: cur})
	if !res.Reset || !reflect.DeepEqual(texts(res.Lines), []string{"1:b1"}) {
		t.Fatalf("truncation: %+v", res)
	}

	// Truncated and regrown past the old offset between polls.
	cur = res.Cursor
	writeLog(t, path, "c1 longer than before\nc2\n")
	res = readLog(t, proto.LogReadPayload{Path: path, Cursor: cur})
	if !res.Reset || !reflect.DeepEqual(texts(res.Lines), []string{"1:c1 longer than before", "2:c2"}) {
		t.Fatalf("truncate+regrow: %+v", res)
	}

	// Rotation by rename: the path now names a new file (same size even).
	cur = res.Cursor
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	writeLog(t, path, "d1 longer than before\nd2\n")
	res = readLog(t, proto.LogReadPayload{Path: path, Cursor: cur})
	if logFileID(mustStat(t, path)) != "" && !res.Reset {
		t.Fatalf("rotation must reset: %+v", res)
	}
	if !reflect.DeepEqual(texts(res.Lines), []string{"1:d1 longer than before", "2:d2"}) {
		t.Fatalf("rotation lines: %v", texts(res.Lines))
	}

	// Garbage cursors never read out of place; they reset.
	for _, bad := range []proto.LogCursor{
		{Offset: -1}, {Offset: 3, Line: 4}, {Offset: 1 << 40}, {Offset: 5, Line: 1, FileID: res.Cursor.FileID, Sum: res.Cursor.Sum},
		{Offset: res.Cursor.Offset, Line: res.Cursor.Line, FileID: "0:0", Sum: res.Cursor.Sum},
		{Offset: res.Cursor.Offset, Line: res.Cursor.Line, FileID: res.Cursor.FileID, Sum: "bogus"},
	} {
		got := readLog(t, proto.LogReadPayload{Path: path, Cursor: &bad})
		if !got.Reset || len(got.Lines) != 2 {
			t.Fatalf("cursor %+v: %+v", bad, got)
		}
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestLogReadCursorPagesLargeAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	writeLog(t, path, "start\n")
	cur := readLog(t, proto.LogReadPayload{Path: path}).Cursor

	// ~6MiB appended at once: more than one page of returned text.
	var b strings.Builder
	const n = 60000
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %06d %s\n", i, strings.Repeat("z", 90))
	}
	appendLog(t, path, b.String())

	seen := 0
	pages := 0
	for {
		res := readLog(t, proto.LogReadPayload{Path: path, Cursor: cur})
		pages++
		for _, l := range res.Lines {
			seen++
			if want := fmt.Sprintf("line %06d ", seen); l.Number != seen+1 || !strings.HasPrefix(l.Text, want) {
				t.Fatalf("line %d = %d:%q", seen, l.Number, l.Text[:min(20, len(l.Text))])
			}
		}
		cur = res.Cursor
		if !res.More {
			break
		}
	}
	if seen != n || pages < 2 {
		t.Fatalf("read %d lines in %d pages, want %d lines over several pages", seen, pages, n)
	}
}

func TestLogReadCursorKeepsSandbox(t *testing.T) {
	root := t.TempDir()
	SetAllowedFileRoots([]string{root})
	defer SetAllowedFileRoots(nil)
	outside := filepath.Join(t.TempDir(), "secret.log")
	writeLog(t, outside, "secret\n")
	_, err := defaultLogReader().Read(context.Background(), proto.LogReadPayload{Path: outside, Cursor: &proto.LogCursor{}})
	if rpcErr, ok := err.(*RPCError); !ok || rpcErr.Code != "invalid_log_path" {
		t.Fatalf("cursor read outside --file-root: err = %v", err)
	}
}

func TestLogReadLongLineErrorsLikeBefore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "long.log")
	writeLog(t, path, "ok\n"+strings.Repeat("x", 1024*1024)+"\n")
	_, err := defaultLogReader().Read(context.Background(), proto.LogReadPayload{Path: path})
	rpcErr, ok := err.(*RPCError)
	if !ok || rpcErr.Code != "log_read_failed" || rpcErr.Message != "bufio.Scanner: token too long" {
		t.Fatalf("err = %#v", err)
	}
}

func TestLogReadNonRegularFileUsesStreamScan(t *testing.T) {
	dir := t.TempDir()
	// A directory cannot be read: same error path as before.
	_, err := defaultLogReader().Read(context.Background(), proto.LogReadPayload{Path: dir})
	if rpcErr, ok := err.(*RPCError); !ok || rpcErr.Code != "log_read_failed" {
		t.Fatalf("directory read err = %v", err)
	}
}
