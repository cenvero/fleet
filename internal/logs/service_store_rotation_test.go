// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package logs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

// remoteLines returns lines from..to of a remote log whose line n reads
// "<prefix><n>".
func remoteLines(prefix string, from, to int) []proto.LogLine {
	out := make([]proto.LogLine, 0, to-from+1)
	for n := from; n <= to; n++ {
		out = append(out, proto.LogLine{Number: n, Text: fmt.Sprintf("%s%d", prefix, n)})
	}
	return out
}

func texts(prefix string, from, to int) []string {
	out := make([]string, 0, to-from+1)
	for n := from; n <= to; n++ {
		out = append(out, fmt.Sprintf("%s%d", prefix, n))
	}
	return out
}

func newRotationStore(t *testing.T) *ServiceStore {
	t.Helper()
	return NewServiceStore(filepath.Join(t.TempDir(), "_aggregated"), 64<<20, 3, time.Hour)
}

func mustAppend(t *testing.T, store *ServiceStore, lines []proto.LogLine, src AppendSource) {
	t.Helper()
	if err := store.AppendFrom("web-01", "nginx.service", lines, src); err != nil {
		t.Fatalf("AppendFrom() error = %v", err)
	}
}

// cachedTexts returns every cached line, oldest first.
func cachedTexts(t *testing.T, store *ServiceStore) []string {
	t.Helper()
	res, err := store.Read("web-01", "nginx.service", "", 1_000_000)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	out := make([]string, len(res.Lines))
	for i, l := range res.Lines {
		if l.Number != i+1 {
			t.Fatalf("cached line %d numbered %d", i+1, l.Number)
		}
		out[i] = l.Text
	}
	return out
}

func expectCache(t *testing.T, store *ServiceStore, want ...[]string) {
	t.Helper()
	var all []string
	for _, w := range want {
		all = append(all, w...)
	}
	got := cachedTexts(t, store)
	if strings.Join(got, "\n") != strings.Join(all, "\n") {
		t.Fatalf("cache has %d lines, want %d\n got: %s\nwant: %s", len(got), len(all), summarize(got), summarize(all))
	}
}

func summarize(lines []string) string {
	if len(lines) <= 8 {
		return strings.Join(lines, ",")
	}
	return strings.Join(lines[:4], ",") + ",...," + strings.Join(lines[len(lines)-4:], ",")
}

// TestServiceStoreRotationToShorterFile: the new file has fewer lines than
// were cached from the old one (this always worked: its numbers fall below
// the cursor).
func TestServiceStoreRotationToShorterFile(t *testing.T) {
	store := newRotationStore(t)
	mustAppend(t, store, remoteLines("a-", 1, 100), AppendSource{})
	mustAppend(t, store, remoteLines("b-", 1, 30), AppendSource{})
	expectCache(t, store, texts("a-", 1, 100), texts("b-", 1, 30))
}

// TestServiceStoreRotationToLongerFile is the B10 regression: after a
// rotation the new file had already grown past the old cursor, so lines
// 1..cursor of the new file were taken for already-cached lines and dropped.
func TestServiceStoreRotationToLongerFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  AppendSource
	}{
		{"content only (old agent)", AppendSource{}},
		{"file id", AppendSource{FileID: "8:2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newRotationStore(t)
			first := AppendSource{}
			if tc.src.FileID != "" {
				first.FileID = "8:1"
			}
			mustAppend(t, store, remoteLines("a-", 1, 100), first)
			mustAppend(t, store, remoteLines("b-", 1, 150), tc.src)
			expectCache(t, store, texts("a-", 1, 100), texts("b-", 1, 150))
			// And the new file then keeps growing normally.
			mustAppend(t, store, remoteLines("b-", 51, 170), tc.src)
			expectCache(t, store, texts("a-", 1, 100), texts("b-", 1, 170))
		})
	}
}

// TestServiceStoreRotationWindowOverlapsCursor: after a rotation the read
// window straddles the old cursor ([71..120] against a cursor at 100); the
// whole window is new, not just 101..120.
func TestServiceStoreRotationWindowOverlapsCursor(t *testing.T) {
	store := newRotationStore(t)
	mustAppend(t, store, remoteLines("a-", 1, 100), AppendSource{})
	mustAppend(t, store, remoteLines("b-", 71, 120), AppendSource{})
	expectCache(t, store, texts("a-", 1, 100), texts("b-", 71, 120))
}

// TestServiceStoreRotationWithIdenticalContent: a log of identical lines
// cannot be told apart by content; the agent's file id or reset flag can.
func TestServiceStoreRotationWithIdenticalContent(t *testing.T) {
	same := func(from, to int) []proto.LogLine {
		out := remoteLines("x", from, to)
		for i := range out {
			out[i].Text = "heartbeat ok"
		}
		return out
	}
	heartbeats := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "heartbeat ok"
		}
		return out
	}

	store := newRotationStore(t)
	mustAppend(t, store, same(1, 10), AppendSource{FileID: "8:1"})
	mustAppend(t, store, same(1, 15), AppendSource{FileID: "8:2"}) // rotated (rename)
	expectCache(t, store, heartbeats(25))

	store = newRotationStore(t)
	mustAppend(t, store, same(1, 10), AppendSource{FileID: "8:1"})
	mustAppend(t, store, same(1, 15), AppendSource{FileID: "8:1", Reset: true}) // copytruncate seen by follow
	expectCache(t, store, heartbeats(25))
}

// TestServiceStoreNormalGrowth: overlapping tail windows, incremental follow
// batches, repeated reads and a window that skipped ahead neither duplicate
// nor drop anything that was read.
func TestServiceStoreNormalGrowth(t *testing.T) {
	for _, src := range []AppendSource{{}, {FileID: "8:1"}} {
		store := newRotationStore(t)
		mustAppend(t, store, remoteLines("l-", 1, 100), src)   // first tail read
		mustAppend(t, store, remoteLines("l-", 51, 160), src)  // overlapping tail
		mustAppend(t, store, remoteLines("l-", 161, 161), src) // follow: one new line
		mustAppend(t, store, remoteLines("l-", 162, 170), src) // follow: a few more
		mustAppend(t, store, remoteLines("l-", 71, 170), src)  // re-read, nothing new
		mustAppend(t, store, remoteLines("l-", 170, 170), src) // --lines 1
		expectCache(t, store, texts("l-", 1, 170))
		mustAppend(t, store, remoteLines("l-", 201, 210), src) // window moved past: unread gap
		expectCache(t, store, texts("l-", 1, 170), texts("l-", 201, 210))
	}
}

// TestServiceStoreCompletedPartialLineIsNotARotation: a read can end with a
// line still being written; seeing it complete later is the same file.
func TestServiceStoreCompletedPartialLineIsNotARotation(t *testing.T) {
	store := newRotationStore(t)
	first := remoteLines("l-", 1, 10)
	first[9].Text = "l-1" // "l-10" caught half written
	mustAppend(t, store, first, AppendSource{})
	mustAppend(t, store, remoteLines("l-", 11, 12), AppendSource{}) // follow batch after it
	mustAppend(t, store, remoteLines("l-", 5, 14), AppendSource{})  // tail read covering it, complete
	partial := append(texts("l-", 1, 9), "l-1")
	expectCache(t, store, partial, texts("l-", 11, 14))
}

// TestServiceStoreOldCursorFormat: a cursor file written before fingerprints
// existed still loads and dedupes by line number exactly as before, and is
// upgraded by the next append.
func TestServiceStoreOldCursorFormat(t *testing.T) {
	store := newRotationStore(t)
	base := store.basePath("web-01", "nginx.service")
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, []byte(strings.Join(texts("l-", 1, 100), "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cursorPath := store.cursorPath("web-01", "nginx.service")
	if err := os.WriteFile(cursorPath, []byte(`{"last_remote_line":100}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	mustAppend(t, store, remoteLines("l-", 51, 150), AppendSource{})
	expectCache(t, store, texts("l-", 1, 150))

	data, err := os.ReadFile(cursorPath)
	if err != nil {
		t.Fatal(err)
	}
	var upgraded logCursor
	if err := json.Unmarshal(data, &upgraded); err != nil {
		t.Fatal(err)
	}
	if upgraded.LastRemoteLine != 150 || len(upgraded.Recent) != maxRecentFingerprints || upgraded.Recent[len(upgraded.Recent)-1].Number != 150 {
		t.Fatalf("cursor after append = %s", data)
	}
	// The new cursor stays readable by an older controller (unknown fields
	// are ignored) and keeps last_remote_line.
	var old struct {
		LastRemoteLine int `json:"last_remote_line"`
	}
	if err := json.Unmarshal(data, &old); err != nil || old.LastRemoteLine != 150 {
		t.Fatalf("old-format decode: %+v %v", old, err)
	}
	// Now rotation is detected.
	mustAppend(t, store, remoteLines("r-", 1, 200), AppendSource{})
	expectCache(t, store, texts("l-", 1, 150), texts("r-", 1, 200))
}
