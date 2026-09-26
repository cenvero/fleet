// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/core"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// ---- helpers ----

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEscape}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "shift+down":
		return tea.KeyMsg{Type: tea.KeyShiftDown}
	case "shift+up":
		return tea.KeyMsg{Type: tea.KeyShiftUp}
	case "end":
		return tea.KeyMsg{Type: tea.KeyEnd}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "ctrl+a":
		return tea.KeyMsg{Type: tea.KeyCtrlA}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func press(t *testing.T, m filesModel, keys ...string) filesModel {
	t.Helper()
	for _, k := range keys {
		mm, _ := m.Update(key(k))
		m = mm.(filesModel)
	}
	return m
}

func frameLines(s string) []string { return strings.Split(s, "\n") }

// bigModel is a model with n plain files in the left pane.
func bigModel(n, w, h int) filesModel {
	m := benchModel(n)
	m.width, m.height = w, h
	m.frames = &frameCache{}
	return m
}

// ---- layout ----

// TestToolbarNeverWraps: the action strip must be exactly one line at every
// width, collapsing into "≡ More" instead of wrapping.
func TestToolbarNeverWraps(t *testing.T) {
	t.Parallel()
	for _, w := range []int{60, 80, 100, 120, 160, 200, 260} {
		m := sampleFilesModel(w, 40)
		l := m.layout()
		line := m.renderToolbarLine(l, fmPal())
		if strings.Contains(line, "\n") {
			t.Fatalf("w=%d: toolbar contains a newline", w)
		}
		if got := lipgloss.Width(line); got != w {
			t.Fatalf("w=%d: toolbar width %d", w, got)
		}
		fit := m.toolbarLayout(l.innerW)
		if fit.width > l.innerW {
			t.Fatalf("w=%d: toolbar content %d wider than %d", w, fit.width, l.innerW)
		}
		plain := stripANSI(line)
		for _, must := range []string{"c Copy", "m Move", "? Help", "q Quit"} {
			if w >= 100 && !strings.Contains(plain, must) {
				t.Fatalf("w=%d: toolbar lost %q: %q", w, must, plain)
			}
		}
		if len(fit.overflow) > 0 && !strings.Contains(plain, "More") && !fit.keysOnly {
			t.Fatalf("w=%d: overflowed buttons but no More button: %q", w, plain)
		}
	}
	// Wide terminals show everything without a More menu.
	m := sampleFilesModel(260, 40)
	if fit := m.toolbarLayout(m.layout().innerW); len(fit.overflow) != 0 {
		t.Fatalf("260 cols should fit every button, overflow=%v", fit.overflow)
	}
}

// TestFrameIsExactlyTheTerminal checks every frame is exactly width×height so
// nothing wraps or scrolls, at small, default and huge sizes, with and without
// the preview and the transfer queue.
func TestFrameIsExactlyTheTerminal(t *testing.T) {
	t.Parallel()
	for _, dim := range [][2]int{{80, 24}, {100, 30}, {120, 40}, {160, 45}, {220, 60}, {50, 16}, {40, 12}} {
		for _, preview := range []bool{false, true} {
			m := sampleFilesModel(dim[0], dim[1])
			m.preview.on = preview
			out := m.View()
			lines := frameLines(out)
			if len(lines) != dim[1] {
				t.Fatalf("%v preview=%v: %d lines, want %d", dim, preview, len(lines), dim[1])
			}
			for i, ln := range lines {
				if w := lipgloss.Width(ln); w != dim[0] {
					t.Fatalf("%v preview=%v: line %d width %d, want %d: %q", dim, preview, i, w, dim[0], stripANSI(ln))
				}
			}
		}
	}
}

func TestNarrowTerminalShowsOnePane(t *testing.T) {
	t.Parallel()
	m := sampleFilesModel(50, 20)
	l := m.layout()
	if !l.single || !l.paneShown[0] || l.paneShown[1] {
		t.Fatalf("50 cols should show only the focused pane: %+v", l)
	}
	m = press(t, m, "tab")
	l = m.layout()
	if !l.paneShown[1] || l.paneShown[0] {
		t.Fatal("tab should flip the visible pane in single-pane mode")
	}
	if out := m.View(); !strings.Contains(out, "web-01") {
		t.Fatal("right pane should be visible after tab")
	}
}

func TestTinyTerminalMessage(t *testing.T) {
	t.Parallel()
	out := sampleFilesModel(25, 8).View()
	if !strings.Contains(out, "too small") {
		t.Fatalf("tiny terminal should explain itself: %q", stripANSI(out))
	}
}

// ---- windowing / performance-shaped behaviour ----

func TestWindowingRendersOnlyVisibleRows(t *testing.T) {
	t.Parallel()
	m := bigModel(50000, 160, 45)
	m.right.allItems, m.right.entries = nil, nil
	rows := m.visibleRows()
	lines := m.renderListLines(0, 60, rows, false)
	if len(lines) != rows {
		t.Fatalf("list body = %d lines, want %d", len(lines), rows)
	}
	first := m.left.entries[1].name
	lastName := m.left.entries[len(m.left.entries)-1].name
	out := m.View()
	if !strings.Contains(out, first) || strings.Contains(out, lastName) {
		t.Fatal("top of the listing expected")
	}
	m = press(t, m, "end")
	out = m.View()
	if !strings.Contains(out, lastName) || strings.Contains(out, first) {
		t.Fatalf("bottom of the listing expected after End (first=%q last=%q):\n%s", first, lastName, stripANSI(out))
	}
	if !strings.Contains(stripANSI(out), "50,000/50,000") {
		t.Fatalf("position indicator missing: %q", stripANSI(out))
	}
}

func TestMouseMotionWithinRowReusesFrame(t *testing.T) {
	t.Parallel()
	m := bigModel(1000, 160, 45)
	first := m.View()
	mm, _ := m.Update(tea.MouseMsg{X: 20, Y: 10, Action: tea.MouseActionMotion})
	m = mm.(filesModel)
	second := m.View()
	mm, _ = m.Update(tea.MouseMsg{X: 25, Y: 10, Action: tea.MouseActionMotion})
	m = mm.(filesModel)
	third := m.View()
	if second != third {
		t.Fatal("moving within the same row must reuse the cached frame")
	}
	_ = first
	// The hover row resolves arithmetically to the entry under the pointer.
	l := m.layout()
	if m.hoverSide != 0 || m.hoverIndex != m.left.scroll+(10-l.bodyY) {
		t.Fatalf("hover = %d/%d", m.hoverSide, m.hoverIndex)
	}
}

func TestHitTestRegions(t *testing.T) {
	t.Parallel()
	m := sampleFilesModel(160, 45)
	l := m.layout()
	// Rows.
	h := m.hitTest(l.paneX[1]+5, l.bodyY+1)
	if h.kind != fmHitRow || h.side != 1 || h.index != 1 {
		t.Fatalf("row hit = %+v", h)
	}
	// Blank space below the entries is inside the pane but not on a row.
	if side, idx, ok := m.hitRow(tea.MouseMsg{X: l.paneX[1] + 5, Y: l.bodyY + 20}); !ok || side != 1 || idx != -1 {
		t.Fatalf("blank hit = %d %d %v", side, idx, ok)
	}
	// Toolbar button.
	fit := m.toolbarLayout(l.innerW)
	sp := fit.spans[0]
	if h := m.hitTest(l.padX+sp.x0+1, l.toolbarY); h.kind != fmHitToolbar || h.action != sp.action {
		t.Fatalf("toolbar hit = %+v want %s", h, sp.action)
	}
	// Pane title opens the source picker.
	mm, _ := m.Update(tea.MouseMsg{X: l.paneX[0] + 4, Y: l.panesY, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if got := mm.(filesModel); got.overlay != overlaySourcePicker {
		t.Fatalf("title click overlay = %v", got.overlay)
	}
	// Column header sorts.
	mm, _ = m.Update(tea.MouseMsg{X: l.paneX[0] + 1 + 5 + 1, Y: l.panesY + 2, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if got := mm.(filesModel); got.left.sortBy != sortName || !got.left.sortDesc {
		t.Fatalf("clicking the active Name column should flip direction: %v %v", got.left.sortBy, got.left.sortDesc)
	}
}

func TestCrumbClickNavigates(t *testing.T) {
	t.Parallel()
	m := sampleFilesModel(160, 45)
	m.left.pathStyle = core.TargetPathPOSIX
	m.left.root = "/"
	l := m.layout()
	_, spans := m.crumbLayout(0, l.contentW(0), fmPal())
	var target crumbSpan
	for _, sp := range spans {
		if sp.path == "/home" {
			target = sp
		}
	}
	if target.path == "" {
		t.Fatalf("no /home crumb in %+v", spans)
	}
	mm, _ := m.Update(tea.MouseMsg{X: l.paneX[0] + 1 + target.x0, Y: l.panesY + 1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	got := mm.(filesModel)
	if got.left.cwd != "/home" || got.left.focusName != "op" {
		t.Fatalf("crumb click: cwd=%q focus=%q", got.left.cwd, got.left.focusName)
	}
	if len(got.hist[0].back) != 1 || got.hist[0].back[0].Path != "/home/op/project" {
		t.Fatalf("crumb navigation should be recorded in history: %+v", got.hist[0])
	}
}

// ---- unicode, truncation, sanitising ----

func TestWideNamesKeepRowsSingleLine(t *testing.T) {
	t.Parallel()
	m := sampleFilesModel(120, 40)
	m.left.entries = []fileItem{
		{name: "日本語のファイル名がとても長い場合のテスト.txt", size: 10},
		{name: "emoji-🚀-rocket-with-a-long-tail-name.md"},
		{name: "mixed 中文 and English and a very long tail to force truncation.txt"},
		{name: "café-naïve-combining-e\u0301.txt"},
		{name: strings.Repeat("x", 300) + ".log"},
		{name: "日本語フォルダ", isDir: true},
	}
	for _, cw := range []int{20, 26, 40, 56, 90, 120} {
		for i := range m.left.entries {
			row := m.renderRow(0, i, cw, false)
			if h := lipgloss.Height(row); h != 1 {
				t.Fatalf("cw=%d row %d height %d", cw, i, h)
			}
			if w := lipgloss.Width(row); w != cw {
				t.Fatalf("cw=%d row %d width %d: %q", cw, i, w, stripANSI(row))
			}
		}
	}
	// Truncation keeps the extension visible.
	if got := fmFitName("a-very-long-file-name-that-keeps-going.log", 20); !strings.HasSuffix(got, ".log") || fmWidth(got) > 20 {
		t.Fatalf("fmFitName = %q", got)
	}
	if got := fmFitName("日本語のファイル名がとても長い.txt", 12); fmWidth(got) > 12 || !strings.Contains(got, "…") {
		t.Fatalf("wide fmFitName = %q (%d)", got, fmWidth(got))
	}
}

// TestHostileNamesCannotInjectEscapes: file names come from disk or from a
// remote agent; control characters must be rendered harmlessly.
func TestHostileNamesCannotInjectEscapes(t *testing.T) {
	t.Parallel()
	m := sampleFilesModel(120, 40)
	evil := "evil\x1b[2J\x1b]52;c;cHduZWQ=\x07name\u202etxt.exe"
	m.left.entries = append(m.left.entries, fileItem{name: evil})
	m.left.index = len(m.left.entries) - 1
	out := stripANSI(m.View())
	for _, bad := range []string{"\x1b[2J", "\x1b]52", "\x07", "\u202e"} {
		if strings.Contains(out, bad) {
			t.Fatalf("raw control sequence %q reached the frame", bad)
		}
	}
	if !strings.Contains(out, "evil␛[2J") {
		t.Fatal("control characters should be shown as visible placeholders")
	}
	if got := fmSanitize("ok\tname"); got != "ok name" {
		t.Fatalf("tab sanitised to %q", got)
	}
}

// ---- selection ----

func TestRangeSelectionAndSelectAll(t *testing.T) {
	t.Parallel()
	m := sampleFilesModel(120, 40)
	m.left.index = 1 // build
	m = press(t, m, "shift+down", "shift+down")
	if len(m.left.selected) != 3 || !m.left.selected[1] || !m.left.selected[3] {
		t.Fatalf("shift range = %v", m.left.selected)
	}
	m = press(t, m, "shift+up")
	if len(m.left.selected) != 2 || m.left.selected[3] {
		t.Fatalf("shrinking the range should deselect: %v", m.left.selected)
	}
	m = press(t, m, "esc")
	if len(m.left.selected) != 0 {
		t.Fatal("esc should clear the selection")
	}
	m = press(t, m, "ctrl+a")
	if len(m.left.selected) != 3 || m.left.selected[0] {
		t.Fatalf("select all must skip '..': %v", m.left.selected)
	}
	m = press(t, m, "*")
	if len(m.left.selected) != 0 {
		t.Fatalf("invert of all = %v", m.left.selected)
	}
	// V mode: movement extends from the anchor.
	m.left.index = 1
	m = press(t, m, "V", "down", "down")
	if len(m.left.selected) != 3 {
		t.Fatalf("visual range = %v", m.left.selected)
	}
	m = press(t, m, "V")
	if m.left.visual {
		t.Fatal("V should end range mode")
	}
	if st := m.selectionStats(0); st.count != 3 || st.dirs != 1 || st.bytes != 12<<20+2048 {
		t.Fatalf("selection stats = %+v", st)
	}
}

// TestSelectionFollowsEntriesAcrossResort is a regression test: selection used
// to be keyed by row index only, so re-sorting silently moved it onto other
// files (and a later delete would hit the wrong ones).
func TestSelectionFollowsEntriesAcrossResort(t *testing.T) {
	t.Parallel()
	m := filesModel{left: paneState{
		pathStyle: core.TargetPathPOSIX, root: "/", cwd: "/data",
		allItems: []fileItem{
			{name: "a.txt", size: 300}, {name: "b.txt", size: 100}, {name: "c.txt", size: 200},
		},
		selected: map[int]bool{},
	}}
	m.reapplyPane(0) // .. a b c
	m.left.selected = map[int]bool{1: true}
	m.left.index = 3   // c.txt
	m = m.cycleSort(0) // by size: .. b c a
	items := m.selectionItems(0)
	if len(items) != 1 || items[0].name != "a.txt" {
		t.Fatalf("selection after re-sort = %+v", items)
	}
	if m.focusedItem(0).name != "c.txt" {
		t.Fatalf("cursor after re-sort on %q", m.focusedItem(0).name)
	}
}

// TestRefreshKeepsSelectionAndFilter: a reload of the same folder (e.g. after
// a transfer finishes) must not wipe what the user is doing.
func TestRefreshKeepsSelectionAndFilter(t *testing.T) {
	t.Parallel()
	m := filesModel{left: paneState{
		pathStyle: core.TargetPathPOSIX, root: "/", cwd: "/data", listedCwd: "/data",
		allItems: []fileItem{{name: "a.log"}, {name: "b.log"}, {name: "c.txt"}},
		selected: map[int]bool{}, filter: "log",
	}}
	m.reapplyPane(0)
	m.left.selected = map[int]bool{2: true} // b.log
	msg := paneLoadedMsg{side: 0, source: "", cwd: "/data", req: "/data",
		items: []fileItem{{name: "a.log"}, {name: "b.log"}, {name: "c.txt"}, {name: "d.log"}}}
	mm, _ := m.update(msg)
	got := mm.(filesModel)
	if got.left.filter != "log" {
		t.Fatal("refresh dropped the filter")
	}
	items := got.selectionItems(0)
	if len(items) != 1 || items[0].name != "b.log" {
		t.Fatalf("refresh changed the selection: %+v", items)
	}
	// Navigating elsewhere resets both.
	got.left.cwd = "/other"
	mm, _ = got.update(paneLoadedMsg{side: 0, source: "", cwd: "/other", req: "/other", items: []fileItem{{name: "x"}}})
	got = mm.(filesModel)
	if got.left.filter != "" || len(got.left.selected) != 0 {
		t.Fatal("a new folder should start without filter/selection")
	}
	// A late listing for a folder we already left is ignored.
	mm, _ = got.update(paneLoadedMsg{side: 0, source: "", cwd: "/data", req: "/data", items: []fileItem{{name: "zzz"}}})
	if mm.(filesModel).left.cwd != "/other" {
		t.Fatal("stale listing replaced the current folder")
	}
}

func TestParentNavigationFocusesPreviousFolder(t *testing.T) {
	t.Parallel()
	m := filesModel{left: paneState{pathStyle: core.TargetPathPOSIX, root: "/", cwd: "/srv/app/logs", listedCwd: "/srv/app/logs", selected: map[int]bool{}}}
	mm, _ := m.enterParent(0)
	m = mm.(filesModel)
	mm, _ = m.update(paneLoadedMsg{side: 0, cwd: "/srv/app", req: "/srv/app",
		items: []fileItem{{name: "bin", isDir: true}, {name: "logs", isDir: true}, {name: "x"}}})
	m = mm.(filesModel)
	if m.focusedItem(0).name != "logs" {
		t.Fatalf("cursor on %q, want logs", m.focusedItem(0).name)
	}
	// Back returns to where we were; forward goes up again.
	mm, _ = m.historyBack(0)
	m = mm.(filesModel)
	if m.left.cwd != "/srv/app/logs" {
		t.Fatalf("back → %q", m.left.cwd)
	}
	mm, _ = m.historyForward(0)
	if got := mm.(filesModel).left.cwd; got != "/srv/app" {
		t.Fatalf("forward → %q", got)
	}
}

// ---- overlays ----

func TestHelpOverlay(t *testing.T) {
	t.Parallel()
	for _, dim := range [][2]int{{80, 24}, {160, 45}, {220, 60}} {
		m := sampleFilesModel(dim[0], dim[1])
		m = press(t, m, "?")
		if m.overlay != overlayHelp {
			t.Fatal("? should open help")
		}
		out := m.View()
		plain := stripANSI(out)
		if !strings.Contains(plain, "Keyboard & mouse") || !strings.Contains(plain, "Navigate") {
			t.Fatalf("%v: help missing content", dim)
		}
		if n := len(frameLines(out)); n != dim[1] {
			t.Fatalf("%v: help frame has %d lines", dim, n)
		}
		for i := 0; i < 50; i++ {
			m = press(t, m, "down")
		}
		_ = m.View()
		m = press(t, m, "esc")
		if m.overlay != overlayNone {
			t.Fatal("esc should close help")
		}
	}
	// Every group title is reachable when scrolling at 80x24.
	m := sampleFilesModel(80, 24)
	width, _ := m.helpViewport()
	all := stripANSI(strings.Join(m.helpLines(width), "\n"))
	for _, g := range fmHelpGroups() {
		if !strings.Contains(all, g.title) {
			t.Fatalf("help lost group %q", g.title)
		}
	}
}

func TestJumpFuzzyFindsAndOpens(t *testing.T) {
	t.Parallel()
	m := filesModel{width: 120, height: 40, left: paneState{
		pathStyle: core.TargetPathPOSIX, root: "/", cwd: "/p",
		allItems: []fileItem{{name: "README.md"}, {name: "main.go"}, {name: "main_test.go"},
			{name: "internal", isDir: true}, {name: "go.mod"}},
		selected: map[int]bool{},
	}}
	m.reapplyPane(0)
	m = m.openJump(0)
	for _, r := range "mgo" {
		mm, _ := m.Update(key(string(r)))
		m = mm.(filesModel)
	}
	if len(m.jumpResults) == 0 || m.left.entries[m.jumpResults[0]].name != "main.go" {
		t.Fatalf("best match for mgo = %v", m.jumpResults)
	}
	mm, _ := m.Update(key("enter"))
	m = mm.(filesModel)
	if m.overlay != overlayNone || m.focusedItem(0).name != "main.go" {
		t.Fatalf("jump landed on %q", m.focusedItem(0).name)
	}
	if s, ok := fmFuzzyScore("xyz", "main.go"); ok || s != 0 {
		t.Fatal("non-subsequence must not match")
	}
}

func TestGotoResolvesAndValidates(t *testing.T) {
	t.Parallel()
	m := filesModel{width: 120, height: 40, right: paneState{
		source: "web-01", remote: true, pathStyle: core.TargetPathPOSIX,
		root: "/srv/allowed", home: "/srv/allowed/home", cwd: "/srv/allowed/app", selected: map[int]bool{},
		allItems: []fileItem{{name: "logs", isDir: true}, {name: "lib", isDir: true}, {name: "x.txt"}},
	}}
	if got := m.resolveGotoPath(1, "logs"); got != "/srv/allowed/app/logs" {
		t.Fatalf("relative = %q", got)
	}
	if got := m.resolveGotoPath(1, "~/data"); got != "/srv/allowed/home/data" {
		t.Fatalf("home = %q", got)
	}
	if got := m.gotoCompletions(1, "/srv/allowed/app/l"); len(got) != 2 {
		t.Fatalf("remote completions from listing = %v", got)
	}
	m = m.openGoto(1)
	m.gotoValue = "/etc"
	mm, cmd := m.submitGoto()
	got := mm.(filesModel)
	if cmd != nil || got.overlay != overlayGoto || !strings.Contains(got.gotoErr, "outside") {
		t.Fatalf("path outside the root must be refused in the dialog: %q", got.gotoErr)
	}
	got.gotoValue = "../app/logs"
	mm, cmd = got.submitGoto()
	got = mm.(filesModel)
	if cmd == nil || got.right.cwd != "/srv/allowed/app/logs" {
		t.Fatalf("goto → %q", got.right.cwd)
	}
}

func TestLocalGotoCompletesAndFocusesFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, d := range []string{"alpha", "alpine", "beta"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "beta", "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	style := core.NativePathStyle()
	m := filesModel{width: 120, height: 40, left: paneState{pathStyle: style, root: style.DefaultRoot(), cwd: dir, selected: map[int]bool{}}}
	m = m.openGoto(0)
	m.gotoValue = filepath.Join(dir, "al")
	m.gotoSugg = m.gotoCompletions(0, m.gotoValue)
	if len(m.gotoSugg) != 2 {
		t.Fatalf("completions = %v", m.gotoSugg)
	}
	mm, _ := m.handleGotoKey(key("tab"))
	m = mm.(filesModel)
	if m.gotoValue != filepath.Join(dir, "alp") {
		t.Fatalf("tab should complete the common prefix, got %q", m.gotoValue)
	}
	m.gotoValue = filepath.Join(dir, "beta", "f.txt")
	mm, _ = m.submitGoto()
	m = mm.(filesModel)
	if m.left.cwd != filepath.Join(dir, "beta") || m.left.focusName != "f.txt" {
		t.Fatalf("goto file → cwd %q focus %q", m.left.cwd, m.left.focusName)
	}
}

func TestPlacesBookmarksAndRecent(t *testing.T) {
	t.Parallel()
	m := sampleFilesModel(120, 40)
	m.lastPath = map[string]string{}
	m.rememberVisit(0)
	m.rememberVisit(1)
	m = m.toggleBookmark(1)
	if len(m.bookmarks) != 1 || m.bookmarks[0].Source != "web-01" {
		t.Fatalf("bookmarks = %+v", m.bookmarks)
	}
	m = m.openPlaces()
	if len(m.placesItems) != 2 || !m.placesItems[0].bookmark || m.placesItems[0].section != "Bookmarks" {
		t.Fatalf("places = %+v", m.placesItems)
	}
	if out := stripANSI(m.View()); !strings.Contains(out, "Places") || !strings.Contains(out, "★") {
		t.Fatal("places overlay should render bookmarks")
	}
	m = m.toggleBookmark(1) // remove again
	if len(m.bookmarks) != 0 {
		t.Fatal("b on a bookmarked folder should remove it")
	}
	if m.lastPath["web-01"] != "/var" {
		t.Fatalf("last path per source = %v", m.lastPath)
	}
}

func TestBookmarksPersistRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := bookmarksFile(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"bookmarks":[{"source":"web-01","path":"/srv"},{"source":"","path":""}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := loadBookmarks(dir)
	if len(got) != 1 || got[0] != (fmLoc{Source: "web-01", Path: "/srv"}) {
		t.Fatalf("loaded %+v", got)
	}
	if loadBookmarks(filepath.Join(dir, "missing")) != nil {
		t.Fatal("missing file should load nothing")
	}
}

func TestMirrorNavigationFollows(t *testing.T) {
	t.Parallel()
	m := filesModel{width: 120, height: 40,
		left: paneState{pathStyle: core.TargetPathPOSIX, root: "/", cwd: "/a", selected: map[int]bool{},
			entries: []fileItem{{name: "..", isDir: true}, {name: "src", isDir: true}}},
		right: paneState{source: "web-01", remote: true, pathStyle: core.TargetPathPOSIX, root: "/", cwd: "/b", selected: map[int]bool{},
			allItems: []fileItem{{name: "src", isDir: true}}},
	}
	m = press(t, m, "=")
	if !m.mirror {
		t.Fatal("= should enable mirror")
	}
	m.left.index = 1
	mm, _ := m.activate(0)
	m = mm.(filesModel)
	if m.left.cwd != "/a/src" || m.right.cwd != "/b/src" {
		t.Fatalf("mirror enter: %q %q", m.left.cwd, m.right.cwd)
	}
	mm, _ = m.enterParent(0)
	m = mm.(filesModel)
	if m.left.cwd != "/a" || m.right.cwd != "/b" {
		t.Fatalf("mirror parent: %q %q", m.left.cwd, m.right.cwd)
	}
}

// ---- preview ----

func TestPreviewCapsAndKinds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	big := filepath.Join(dir, "big.log")
	var sb strings.Builder
	for sb.Len() < 3*previewMaxBytes {
		sb.WriteString("a line of log text that repeats\n")
	}
	if err := os.WriteFile(big, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(bin, append([]byte("ELF"), make([]byte, 600)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	for i := range previewDirEntries + 20 {
		if err := os.WriteFile(filepath.Join(dir, "sub", fmt.Sprintf("f%03d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	style := core.NativePathStyle()
	pane := paneState{pathStyle: style, cwd: dir}

	st, _ := os.Stat(big)
	d := buildPreview(nil, pane, fileItem{name: "big.log", size: st.Size(), mode: uint32(st.Mode())}, "k")
	if len(d.lines) == 0 || len(d.lines) > previewMaxLines || !strings.Contains(d.note, "first") {
		t.Fatalf("big text preview: %d lines note=%q", len(d.lines), d.note)
	}
	d = buildPreview(nil, pane, fileItem{name: "blob.bin", size: 603}, "k")
	if !strings.Contains(d.note, "binary") || len(d.lines) != 16 {
		t.Fatalf("binary preview: note=%q lines=%d", d.note, len(d.lines))
	}
	d = buildPreview(nil, pane, fileItem{name: "sub", isDir: true}, "k")
	if len(d.lines) != previewDirEntries || !strings.Contains(d.note, "first") {
		t.Fatalf("dir preview: %d lines note=%q", len(d.lines), d.note)
	}
	// Remote previews never download large files.
	d = buildPreview(nil, paneState{source: "web-01", remote: true, pathStyle: core.TargetPathPOSIX, cwd: "/x"},
		fileItem{name: "huge.iso", size: 4 << 30}, "k")
	if !strings.Contains(d.note, "not fetched") {
		t.Fatalf("large remote preview note = %q", d.note)
	}
	// The head buffer stops a streaming reader at its limit.
	var hb headBuffer
	hb.limit = 10
	if n, err := hb.Write([]byte("0123456789abcdef")); n != 10 || err == nil || !hb.full {
		t.Fatalf("headBuffer n=%d err=%v full=%v", n, err, hb.full)
	}
}

func TestPreviewToggleDebouncesAndCaches(t *testing.T) {
	t.Parallel()
	m := sampleFilesModel(160, 45)
	mm, cmd := m.togglePreview()
	m = mm.(filesModel)
	if !m.preview.on || cmd == nil || !m.preview.loading {
		t.Fatal("preview should schedule a debounced load")
	}
	seq := m.preview.seq
	// A stale debounce tick (cursor moved on) does nothing.
	mm, cmd = m.onPreviewDebounce(previewDebounceMsg{seq: seq - 1})
	if cmd != nil {
		t.Fatal("stale debounce should not load")
	}
	m = mm.(filesModel)
	data := &previewData{key: m.preview.key, name: "x"}
	mm, _ = m.onPreviewLoaded(previewLoadedMsg{key: m.preview.key, data: data})
	m = mm.(filesModel)
	if m.preview.loading || m.preview.data != data {
		t.Fatal("loaded preview should be shown")
	}
	// Coming back to the same entry is served from cache without a load.
	k := m.preview.key
	m.preview.key = "other"
	if c := m.syncPreview(); c != nil || m.preview.key != k || m.preview.data != data {
		t.Fatal("cached preview should be reused")
	}
}

// ---- transfers ----

// runCmd executes a command (and nested batches) with a timeout, returning
// the messages produced. Ticks longer than the timeout are skipped.
func runCmd(cmd tea.Cmd, timeout time.Duration) []tea.Msg {
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case msg := <-ch:
		if b, ok := msg.(tea.BatchMsg); ok {
			var out []tea.Msg
			for _, c := range b {
				out = append(out, runCmd(c, timeout)...)
			}
			return out
		}
		return []tea.Msg{msg}
	case <-time.After(timeout):
		return nil
	}
}

// pump feeds messages back into the model until transfers settle.
func pumpUntilIdle(t *testing.T, m filesModel, cmd tea.Cmd) filesModel {
	t.Helper()
	queue := []tea.Cmd{cmd}
	deadline := time.Now().Add(10 * time.Second)
	for len(queue) > 0 && time.Now().Before(deadline) && m.transfersActive() {
		c := queue[0]
		queue = queue[1:]
		for _, msg := range runCmd(c, 500*time.Millisecond) {
			switch msg.(type) {
			case progressTickMsg, transferDoneMsg:
				mm, next := m.Update(msg)
				m = mm.(filesModel)
				queue = append(queue, next)
			}
		}
	}
	return m
}

func TestTransferQueueStateMachine(t *testing.T) {
	t.Parallel()
	src, dst := t.TempDir(), t.TempDir()
	var items []fileItem
	for i := range 5 {
		name := fmt.Sprintf("f%d.txt", i)
		if err := os.WriteFile(filepath.Join(src, name), []byte(strings.Repeat("x", 1000*(i+1))), 0o600); err != nil {
			t.Fatal(err)
		}
		items = append(items, fileItem{name: name, size: int64(1000 * (i + 1))})
	}
	style := core.NativePathStyle()
	m := filesModel{
		width: 160, height: 45, chans: map[int]*transferChans{},
		left:  paneState{pathStyle: style, root: style.DefaultRoot(), cwd: src, listedCwd: src, allItems: items, selected: map[int]bool{}},
		right: paneState{pathStyle: style, root: style.DefaultRoot(), cwd: dst, listedCwd: dst, selected: map[int]bool{}},
	}
	m.reapplyPane(0)
	// Keyboard copy opens the plan dialog first.
	m.left.index = 1
	m = press(t, m, "ctrl+a")
	mm, _ := m.copyToOtherPane(0)
	m = mm.(filesModel)
	if m.overlay != overlayConfirm || m.confirm != confirmTransfer || len(m.plan.items) != 5 {
		t.Fatalf("copy should show a plan: overlay=%v plan=%+v", m.overlay, m.plan)
	}
	out := stripANSI(m.View())
	if !strings.Contains(out, "5 files") || !strings.Contains(out, "Copy 5 items") {
		t.Fatalf("plan dialog should list counts: %q", out)
	}
	mm, cmd := m.Update(key("enter"))
	m = mm.(filesModel)
	running, queued := 0, 0
	for _, r := range m.transfers {
		switch r.state() {
		case xferRunning:
			running++
		case xferQueued:
			queued++
		}
	}
	if running != maxActiveTransfers || queued != 5-maxActiveTransfers {
		t.Fatalf("running=%d queued=%d", running, queued)
	}
	// Cancel the last queued one before it starts.
	order := m.transferOrder()
	last := order[len(order)-1]
	if last.state() != xferQueued {
		t.Fatalf("last row state = %v", last.state())
	}
	m.cancelTransfer(last)
	if last.state() != xferCancelled {
		t.Fatal("queued transfer should cancel")
	}
	// A running one reports that it cannot be interrupted.
	m.cancelTransfer(order[0])
	if order[0].state() != xferRunning || !strings.Contains(m.status, "can't be interrupted") {
		t.Fatalf("running cancel: state=%v status=%q", order[0].state(), m.status)
	}
	m = pumpUntilIdle(t, m, cmd)
	done := 0
	for _, r := range m.transfers {
		if r.state() == xferDone {
			done++
		}
	}
	if done != 4 {
		t.Fatalf("done=%d rows=%+v", done, m.transfers)
	}
	if _, err := os.Stat(filepath.Join(dst, last.name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled transfer must not have run")
	}
	// Retry the cancelled row.
	cmd = m.retryTransfer(last)
	m = pumpUntilIdle(t, m, cmd)
	if last.state() != xferDone {
		t.Fatalf("retried row state = %v err=%v", last.state(), last.err)
	}
	if n := m.clearFinished(); n != 5 || len(m.transfers) != 0 {
		t.Fatalf("clearFinished removed %d, left %d", n, len(m.transfers))
	}
}

func TestTransferFailureShowsErrorAndRetries(t *testing.T) {
	t.Parallel()
	src, dst := t.TempDir(), t.TempDir()
	style := core.NativePathStyle()
	m := filesModel{width: 160, height: 45, chans: map[int]*transferChans{},
		left:  paneState{pathStyle: style, cwd: src},
		right: paneState{pathStyle: style, cwd: dst},
	}
	job := &transferJob{srcPath: filepath.Join(src, "missing.txt"), dstPath: filepath.Join(dst, "missing.txt")}
	row := m.enqueueTransfer(job, "⇒ missing.txt", "missing.txt", "Local:"+dst, 10)
	m = pumpUntilIdle(t, m, m.pumpTransfers())
	if row.state() != xferFailed || row.err == nil {
		t.Fatalf("state = %v", row.state())
	}
	if lvl := m.statusLevelOf(); lvl != levelError || !strings.Contains(m.status, "missing.txt") {
		t.Fatalf("status = %q level %v", m.status, lvl)
	}
	out := stripANSI(m.View())
	if !strings.Contains(out, "✗") || !strings.Contains(out, "1 failed") {
		t.Fatalf("failed row should be visible in the panel: %q", out)
	}
	// Make it succeed and retry.
	if err := os.WriteFile(job.srcPath, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	m = pumpUntilIdle(t, m, m.retryFailed())
	if row.state() != xferDone {
		t.Fatalf("retry state = %v err=%v", row.state(), row.err)
	}
}

// TestMixedSelectionTransfersEveryFolder is a regression test: a selection
// with several folders used to confirm and transfer only the first one.
func TestMixedSelectionTransfersEveryFolder(t *testing.T) {
	t.Parallel()
	m := filesModel{width: 160, height: 45, chans: map[int]*transferChans{},
		left: paneState{remote: true, source: "a", pathStyle: core.TargetPathPOSIX, root: "/", cwd: "/src", selected: map[int]bool{}},
		right: paneState{remote: true, source: "b", pathStyle: core.TargetPathPOSIX, root: "/", cwd: "/dst", listedCwd: "/dst",
			allItems: []fileItem{{name: "keep"}}, selected: map[int]bool{}},
	}
	items := []fileItem{{name: "one", isDir: true}, {name: "f.txt", size: 5}, {name: "two", isDir: true}}
	mm, _ := m.planTransfer(0, 1, items, dtCopy, -1, true)
	m = mm.(filesModel)
	if m.plan == nil || m.plan.dirs != 2 || m.plan.files != 1 {
		t.Fatalf("plan = %+v", m.plan)
	}
	// Execute without starting goroutines: inspect what gets queued.
	plan := m.plan
	m.transfers = nil
	for _, it := range plan.items {
		_ = it
	}
	mm, _ = m.executePlanQueued(plan)
	m = mm.(filesModel)
	if len(m.transfers) != 3 {
		t.Fatalf("queued %d rows, want 3", len(m.transfers))
	}
	names := []string{}
	for _, r := range m.transfers {
		names = append(names, r.job.dstPath)
	}
	if strings.Join(names, ",") != "/dst/one,/dst/f.txt,/dst/two" {
		t.Fatalf("destinations = %v", names)
	}
}

func TestTransferConflictPolicies(t *testing.T) {
	t.Parallel()
	base := filesModel{width: 160, height: 45, chans: map[int]*transferChans{},
		left: paneState{remote: true, source: "a", pathStyle: core.TargetPathPOSIX, root: "/", cwd: "/src", selected: map[int]bool{}},
		right: paneState{remote: true, source: "b", pathStyle: core.TargetPathPOSIX, root: "/", cwd: "/dst", listedCwd: "/dst",
			allItems: []fileItem{{name: "a.txt"}, {name: "a copy.txt"}}, selected: map[int]bool{}},
	}
	items := []fileItem{{name: "a.txt", size: 1}, {name: "b.txt", size: 2}}
	mm, _ := base.planTransfer(0, 1, items, dtCopy, -1, true)
	m := mm.(filesModel)
	if m.plan == nil || len(m.plan.conflicts) != 1 || m.plan.conflicts[0] != "a.txt" {
		t.Fatalf("conflicts = %+v", m.plan)
	}
	if out := stripANSI(m.View()); !strings.Contains(out, "1 already exists") || !strings.Contains(out, "Overwrite") {
		t.Fatalf("dialog should explain the collision: %q", out)
	}
	cases := []struct {
		key  string
		want []string
	}{
		{"o", []string{"/dst/a.txt", "/dst/b.txt"}},
		{"s", []string{"/dst/b.txt"}},
		{"k", []string{"/dst/a copy 2.txt", "/dst/b.txt"}},
	}
	for _, c := range cases {
		mm, _ := m.handlePlanKey(key(c.key))
		mc := mm.(filesModel)
		mc.transfers = nil
		mm, _ = mc.executePlanQueued(mc.plan)
		mc = mm.(filesModel)
		var got []string
		for _, r := range mc.transfers {
			got = append(got, r.job.dstPath)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Fatalf("policy %s: %v want %v", c.key, got, c.want)
		}
	}
}

func TestTransferPanelRendersProgress(t *testing.T) {
	t.Parallel()
	m := sampleFilesModel(160, 45)
	m.transfers = append(m.transfers,
		&transferRow{id: 2, label: "↑ queued.bin", name: "queued.bin", queued: true, total: 10},
		&transferRow{id: 3, label: "↑ bad.bin", name: "bad.bin", done: true, err: errors.New("invalid_path: path is outside the agent's allowed file roots")},
	)
	out := stripANSI(m.View())
	for _, want := range []string{"Transfers", "1 running", "1 queued", "1 failed", "app.tar.gz", "66%", "queued", "--file-root"} {
		if !strings.Contains(out, want) {
			t.Fatalf("panel missing %q:\n%s", want, out)
		}
	}
	// t focuses the panel; ↓ selects the failed row and surfaces its error.
	m = press(t, m, "t", "down", "down")
	if !m.xferFocus || !strings.Contains(m.status, "allowed file roots") {
		t.Fatalf("focus=%v status=%q", m.xferFocus, m.status)
	}
	m = press(t, m, "esc")
	if m.xferFocus {
		t.Fatal("esc should leave the panel")
	}
}

// ---- errors, status, colour ----

func TestPaneErrorClassification(t *testing.T) {
	t.Parallel()
	remote := paneState{source: "web-01", remote: true, root: "/srv/fleet"}
	cases := []struct {
		err   string
		pane  paneState
		title string
	}{
		{"invalid_path: path is outside the agent's allowed file roots", remote, "Outside the allowed file roots"},
		{"open /root: permission denied", paneState{}, "Permission denied"},
		{"dial tcp 127.0.0.1:1: connect: connection refused", remote, "web-01 is unreachable"},
		{"open /x: no such file or directory", paneState{}, "Folder not found"},
		{"something odd", paneState{}, "Couldn't list this folder"},
	}
	for _, c := range cases {
		if got := classifyPaneError(errors.New(c.err), c.pane); got.title != c.title {
			t.Fatalf("%q → %q want %q", c.err, got.title, c.title)
		}
	}
	m := sampleFilesModel(120, 40)
	m.right.err = errors.New("invalid_path: path is outside the agent's allowed file roots")
	m.right.root = "/srv/fleet"
	out := stripANSI(m.View())
	for _, want := range []string{"Outside the allowed file roots", "Allowed root: /srv/fleet", "~ go to allowed root"} {
		if !strings.Contains(out, want) {
			t.Fatalf("error state missing %q", want)
		}
	}
}

func TestStatusSeverityAndExpiry(t *testing.T) {
	t.Parallel()
	m := sampleFilesModel(120, 40)
	m.setStatus(levelOK, "copied things")
	mm, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = mm.(filesModel)
	if cmd == nil {
		t.Fatal("a new status should schedule its expiry")
	}
	seq := m.statusSeq
	mm, _ = m.Update(statusExpireMsg{seq: seq - 1})
	if mm.(filesModel).status == "" {
		t.Fatal("stale expiry must not clear a newer message")
	}
	mm, _ = m.Update(statusExpireMsg{seq: seq})
	if mm.(filesModel).status != "" {
		t.Fatal("expiry should clear the message")
	}
	for text, want := range map[string]fmLevel{
		"delete failed: x":          levelError,
		"cancelled":                 levelWarn,
		"completed: ↑ a":            levelOK,
		"left pane: sort by Size ↑": levelInfo,
	} {
		if got := classifyStatus(text); got != want {
			t.Fatalf("classifyStatus(%q) = %v want %v", text, got, want)
		}
	}
}

// TestPaletteSurvives256Colours is a regression test for termenv degrading
// the light text colour to 232 (near black) in 256-colour terminals.
func TestPaletteSurvives256Colours(t *testing.T) {
	for hex, want := range map[string]string{"#e7ecef": "255", "#ffffff": "231", "#000000": "16", "#00d4aa": "43"} {
		if got := fmColor(hex).ANSI256; got != want {
			t.Fatalf("fmColor(%s).ANSI256 = %s want %s", hex, got, want)
		}
	}
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	if p := fmPal(); strings.Contains(p.base.pre, "38;5;232") {
		t.Fatalf("text colour degraded to 232: %q", p.base.pre)
	}
}

// TestNoColorKeepsCursorVisible: with NO_COLOR (Ascii profile) the cursor row
// must still be distinguishable, via reverse video.
func TestNoColorKeepsCursorVisible(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	m := sampleFilesModel(80, 24)
	row := m.renderRow(0, m.left.index, 40, false)
	if !strings.Contains(row, "\x1b[7") && !strings.Contains(row, ";7m") {
		t.Fatalf("cursor row without colour should use reverse video: %q", row)
	}
	if strings.Contains(row, "38;") || strings.Contains(row, "48;") {
		t.Fatalf("Ascii profile must not emit colours: %q", row)
	}
	out := m.View()
	if n := len(frameLines(out)); n != 24 {
		t.Fatalf("NO_COLOR frame has %d lines", n)
	}
}

// TestEditorFrameFitsScreen is a regression test: the editor's header and
// rules were built for the padded width, so they wrapped and pushed the
// title off the top of the screen.
func TestEditorFrameFitsScreen(t *testing.T) {
	t.Parallel()
	for _, dim := range [][2]int{{80, 24}, {160, 45}, {220, 60}} {
		m := sampleFilesModel(dim[0], dim[1])
		m.overlay = overlayEditor
		m.editor = &editorState{active: true, path: "/x/main.go", name: "main.go"}
		mm, _ := m.onEditorLoaded(editorLoadedMsg{path: "/x/main.go", name: "main.go", content: "package main\n\nfunc main() {}\n"})
		m = mm.(filesModel)
		for _, mode := range []string{"view", "edit"} {
			if mode == "edit" {
				mm, _ = m.toggleEditorMode()
				m = mm.(filesModel)
			}
			lines := frameLines(m.View())
			if len(lines) != dim[1] {
				t.Fatalf("%v %s: editor frame has %d lines, want %d", dim, mode, len(lines), dim[1])
			}
			for i, ln := range lines {
				if w := lipgloss.Width(ln); w > dim[0] {
					t.Fatalf("%v %s: line %d is %d wide", dim, mode, i, w)
				}
			}
			if !strings.Contains(stripANSI(lines[2]), "main.go") {
				t.Fatalf("%v %s: title not on the header line: %q", dim, mode, stripANSI(lines[2]))
			}
		}
	}
}
