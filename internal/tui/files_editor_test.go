// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// TestEditorLoadSaveLocal exercises the local editor load/save plumbing end to
// end: write a file, load it, mutate it, save it, and confirm the bytes land.
func TestEditorLoadSaveLocal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "hello.go")
	original := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(p, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadFileForEdit(nil, "", p)
	if err != nil {
		t.Fatalf("loadFileForEdit: %v", err)
	}
	if got != original {
		t.Fatalf("loaded %q, want %q", got, original)
	}

	updated := original + "// edited\n"
	if err := saveFileFromEdit(nil, "", p, []byte(updated)); err != nil {
		t.Fatalf("saveFileFromEdit: %v", err)
	}
	roundtrip, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(roundtrip) != updated {
		t.Fatalf("saved file = %q, want %q", roundtrip, updated)
	}
}

// TestEditorRefusesLargeAndBinary verifies the load guard rejects oversize and
// binary files with a clear error instead of loading them.
func TestEditorRefusesLargeAndBinary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Binary file (contains a NUL byte).
	binPath := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(binPath, []byte{'a', 0x00, 'b'}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFileForEdit(nil, "", binPath); err == nil ||
		!strings.Contains(err.Error(), "binary") {
		t.Fatalf("expected binary refusal, got %v", err)
	}

	// Oversize file.
	bigPath := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(bigPath, bytes.Repeat([]byte("x"), maxEditBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFileForEdit(nil, "", bigPath); err == nil ||
		!strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected size refusal, got %v", err)
	}
}

func TestIsBinary(t *testing.T) {
	t.Parallel()
	if isBinary([]byte("plain text\nwith lines\n")) {
		t.Fatal("text should not be flagged binary")
	}
	if !isBinary([]byte{'o', 'k', 0x00, 'x'}) {
		t.Fatal("NUL byte should flag binary")
	}
	if isBinary(nil) {
		t.Fatal("empty content should not be flagged binary")
	}
}

// TestHighlightLines confirms the chroma-backed highlighter splits content into
// the right number of lines and emits ANSI styling for recognised source.
func TestHighlightLines(t *testing.T) {
	// Force a color profile so lipgloss emits ANSI even with no TTY (tests run
	// with a non-terminal stdout, which would otherwise strip styling). Not
	// parallel: SetColorProfile is process-global.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	src := "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n"
	lines := highlightLines("main.go", src)
	// Trailing newline yields an empty final element: 6 lines total.
	if got := len(lines); got != 6 {
		t.Fatalf("got %d lines, want 6", got)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "\x1b[") {
		t.Fatal("expected ANSI escapes in highlighted Go source")
	}
	if !strings.Contains(stripANSI(joined), "func main") {
		t.Fatal("highlighted output should preserve the source text")
	}

	// Unknown extension still returns content (via the fallback lexer/plain path).
	plain := highlightLines("data.unknownext", "just some text\n")
	if len(plain) == 0 || !strings.Contains(stripANSI(strings.Join(plain, "\n")), "just some text") {
		t.Fatal("fallback highlighting should preserve text")
	}
}

// TestSortItems checks dirs-first ordering plus each sort key and direction.
func TestSortItems(t *testing.T) {
	t.Parallel()
	base := func() []fileItem {
		t0 := time.Unix(1000, 0)
		return []fileItem{
			{name: "zeta", isDir: true, modTime: t0.Add(5 * time.Second)},
			{name: "alpha", isDir: true, modTime: t0},
			{name: "big.bin", size: 900, modTime: t0.Add(3 * time.Second)},
			{name: "small.txt", size: 10, modTime: t0.Add(1 * time.Second)},
			{name: "mid.dat", size: 100, modTime: t0.Add(2 * time.Second)},
		}
	}

	// Name ascending: dirs (alpha, zeta) then files by name.
	items := base()
	sortItems(items, sortName, false)
	wantNames := []string{"alpha", "zeta", "big.bin", "mid.dat", "small.txt"}
	for i, w := range wantNames {
		if items[i].name != w {
			t.Fatalf("name-asc[%d]=%s want %s", i, items[i].name, w)
		}
	}

	// Size ascending: dirs first, files by ascending size.
	items = base()
	sortItems(items, sortSize, false)
	wantSize := []string{"alpha", "zeta", "small.txt", "mid.dat", "big.bin"}
	for i, w := range wantSize {
		if items[i].name != w {
			t.Fatalf("size-asc[%d]=%s want %s", i, items[i].name, w)
		}
	}

	// Size descending: dirs STILL first (direction never reorders the dir block
	// ahead of files), files by descending size.
	items = base()
	sortItems(items, sortSize, true)
	if items[0].isDir != true || items[1].isDir != true {
		t.Fatal("dirs must stay first regardless of direction")
	}
	if items[2].name != "big.bin" {
		t.Fatalf("size-desc first file = %s, want big.bin", items[2].name)
	}

	// Modified ascending: files ordered by modTime.
	items = base()
	sortItems(items, sortModified, false)
	wantMod := []string{"alpha", "zeta", "small.txt", "mid.dat", "big.bin"}
	for i, w := range wantMod {
		if items[i].name != w {
			t.Fatalf("mod-asc[%d]=%s want %s", i, items[i].name, w)
		}
	}
}

// TestReapplyPaneFilter verifies the per-pane name filter narrows the listing
// case-insensitively while keeping ".." and the index in range.
func TestReapplyPaneFilter(t *testing.T) {
	t.Parallel()
	m := filesModel{
		left: paneState{
			source: "", cwd: "/home/op/project",
			allItems: []fileItem{
				{name: "README.md"},
				{name: "main.go"},
				{name: "main_test.go"},
				{name: "build", isDir: true},
			},
			selected: map[int]bool{},
		},
	}

	// No filter: dirs first, all items, plus ".." (not at root).
	m.reapplyPane(0)
	if m.left.entries[0].name != ".." {
		t.Fatalf("expected '..' first, got %s", m.left.entries[0].name)
	}
	if countReal(m.left.entries) != 4 {
		t.Fatalf("no-filter visible = %d, want 4", countReal(m.left.entries))
	}

	// Filter "MAIN" (case-insensitive) narrows to the two main* files.
	m.left.filter = "MAIN"
	m.reapplyPane(0)
	got := []string{}
	for _, it := range m.left.entries {
		if it.name != ".." {
			got = append(got, it.name)
		}
	}
	if len(got) != 2 || got[0] != "main.go" || got[1] != "main_test.go" {
		t.Fatalf("filtered names = %v, want [main.go main_test.go]", got)
	}

	// Clearing the filter restores everything.
	m.left.filter = ""
	m.reapplyPane(0)
	if countReal(m.left.entries) != 4 {
		t.Fatalf("post-clear visible = %d, want 4", countReal(m.left.entries))
	}
}

// TestCycleSort walks the sort key/direction cycle driven by the `o` key.
func TestCycleSort(t *testing.T) {
	t.Parallel()
	m := filesModel{
		left: paneState{source: "", cwd: "/", selected: map[int]bool{}},
	}
	if m.left.sortBy != sortName || m.left.sortDesc {
		t.Fatal("default sort should be Name ascending")
	}
	m = m.cycleSort(0)
	if m.left.sortBy != sortSize {
		t.Fatalf("after one cycle = %v, want Size", m.left.sortBy)
	}
	m = m.cycleSort(0)
	if m.left.sortBy != sortModified {
		t.Fatalf("after two cycles = %v, want Modified", m.left.sortBy)
	}
	m = m.cycleSort(0)
	if m.left.sortBy != sortName || !m.left.sortDesc {
		t.Fatalf("after full cycle want Name desc, got %v desc=%v", m.left.sortBy, m.left.sortDesc)
	}
}

// stripANSI removes ANSI escape sequences for plain-text assertions.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if r == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
