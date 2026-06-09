// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"os"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"
)

func TestMain(m *testing.M) {
	// bubblezone's global manager must exist before any View() that marks zones.
	zone.NewGlobal()
	os.Exit(m.Run())
}

func sampleFilesModel(w, h int) filesModel {
	return filesModel{
		width:      w,
		height:     h,
		focus:      0,
		hoverSide:  -1,
		hoverIndex: -1,
		chans:      map[int]*transferChans{},
		left: paneState{
			source: "",
			cwd:    "/home/op/project",
			entries: []fileItem{
				{name: "..", isDir: true},
				{name: "build", isDir: true},
				{name: "app.tar.gz", size: 12 << 20},
				{name: "notes.txt", size: 2048},
			},
			index:    2,
			selected: map[int]bool{},
		},
		right: paneState{
			source: "web-01",
			remote: true,
			cwd:    "/var",
			entries: []fileItem{
				{name: "log", isDir: true},
				{name: "backup.sql", size: 88 << 20},
			},
			selected: map[int]bool{},
		},
		transfers: []*transferRow{
			{id: 1, label: "↑ app.tar.gz", bytesDone: 8 << 20, total: 12 << 20, rate: 9.6e6, streams: 4},
		},
	}
}

func TestFilesViewRenders(t *testing.T) {
	t.Parallel()
	out := sampleFilesModel(120, 40).View()
	for _, want := range []string{
		"Cenvero Fleet", // header brand
		"Local",         // local pane source label
		"web-01",        // remote pane source label
		"app.tar.gz",    // a file row
		"build",         // a dir row
		"Transfers",     // progress dock
		"copy/move",     // footer hint label
		"%",             // progress percentage
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered view missing %q", want)
		}
	}
}

func TestFilesViewSmallTerminalNoPanic(t *testing.T) {
	t.Parallel()
	// Must not panic on a cramped terminal (guards width/Repeat math).
	for _, dim := range [][2]int{{40, 10}, {30, 8}, {80, 24}, {200, 60}} {
		_ = sampleFilesModel(dim[0], dim[1]).View()
	}
}

func TestSmoothBarBounds(t *testing.T) {
	t.Parallel()
	for _, pct := range []int{-10, 0, 1, 37, 50, 99, 100, 150} {
		_ = smoothBar(pct, 22) // must not panic for any input
	}
}

func TestPaneLabel(t *testing.T) {
	t.Parallel()
	if (paneState{source: ""}).label() != "Local" {
		t.Fatal("empty source should label as Local")
	}
	if (paneState{source: "web-01"}).label() != "web-01" {
		t.Fatal("server source should label as its name")
	}
}

func TestResolveSources(t *testing.T) {
	t.Parallel()
	one := []core.ServerRecord{{Name: "a"}}
	none := []core.ServerRecord(nil)

	// No args: Local | first server.
	if l, r := resolveSources(nil, one); l != "" || r != "a" {
		t.Fatalf("no-args = %q|%q, want \"\"|\"a\"", l, r)
	}
	// One server arg must NOT duplicate onto both panes: Local | a.
	if l, r := resolveSources([]string{"a"}, one); l != "" || r != "a" {
		t.Fatalf("one-arg = %q|%q, want \"\"|\"a\"", l, r)
	}
	// Two args: a | b.
	if l, r := resolveSources([]string{"a", "b"}, one); l != "a" || r != "b" {
		t.Fatalf("two-args = %q|%q, want \"a\"|\"b\"", l, r)
	}
	// No servers, no args: Local | Local (only acceptable same-source case).
	if l, r := resolveSources(nil, none); l != "" || r != "" {
		t.Fatalf("no-servers = %q|%q, want \"\"|\"\"", l, r)
	}
}

func TestValidateServerArgs(t *testing.T) {
	t.Parallel()
	available := []core.ServerRecord{{Name: "web-01"}, {Name: "db-01"}}

	// Local ("") is always valid, as are known server names.
	for _, args := range [][]string{
		nil,
		{""},
		{"web-01"},
		{"", "db-01"},
		{"web-01", "db-01"},
	} {
		if err := validateServerArgs(args, available); err != nil {
			t.Fatalf("validateServerArgs(%v) unexpected error: %v", args, err)
		}
	}

	// An unknown server must error and must not be matched case-insensitively.
	for _, args := range [][]string{
		{"nope"},
		{"WEB-01"},          // case-sensitive: not the same as web-01
		{"web-01", "ghost"}, // second arg unknown
	} {
		err := validateServerArgs(args, available)
		if err == nil {
			t.Fatalf("validateServerArgs(%v) expected error, got nil", args)
		}
		if !strings.Contains(err.Error(), "unknown server") {
			t.Fatalf("error %q should mention 'unknown server'", err)
		}
		// Error lists the known servers to help the user.
		if !strings.Contains(err.Error(), "web-01") || !strings.Contains(err.Error(), "db-01") {
			t.Fatalf("error %q should list known servers", err)
		}
	}
}

// TestListRowsAreSingleLine guards the spacing regression: each list row must
// render as exactly one terminal line, exactly the pane content width wide, so
// rows stay contiguous and never soft-wrap into blank-gap pairs inside the box.
func TestListRowsAreSingleLine(t *testing.T) {
	t.Parallel()
	m := sampleFilesModel(120, 40)
	// Cover several content widths, including the cramped clamp path.
	for _, cw := range []int{26, 40, 50, 56} {
		for i := range m.left.entries {
			row := m.renderRow(0, i, cw, false)
			if h := lipgloss.Height(row); h != 1 {
				t.Fatalf("cw=%d row %d height=%d, want 1 (would wrap/pair)", cw, i, h)
			}
			if w := lipgloss.Width(row); w != cw {
				t.Fatalf("cw=%d row %d width=%d, want exactly cw", cw, i, w)
			}
		}
	}
}

func TestTransferGlyph(t *testing.T) {
	t.Parallel()
	cases := []struct {
		srcRemote, dstRemote bool
		kind                 dirTransferKind
		want                 string
	}{
		{false, true, dtCopy, "↑"},
		{true, false, dtCopy, "↓"},
		{true, true, dtMove, "↦"},
		{true, true, dtCopy, "⇒"},
	}
	for _, c := range cases {
		if got := transferGlyph(c.srcRemote, c.dstRemote, c.kind); got != c.want {
			t.Fatalf("transferGlyph(%v,%v,%v)=%q want %q", c.srcRemote, c.dstRemote, c.kind, got, c.want)
		}
	}
}

func TestGridViewRenders(t *testing.T) {
	t.Parallel()
	m := sampleFilesModel(120, 40)
	m.left.view = viewGrid
	out := m.View()
	for _, want := range []string{
		"build",       // a dir cell name
		"app.tar.gz",  // a file cell name
		"View: Icons", // toolbar reflects focused pane's current (grid) layout
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("grid view missing %q", want)
		}
	}
}

func TestGridViewCycleAndNav(t *testing.T) {
	t.Parallel()
	m := sampleFilesModel(120, 40)
	// Cycle the focused (left) pane into grid view.
	m.cycleView(0)
	if m.left.view != viewGrid {
		t.Fatalf("cycleView should switch left pane to grid")
	}
	// vStep in grid equals the column count (>=1); list pane stays 1.
	if m.vStep(0) < 1 {
		t.Fatalf("grid vStep should be >= 1, got %d", m.vStep(0))
	}
	if m.vStep(1) != 1 {
		t.Fatalf("list vStep should be 1, got %d", m.vStep(1))
	}
	// Navigation must clamp without panic across the whole range.
	for range 50 {
		m.movePane(0, m.vStep(0))
	}
	if m.left.index >= len(m.left.entries) || m.left.index < 0 {
		t.Fatalf("grid nav index out of range: %d", m.left.index)
	}
	// Scroll stays aligned to a grid-row boundary (a multiple of cols).
	cols, _ := m.gridDims()
	if m.left.scroll%cols != 0 {
		t.Fatalf("grid scroll %d not aligned to cols %d", m.left.scroll, cols)
	}
	// Cycle back to list.
	m.cycleView(0)
	if m.left.view != viewList {
		t.Fatalf("cycleView should return left pane to list")
	}
}

func TestGridViewNarrowAndEmptyNoPanic(t *testing.T) {
	t.Parallel()
	// Narrow pane forces a single column; empty dir must not panic either.
	for _, dim := range [][2]int{{30, 8}, {40, 10}, {200, 60}} {
		m := sampleFilesModel(dim[0], dim[1])
		m.left.view = viewGrid
		m.right.view = viewGrid
		_ = m.View()
		// Empty grid pane.
		m.left.entries = nil
		m.left.index, m.left.scroll = 0, 0
		_ = m.View()
		m.movePane(0, 1) // no-op on empty, must not panic
	}
}

func TestOverlayRendersCentered(t *testing.T) {
	t.Parallel()
	m := sampleFilesModel(120, 40)
	m.overlay = overlaySourcePicker
	m.pickerSide = 0
	m.pickerItems = []string{"Local", "web-01"}
	out := m.View()
	if !strings.Contains(out, "Open source") {
		t.Fatal("source picker overlay should render its title")
	}
}
