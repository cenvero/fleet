// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"
)

// ============================================================================
// Palette & styles (self-contained, Charm-grade polish)
// ============================================================================

var (
	fmAccent   = fmColor("#00d4aa")
	fmAccent2  = fmColor("#36f0c0")
	fmInk      = fmColor("#04231d")
	fmText     = fmColor("#e7ecef")
	fmMutedC   = fmColor("#8fa7b3")
	fmDimC     = fmColor("#5f7480")
	fmBorderC  = fmColor("#1c2b36")
	fmZebraC   = fmColor("#0c141d")
	fmDirC     = fmColor("#7ad7ff")
	fmDangerC  = fmColor("#ff6b6b")
	fmWarnC    = fmColor("#ffce6b")
	fmHeaderBg = fmColor("#0e1620")
	fmPanelBg  = fmColor("#0d131b")
	fmDropC    = fmColor("#36f0c0")

	// Category colors for file-type icons (palette-consistent: teal/blue accents,
	// soft warm tones). Used by iconFor for both list and grid views.
	fmCodeC    = fmColor("#7ee787") // code: soft green
	fmDocC     = fmColor("#a8c7e0") // docs/text: soft blue
	fmDataC    = fmColor("#c8a8ff") // structured data: lavender
	fmImageC   = fmColor("#f0a8d0") // images: pink
	fmArchiveC = fmColor("#e0b87a") // archives: amber/tan
	fmMediaC   = fmColor("#8fd0c8") // audio/video: muted teal
	fmExecC    = fmColor("#ff9d6b") // executables/binaries: warm orange
	fmConfigC  = fmColor("#9fb0bd") // config/dotfiles: cool grey
	fmDocsRedC = fmColor("#ff8c8c") // pdf/rich docs: soft red

	fmTag       = lipgloss.NewStyle().Foreground(fmDimC)
	fmServerTag = lipgloss.NewStyle().Foreground(fmMutedC)

	fmRule = lipgloss.NewStyle().Foreground(fmBorderC)

	fmSelRow = lipgloss.NewStyle().Background(fmAccent).Foreground(fmInk).Bold(true)

	fmKeyChip   = lipgloss.NewStyle().Background(fmBorderC).Foreground(fmAccent2).Bold(true).Padding(0, 1)
	fmHintLabel = lipgloss.NewStyle().Foreground(fmMutedC)
	fmStatusSty = lipgloss.NewStyle().Foreground(fmMutedC)
	fmDoneSty   = lipgloss.NewStyle().Foreground(fmAccent).Bold(true)
	fmErrSty    = lipgloss.NewStyle().Foreground(fmDangerC).Bold(true)
	fmWarnSty   = lipgloss.NewStyle().Foreground(fmWarnC).Bold(true)
	fmTextSty   = lipgloss.NewStyle().Foreground(fmText)
	fmTitleSty  = lipgloss.NewStyle().Foreground(fmAccent).Bold(true)
	fmDimSty    = lipgloss.NewStyle().Foreground(fmDimC)

	fmCrumbSty = lipgloss.NewStyle().Foreground(fmDimC)
	fmCrumbCur = lipgloss.NewStyle().Foreground(fmAccent2).Bold(true)
	fmCrumbSep = lipgloss.NewStyle().Foreground(fmBorderC)

	fmOverlayBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(fmAccent).
			Background(fmPanelBg).
			Padding(1, 2)

	fmMenuBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(fmBorderC).
			Background(fmPanelBg).
			Padding(0, 1)

	fmInputSty = lipgloss.NewStyle().
			Background(fmColor("#101822")).
			Foreground(fmText).
			Padding(0, 1)
)

// Glyphs for pane sources. Both are single-cell in every common terminal
// (the old 🖥 emoji is two cells in some and one in others, which tore the
// pane border).
const (
	fmLocalGlyph  = "⌂"
	fmRemoteGlyph = "☁"
)

func sourceGlyph(remote bool) string {
	if remote {
		return fmRemoteGlyph
	}
	return fmLocalGlyph
}

// ============================================================================
// Top-level View
// ============================================================================

// frameKey is a compact digest of everything the base frame renders from. It is
// deliberately cheap: pane content is represented by paneState.rev (bumped on
// every listing/sort/filter/selection change) rather than by walking entries,
// and m.ver is bumped by Update for every message except pure mouse motion, so
// any state change that goes through Update re-renders.
//
// It is only meaningful when no overlay or drag is active — those carry a lot of
// transient state (menu geometry, editor buffer, ghost position, mouse
// coordinates) that would be easy to under-capture, so View skips the cache
// entirely in those modes rather than risk a stale frame.
func (m filesModel) frameKey() string {
	var b strings.Builder
	b.Grow(192)
	put := func(vals ...int) {
		for _, v := range vals {
			b.WriteString(strconv.Itoa(v))
			b.WriteByte('|')
		}
	}
	put(int(m.ver), m.width, m.height, m.focus, boolInt(m.showHidden), m.hoverSide, m.hoverIndex)
	b.WriteString(m.hoverTool)
	b.WriteByte('|')
	b.WriteString(m.status)
	b.WriteByte('|')
	for _, p := range []paneState{m.left, m.right} {
		b.WriteString(p.source)
		b.WriteByte('\x00')
		b.WriteString(p.cwd)
		b.WriteByte('\x00')
		b.WriteString(p.filter)
		b.WriteByte('\x00')
		if p.err != nil {
			b.WriteString(p.err.Error())
		}
		b.WriteByte('|')
		b.WriteString(strconv.FormatUint(p.rev, 10))
		b.WriteByte('|')
		put(p.index, p.scroll, boolInt(p.loading), int(p.view), int(p.sortBy),
			boolInt(p.sortDesc), len(p.entries), len(p.selected))
	}
	for _, t := range m.transfers {
		put(t.id, int(t.bytesDone), int(t.total), t.streams, boolInt(t.done), boolInt(t.err != nil))
		// Rate is displayed rounded to whole units; key off the same resolution
		// so a jittering float doesn't invalidate an otherwise identical frame.
		put(int(t.rate) / 1024)
	}
	return b.String()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (m filesModel) View() string {
	// Fast path: nothing the base frame depends on changed since the last
	// render, so hand back the identical string instead of rebuilding it. This
	// is what makes idle mouse motion (one event per cursor move under
	// WithMouseAllMotion) essentially free.
	cacheable := m.overlay == overlayNone && m.drag == nil && m.frames != nil
	var key string
	if cacheable {
		key = m.frameKey()
		if m.frames.valid && m.frames.key == key {
			return m.frames.frame
		}
	}

	frame := m.renderFrame()

	if cacheable {
		m.frames.key = key
		m.frames.frame = frame
		m.frames.valid = true
	} else if m.frames != nil {
		// An overlay/drag frame was rendered uncached; make sure the next base
		// frame re-renders rather than restoring a pre-overlay screen.
		m.frames.valid = false
	}
	return frame
}

func (m filesModel) renderFrame() string {
	if m.overlay == overlayEditor {
		return zone.Scan(m.renderEditor())
	}
	l := m.layout()
	p := fmPal()
	if l.tooSmall {
		return m.renderTooSmall(l, p)
	}

	lines := make([]string, 0, l.h)
	lines = append(lines, m.renderHeaderLine(l, p))
	lines = append(lines, m.renderToolbarLine(l, p))

	var boxes [3][]string
	for side := range 2 {
		if l.paneShown[side] {
			boxes[side] = m.renderPaneBox(side, l, p)
		}
	}
	if l.previewShown {
		boxes[2] = m.renderPreviewBox(l, p)
	}
	type col struct {
		x, w  int
		lines []string
	}
	cols := make([]col, 0, 3)
	for side := range 2 {
		if boxes[side] != nil {
			cols = append(cols, col{x: l.paneX[side], w: l.paneW[side], lines: boxes[side]})
		}
	}
	if boxes[2] != nil {
		cols = append(cols, col{x: l.previewX, w: l.previewW, lines: boxes[2]})
	}
	// Order columns left to right (the preview can sit on the left when it
	// replaces the left pane).
	for i := 1; i < len(cols); i++ {
		for j := i; j > 0 && cols[j].x < cols[j-1].x; j-- {
			cols[j], cols[j-1] = cols[j-1], cols[j]
		}
	}
	for row := range l.boxH {
		var b strings.Builder
		x := 0
		for _, c := range cols {
			if c.x > x {
				b.WriteString(p.pad(c.x - x))
				x = c.x
			}
			if row < len(c.lines) {
				b.WriteString(c.lines[row])
			}
			x += c.w
		}
		if x < l.w {
			b.WriteString(p.pad(l.w - x))
		}
		lines = append(lines, b.String())
	}

	lines = append(lines, m.renderTransferPanel(l, p)...)
	lines = append(lines, m.renderStatusLine(l, p))
	if l.footer {
		lines = append(lines, m.renderFooterLine(l, p))
	}
	for len(lines) < l.h {
		lines = append(lines, p.pad(l.w))
	}
	if len(lines) > l.h && l.h > 0 {
		lines = lines[:l.h]
	}
	rendered := strings.Join(lines, "\n")

	// Compose overlays on top of the base frame, then drag ghost on top of all.
	if m.overlay != overlayNone {
		rendered = m.composeOverlay(rendered)
		rendered = m.composeGhost(rendered)
		// Only overlays carry bubblezone markers; the base frame is hit-tested
		// arithmetically (see fmLayout), so zone.Scan — which walks the whole
		// frame — is paid only while a popup is open.
		return zone.Scan(rendered)
	}
	return m.composeGhost(rendered)
}

func (m filesModel) renderTooSmall(l fmLayout, p *fmPalette) string {
	msg := fmt.Sprintf("Terminal too small (%d×%d)", l.w, l.h)
	hint := "resize to at least 30×10 · q quit"
	lines := make([]string, 0, l.h)
	for i := range l.h {
		switch i {
		case l.h/2 - 1:
			lines = append(lines, p.accentB.s(fmPadRight(" "+msg, l.w)))
		case l.h / 2:
			lines = append(lines, p.muted.s(fmPadRight(" "+hint, l.w)))
		default:
			lines = append(lines, p.pad(l.w))
		}
	}
	return strings.Join(lines, "\n")
}

// ============================================================================
// Header (brand + sources)
// ============================================================================

func (m filesModel) renderHeaderLine(l fmLayout, p *fmPalette) string {
	w := l.innerW
	brand := " ◆ Cenvero Fleet"
	tag := "  ·  files"
	srcs := fmt.Sprintf("%s %s  ⇄  %s %s ",
		sourceGlyph(m.left.remote), fmSanitize(m.left.label()),
		sourceGlyph(m.right.remote), fmSanitize(m.right.label()))
	if w < 70 {
		tag = ""
	}
	used := fmWidth(brand) + fmWidth(tag)
	if used+fmWidth(srcs) > w {
		srcs = fmFit(srcs, w-used)
	}
	gap := w - used - fmWidth(srcs)
	if gap < 0 {
		gap = 0
	}
	line := p.barBrand.s(brand) + p.barDim.s(tag) + p.barBase.s(strings.Repeat(" ", gap)) + p.barMuted.s(srcs)
	return p.pad(l.padX) + line + p.pad(l.w-l.padX-w)
}

// ============================================================================
// Toolbar (responsive action strip; every button has a key)
// ============================================================================

type toolButton struct {
	action string
	key    string
	label  string
	short  string
	prio   int
}

type toolSpan struct {
	x0, x1 int
	action string
}

type toolbarFit struct {
	shown    []toolButton
	right    []toolButton
	overflow []toolButton
	short    bool
	keysOnly bool
	spans    []toolSpan
	width    int
}

// toolbarButtons lists the action strip in display order. prio decides what
// collapses into the "≡ More" menu first when the terminal is narrow (lower
// goes first).
func (m filesModel) toolbarButtons() []toolButton {
	focus := m.paneRefConst(m.focus)
	previewLabel := "Preview"
	if m.preview.on {
		previewLabel = "Preview ✓"
	}
	return []toolButton{
		{"source", "s", "Source", "Src", 8},
		{"edit", "e", "Edit", "Edit", 7},
		{"newfolder", "n", "Folder", "Dir", 6},
		{"newfile", "N", "File", "File", 3},
		{"rename", "r", "Rename", "Ren", 6},
		{"delete", "d", "Delete", "Del", 8},
		{"copy", "c", "Copy →", "Copy", 10},
		{"move", "m", "Move →", "Move", 10},
		{"compress", "z", "Zip", "Zip", 2},
		{"chmod", "p", "Perms", "Perm", 1},
		{"props", "i", "Info", "Info", 4},
		{"filter", "/", "Filter", "Filt", 5},
		{"sort", "o", sortLabel(focus), sortShort(focus), 5},
		{"view", "v", viewLabel(focus.view), viewShort(focus.view), 7},
		{"preview", "P", previewLabel, "Prev", 5},
		{"hidden", ".", hiddenLabel(m.showHidden), "Hid", 2},
		{"refresh", "g", "Refresh", "Ref", 1},
	}
}

func toolBtnText(b toolButton, short, keysOnly bool) string {
	switch {
	case keysOnly:
		return " " + b.key + " "
	case short:
		return " " + b.key + " " + b.short + " "
	default:
		return " " + b.key + " " + b.label + " "
	}
}

// toolbarLayout picks the richest toolbar that fits in width w: first by
// moving low-priority buttons into an overflow menu, then by shortening
// labels, then by showing keys only. It never wraps.
func (m filesModel) toolbarLayout(w int) toolbarFit {
	all := m.toolbarButtons()
	help := toolButton{"help", "?", "Help", "Help", 99}
	quit := toolButton{"quit", "q", "Quit", "Quit", 99}
	more := toolButton{"more", "≡", "More", "More", 99}

	measure := func(shown []toolButton, right []toolButton, short, keysOnly bool) int {
		w := 1 // leading space
		for i, b := range shown {
			if i > 0 {
				w++ // separator
			}
			w += fmWidth(toolBtnText(b, short, keysOnly))
		}
		w++ // gap before the right group
		for i, b := range right {
			if i > 0 {
				w++
			}
			w += fmWidth(toolBtnText(b, short, keysOnly))
		}
		return w
	}
	// Removal order: lowest priority first; among equals, the right-most first.
	order := make([]int, len(all))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		if all[order[a]].prio != all[order[b]].prio {
			return all[order[a]].prio < all[order[b]].prio
		}
		return order[a] > order[b]
	})
	build := func(removed map[int]bool, short, keysOnly bool) toolbarFit {
		var shown, overflow []toolButton
		for i, b := range all {
			if removed[i] {
				overflow = append(overflow, b)
			} else {
				shown = append(shown, b)
			}
		}
		right := []toolButton{help, quit}
		if len(overflow) > 0 {
			right = []toolButton{more, help, quit}
		}
		width := measure(shown, right, short, keysOnly)
		return toolbarFit{shown: shown, right: right, overflow: overflow, short: short, keysOnly: keysOnly, width: width}
	}
	// For each label style, drop buttons one at a time (never below keepPrio)
	// until the strip fits.
	fitWith := func(short, keysOnly bool, keepPrio int) (toolbarFit, bool) {
		removed := map[int]bool{}
		f := build(removed, short, keysOnly)
		for _, i := range order {
			if f.width <= w {
				return f, true
			}
			if all[i].prio >= keepPrio {
				break
			}
			removed[i] = true
			f = build(removed, short, keysOnly)
		}
		return f, f.width <= w
	}
	fit, ok := fitWith(false, false, 7)
	if !ok {
		fit, ok = fitWith(true, false, 9)
	}
	if !ok {
		fit, ok = fitWith(false, true, 100)
	}
	_ = ok
	// Lay out spans (x relative to the start of the toolbar content).
	x := 1
	for i, b := range fit.shown {
		if i > 0 {
			x++
		}
		t := fmWidth(toolBtnText(b, fit.short, fit.keysOnly))
		fit.spans = append(fit.spans, toolSpan{x0: x, x1: x + t, action: b.action})
		x += t
	}
	rightW := 0
	for i, b := range fit.right {
		if i > 0 {
			rightW++
		}
		rightW += fmWidth(toolBtnText(b, fit.short, fit.keysOnly))
	}
	rx := w - rightW
	if rx < x+1 {
		rx = x + 1
	}
	for i, b := range fit.right {
		if i > 0 {
			rx++
		}
		t := fmWidth(toolBtnText(b, fit.short, fit.keysOnly))
		fit.spans = append(fit.spans, toolSpan{x0: rx, x1: rx + t, action: b.action})
		rx += t
	}
	return fit
}

func (m filesModel) renderToolbarLine(l fmLayout, p *fmPalette) string {
	w := l.innerW
	fit := m.toolbarLayout(w)
	var b strings.Builder
	x := 0
	btn := func(t toolButton, sp toolSpan) {
		if sp.x0 > x {
			if x > 1 && sp.x0-x == 1 {
				b.WriteString(p.toolSep.s("│"))
			} else {
				b.WriteString(p.toolBase.s(strings.Repeat(" ", sp.x0-x)))
			}
		}
		text := toolBtnText(t, fit.short, fit.keysOnly)
		if m.hoverTool == t.action {
			b.WriteString(p.toolHot.s(text))
		} else {
			// " k Label ": key bright, label muted.
			key := t.key
			rest := strings.TrimPrefix(text, " "+key)
			b.WriteString(p.toolBase.s(" "))
			b.WriteString(p.toolKey.s(key))
			b.WriteString(p.toolLabel.s(rest))
		}
		x = sp.x1
	}
	all := append(append([]toolButton{}, fit.shown...), fit.right...)
	for i, t := range all {
		if i < len(fit.spans) {
			btn(t, fit.spans[i])
		}
	}
	if x < w {
		b.WriteString(p.toolBase.s(strings.Repeat(" ", w-x)))
	}
	line := b.String()
	if fit.width > w {
		line = truncateANSI(line, w)
	}
	return p.pad(l.padX) + line + p.pad(l.w-l.padX-w)
}

func hiddenLabel(on bool) string {
	if on {
		return "Hidden ✓"
	}
	return "Hidden"
}

// sortLabel names the toolbar/menu sort button for a pane's current sort key and
// direction (e.g. "Sort: Size ↓").
func sortLabel(p paneState) string {
	return "Sort: " + p.sortBy.label() + " " + sortArrow(p.sortDesc)
}

func sortShort(p paneState) string {
	return p.sortBy.label() + sortArrow(p.sortDesc)
}

// viewLabel names the toolbar/menu button for the focused pane's current layout
// (showing what `v` will offer next, Finder-style).
func viewLabel(v viewMode) string {
	if v == viewGrid {
		return "View: Icons"
	}
	return "View: List"
}

func viewShort(v viewMode) string {
	if v == viewGrid {
		return "Icons"
	}
	return "List"
}

// ============================================================================
// Pane box
// ============================================================================

// rowColumns is the list-view column plan for a content width.
type rowColumns struct {
	nameW, sizeW, modeW, timeW int
}

func listColumns(cw int) rowColumns {
	c := rowColumns{}
	switch {
	case cw >= 96:
		c.sizeW, c.modeW, c.timeW = 8, 10, 12
	case cw >= 46:
		c.sizeW, c.timeW = 8, 12
	case cw >= 30:
		c.sizeW = 8
	}
	// lead " m i " (5) + trailing " " (1) + one separator per column.
	used := 6
	for _, w := range []int{c.sizeW, c.modeW, c.timeW} {
		if w > 0 {
			used += 1 + w
		}
	}
	c.nameW = cw - used
	if c.nameW < 1 {
		c.nameW = 1
	}
	return c
}

// renderPaneBox draws one pane as exactly l.boxH lines, each l.paneW wide.
func (m filesModel) renderPaneBox(side int, l fmLayout, p *fmPalette) []string {
	pane := m.paneRefConst(side)
	w := l.paneW[side]
	cw := l.contentW(side)
	focused := side == m.focus
	isDropTarget := m.drag != nil && m.drag.active && m.hoverSide == side && side != m.drag.fromSide

	border := p.paneRule
	title := p.muted
	switch {
	case isDropTarget:
		border, title = p.accent2, p.accent2
	case focused:
		border, title = p.accent, p.accentB
	}

	out := make([]string, 0, l.boxH)

	// Top border: ╭─ ⌂ Local ──────────── badges ─╮
	name := fmSanitize(pane.label())
	titleText := " " + sourceGlyph(pane.remote) + " " + name + " "
	if focused && p.noColor {
		titleText = " ▸" + titleText
	}
	var badges []string
	var badgePaints []fmPaint
	if pane.loading && pane.listedCwd == pane.cwd && pane.listedCwd != "" {
		badges, badgePaints = append(badges, " ↻ "), append(badgePaints, p.muted)
	}
	if m.mirror {
		badges, badgePaints = append(badges, " ⇆ mirror "), append(badgePaints, p.accent2)
	}
	if pane.view == viewGrid {
		badges, badgePaints = append(badges, " icons "), append(badgePaints, p.dim)
	}
	badgeW := 0
	for _, b := range badges {
		badgeW += fmWidth(b)
	}
	maxTitle := w - 4 - badgeW
	if maxTitle < 4 {
		maxTitle = 4
		badges, badgePaints, badgeW = nil, nil, 0
	}
	titleText = fmFit(titleText, maxTitle)
	fill := w - 3 - fmWidth(titleText) - badgeW - 1
	if fill < 0 {
		fill = 0
	}
	var top strings.Builder
	top.WriteString(border.s("╭─"))
	top.WriteString(title.s(titleText))
	top.WriteString(border.s(strings.Repeat("─", fill)))
	for i, b := range badges {
		top.WriteString(badgePaints[i].s(b))
	}
	top.WriteString(border.s("─╮"))
	out = append(out, top.String())

	side2 := border.s("│")
	// Breadcrumb line.
	out = append(out, side2+m.renderCrumbLine(side, cw, p)+side2)
	// Column header.
	out = append(out, side2+m.renderColumnHeader(side, cw, p)+side2)

	// Body.
	body := m.renderPaneBody(side, cw, l.rows, isDropTarget, p)
	sb := scrollbar(len(pane.entries), l.rows, pane.scroll, pane.view == viewGrid, m.gridColsFor(cw))
	for i := range l.rows {
		line := ""
		if i < len(body) {
			line = body[i]
		}
		right := side2
		if sb != nil && sb[i] {
			if focused {
				right = p.accent.s("┃")
			} else {
				right = p.muted.s("┃")
			}
		}
		out = append(out, side2+line+right)
	}

	// Bottom border: ╰─ 12 items · 3 selected (1.2 MB) ──── 37/50000 ─╯
	info := " " + m.paneSummary(side) + " "
	pos := ""
	if n := realCountFast(pane.entries); n > 0 && !pane.loading {
		pos = " " + groupThousands(int64(positionOf(pane))) + "/" + groupThousands(int64(n)) + " "
	}
	info = fmFit(info, w-4-fmWidth(pos))
	fill = w - 3 - fmWidth(info) - fmWidth(pos) - 1
	if fill < 0 {
		fill = 0
	}
	var bot strings.Builder
	bot.WriteString(border.s("╰─"))
	if len(pane.selected) > 0 {
		bot.WriteString(p.accent2.s(info))
	} else {
		bot.WriteString(p.dim.s(info))
	}
	bot.WriteString(border.s(strings.Repeat("─", fill)))
	bot.WriteString(p.dim.s(pos))
	bot.WriteString(border.s("─╯"))
	out = append(out, bot.String())
	return out
}

func (m filesModel) gridColsFor(cw int) int { return gridCols(cw) }

// positionOf is the 1-based cursor position among real entries.
func positionOf(p paneState) int {
	pos := p.index + 1
	if len(p.entries) > 0 && p.entries[0].name == ".." {
		pos--
	}
	if pos < 1 {
		pos = 1
	}
	return pos
}

// realCountFast counts entries excluding "..", which only ever sits first.
func realCountFast(items []fileItem) int {
	n := len(items)
	if n > 0 && items[0].name == ".." {
		n--
	}
	return n
}

// paneSummary is the per-pane footer: item count, and the selection's size.
func (m filesModel) paneSummary(side int) string {
	pane := m.paneRefConst(side)
	if pane.loading && pane.listedCwd != pane.cwd {
		return "loading…"
	}
	if pane.err != nil {
		return "unavailable"
	}
	n := realCountFast(pane.entries)
	s := plural(n, "item", "items")
	if pane.filter != "" {
		s = fmt.Sprintf("%d of %d shown", n, len(pane.allItems))
	}
	if len(pane.selected) > 0 {
		st := m.selectionStats(side)
		s = fmt.Sprintf("%d selected · %s", st.count, st.sizeText())
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%s %s", groupThousands(int64(n)), many)
}

// groupThousands renders 50000 as "50,000".
func groupThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// scrollbar returns, for each body line, whether it is part of the thumb. nil
// means everything fits and no scrollbar is drawn.
func scrollbar(total, rows, scroll int, grid bool, cols int) []bool {
	if grid {
		if cols < 1 {
			cols = 1
		}
		total = (total + cols - 1) / cols * gridCellH
		scroll = scroll / cols * gridCellH
	}
	if total <= rows || rows <= 0 {
		return nil
	}
	thumb := rows * rows / total
	if thumb < 1 {
		thumb = 1
	}
	maxScroll := total - rows
	pos := 0
	if maxScroll > 0 {
		pos = scroll * (rows - thumb) / maxScroll
	}
	if pos+thumb > rows {
		pos = rows - thumb
	}
	out := make([]bool, rows)
	for i := pos; i < pos+thumb; i++ {
		out[i] = true
	}
	return out
}

// ---- breadcrumb line ----

type crumbSpan struct {
	x0, x1 int
	path   string
}

// crumbLayout renders the clickable breadcrumb for a pane within cw columns
// (including a leading space) and returns the rendered text and its spans.
func (m filesModel) crumbLayout(side, cw int, p *fmPalette) (string, []crumbSpan) {
	pane := m.paneRefConst(side)
	segs := breadcrumbSegmentsWithin(pane.cwd, pane.root, pane.pathStyle)
	paths := make([]string, len(segs))
	for i, s := range segs {
		if i == 0 {
			paths[i] = s
		} else {
			paths[i] = pane.pathStyle.Join(paths[i-1], s)
		}
	}
	names := make([]string, len(segs))
	for i, s := range segs {
		names[i] = fmSanitize(s)
	}
	const sep = " › "
	avail := cw - 1
	// Drop leading segments (keeping the root) until the rest fits.
	start := 1
	width := func(from int) int {
		w := fmWidth(names[0])
		if from > 1 {
			w += fmWidth(sep) + 1 // "…"
		}
		for i := from; i < len(names); i++ {
			w += fmWidth(sep) + fmWidth(names[i])
		}
		return w
	}
	for start < len(names)-1 && width(start) > avail {
		start++
	}
	var b strings.Builder
	var spans []crumbSpan
	x := 1
	b.WriteString(p.base.s(" "))
	add := func(text string, pt fmPaint, path string) {
		tw := fmWidth(text)
		if x+tw > cw {
			text = fmFit(text, cw-x)
			tw = fmWidth(text)
		}
		if tw <= 0 {
			return
		}
		b.WriteString(pt.s(text))
		if path != "" {
			spans = append(spans, crumbSpan{x0: x, x1: x + tw, path: path})
		}
		x += tw
	}
	last := len(names) - 1
	segPaint := func(i int) fmPaint {
		if i == last {
			return p.accent2
		}
		return p.dim
	}
	add(names[0], segPaint(0), paths[0])
	if start > 1 {
		add(sep, p.rule, "")
		add("…", p.dim, paths[start-1])
	}
	for i := start; i < len(names); i++ {
		add(sep, p.rule, "")
		add(names[i], segPaint(i), paths[i])
	}
	// Right-aligned filter badge.
	badge := ""
	if pane.filter != "" {
		badge = " /" + fmFit(fmSanitize(pane.filter), 16) + " "
	}
	if bw := fmWidth(badge); bw > 0 && x+bw+1 <= cw {
		b.WriteString(p.pad(cw - x - bw))
		b.WriteString(p.warn.s(badge))
		x = cw
	}
	if x < cw {
		b.WriteString(p.pad(cw - x))
	}
	return b.String(), spans
}

func (m filesModel) renderCrumbLine(side, cw int, p *fmPalette) string {
	s, _ := m.crumbLayout(side, cw, p)
	return s
}

// crumbHit returns the index of the crumb span under content column cx, or -1.
func (m filesModel) crumbHit(side, cw, cx int) int {
	_, spans := m.crumbLayout(side, cw, fmPal())
	for i, sp := range spans {
		if cx >= sp.x0 && cx < sp.x1 {
			return i
		}
	}
	return -1
}

// crumbPath returns the directory the i-th crumb span navigates to.
func (m filesModel) crumbPath(side, cw, i int) string {
	_, spans := m.crumbLayout(side, cw, fmPal())
	if i < 0 || i >= len(spans) {
		return ""
	}
	return spans[i].path
}

// ---- column header ----

func (m filesModel) renderColumnHeader(side, cw int, p *fmPalette) string {
	pane := m.paneRefConst(side)
	arrow := sortArrow(pane.sortDesc)
	label := func(text string, key sortKey) (string, fmPaint) {
		if pane.sortBy == key {
			return text + " " + arrow, p.accent2
		}
		return text, p.dim
	}
	if pane.view == viewGrid {
		t, pt := label("Name", sortName)
		s, sp := label("Size", sortSize)
		d, dp := label("Modified", sortModified)
		txt := "   " + t
		line := p.base.s("   ") + pt.s(t)
		rest := "  " + s + "  " + d
		if fmWidth(txt)+fmWidth(rest) <= cw {
			line += p.dim.s("  ") + sp.s(s) + p.dim.s("  ") + dp.s(d)
			txt += rest
		}
		return line + p.pad(cw-fmWidth(txt))
	}
	c := listColumns(cw)
	var b strings.Builder
	x := 0
	write := func(text string, pt fmPaint) {
		b.WriteString(pt.s(text))
		x += fmWidth(text)
	}
	write("     ", p.base)
	t, pt := label("Name", sortName)
	write(fmPadRight(t, c.nameW), pt)
	if c.sizeW > 0 {
		s, sp := label("Size", sortSize)
		write(" ", p.base)
		write(fmPadLeft(s, c.sizeW), sp)
	}
	if c.modeW > 0 {
		write(" ", p.base)
		write(fmPadRight("Mode", c.modeW), p.dim)
	}
	if c.timeW > 0 {
		d, dp := label("Modified", sortModified)
		write(" ", p.base)
		write(fmPadRight(d, c.timeW), dp)
	}
	if x < cw {
		write(strings.Repeat(" ", cw-x), p.base)
	}
	return b.String()
}

// columnHit maps a click on the column header to a sort key name.
func (m filesModel) columnHit(side, cw, cx int) string {
	pane := m.paneRefConst(side)
	if pane.view == viewGrid {
		return ""
	}
	c := listColumns(cw)
	x := 5
	if cx >= x && cx < x+c.nameW {
		return "name"
	}
	x += c.nameW
	if c.sizeW > 0 {
		if cx >= x && cx < x+1+c.sizeW {
			return "size"
		}
		x += 1 + c.sizeW
	}
	if c.modeW > 0 {
		x += 1 + c.modeW
	}
	if c.timeW > 0 && cx >= x && cx < x+1+c.timeW {
		return "modified"
	}
	return ""
}

// ---- body ----

func (m filesModel) renderPaneBody(side, cw, rows int, isDropTarget bool, p *fmPalette) []string {
	pane := m.paneRefConst(side)
	switch {
	case pane.loading && pane.listedCwd != pane.cwd:
		return centeredBlock(cw, rows, p, []fmStyledLine{{"Loading…", p.muted}})
	case pane.err != nil && !(pane.loading && pane.listedCwd == pane.cwd):
		return m.renderPaneError(side, cw, rows, p)
	case len(pane.entries) == 0 || (len(pane.entries) == 1 && pane.entries[0].name == ".." && pane.filter != ""):
		if pane.filter != "" {
			return centeredBlock(cw, rows, p, []fmStyledLine{
				{"No items match “" + fmFit(fmSanitize(pane.filter), cw-20) + "”", p.warn},
				{"", p.base},
				{"esc clear filter · / edit filter", p.dim},
			})
		}
		if len(pane.entries) == 0 {
			return centeredBlock(cw, rows, p, []fmStyledLine{
				{"This folder is empty", p.muted},
				{"", p.base},
				{"n new folder · N new file · drop files here", p.dim},
			})
		}
	}
	if pane.view == viewGrid {
		return m.renderGridLines(side, cw, rows, isDropTarget, p)
	}
	return m.renderListLines(side, cw, rows, isDropTarget)
}

type fmStyledLine struct {
	text string
	pt   fmPaint
}

// centeredBlock renders a few lines vertically/horizontally centred in the
// pane body.
func centeredBlock(cw, rows int, p *fmPalette, lines []fmStyledLine) []string {
	out := make([]string, 0, rows)
	top := (rows - len(lines)) / 3
	if top < 0 {
		top = 0
	}
	for i := 0; i < top && len(out) < rows; i++ {
		out = append(out, p.pad(cw))
	}
	for _, ln := range lines {
		if len(out) >= rows {
			break
		}
		t := fmFit(ln.text, cw-2)
		tw := fmWidth(t)
		left := (cw - tw) / 2
		out = append(out, p.pad(left)+ln.pt.s(t)+p.pad(cw-left-tw))
	}
	for len(out) < rows {
		out = append(out, p.pad(cw))
	}
	return out
}

// renderListBody is kept for callers/tests that want the joined body text.
func (m filesModel) renderListBody(side, cw, rows int, isDropTarget bool) string {
	return strings.Join(m.renderListLines(side, cw, rows, isDropTarget), "\n")
}

// renderListLines draws only the visible window of the listing (windowing):
// a 50,000-entry directory costs the same per frame as a 50-entry one.
func (m filesModel) renderListLines(side, cw, rows int, isDropTarget bool) []string {
	pane := m.paneRefConst(side)
	end := pane.scroll + rows
	if end > len(pane.entries) {
		end = len(pane.entries)
	}
	start := pane.scroll
	if start < 0 {
		start = 0
	}
	lines := make([]string, 0, rows)
	for i := start; i < end; i++ {
		lines = append(lines, m.renderRow(side, i, cw, isDropTarget))
	}
	p := fmPal()
	for len(lines) < rows {
		lines = append(lines, p.pad(cw))
	}
	return lines
}

// renderRow draws one full-width file row, exactly cw columns wide.
func (m filesModel) renderRow(side, i, cw int, dropTargetPane bool) string {
	pane := m.paneRefConst(side)
	item := pane.entries[i]
	p := fmPal()
	focused := side == m.focus
	cursor := i == pane.index
	selected := cursor && focused
	marked := pane.selected[i]
	hovered := side == m.hoverSide && i == m.hoverIndex
	dropHover := dropTargetPane && i == m.hoverIndex && item.isDir

	glyph, kind := iconKindFor(item)
	name := fmSanitize(item.name)
	if (item.isDir || item.linkDir) && item.name != ".." {
		name += "/"
	}
	mark := " "
	if marked {
		mark = "✓"
	}
	c := listColumns(cw)

	sizeStr, modeStr, timeStr := "", "", ""
	if c.sizeW > 0 && item.name != ".." {
		if !item.isDir {
			sizeStr = humanSize(item.size)
		}
	}
	if c.modeW > 0 && item.name != ".." && item.mode != 0 {
		modeStr = os.FileMode(item.mode).String()
	}
	if c.timeW > 0 && !item.modTime.IsZero() {
		timeStr = fmtTime(item.modTime)
	}

	var rowPaint fmPaint
	full := true
	switch {
	case selected:
		rowPaint = p.sel
	case dropHover:
		rowPaint = p.drop
	case marked:
		rowPaint = p.mark
	case cursor && !focused:
		rowPaint = p.cursor
	case hovered && p.hover != (fmPaint{}):
		rowPaint = p.hover
	default:
		full = false
	}

	var b strings.Builder
	b.Grow(cw + 96)
	if full {
		var t strings.Builder
		t.WriteString(" ")
		t.WriteString(mark)
		t.WriteString(" ")
		t.WriteString(glyph)
		t.WriteString(" ")
		t.WriteString(fmPadRight(fmFitName(name, c.nameW), c.nameW))
		if c.sizeW > 0 {
			t.WriteString(" ")
			t.WriteString(fmPadLeft(sizeStr, c.sizeW))
		}
		if c.modeW > 0 {
			t.WriteString(" ")
			t.WriteString(fmPadRight(modeStr, c.modeW))
		}
		if c.timeW > 0 {
			t.WriteString(" ")
			t.WriteString(fmPadRight(timeStr, c.timeW))
		}
		t.WriteString(" ")
		b.WriteString(rowPaint.s(t.String()))
		return b.String()
	}

	zebra := i%2 == 1
	baseP, nameP, metaP, iconP := p.base, p.base, p.dim, p.icons[kind]
	if item.isDir || item.linkDir {
		nameP = p.dir
	}
	if zebra {
		baseP, metaP, iconP = p.zBase, p.zDim, p.iconsZ[kind]
		nameP = p.zBase
		if item.isDir || item.linkDir {
			nameP = p.zDir
		}
	}
	b.WriteString(baseP.s(" "))
	if marked {
		b.WriteString(baseP.s(mark))
	} else {
		b.WriteString(baseP.s(" "))
	}
	b.WriteString(baseP.s(" "))
	b.WriteString(iconP.s(glyph))
	b.WriteString(baseP.s(" "))
	b.WriteString(nameP.s(fmPadRight(fmFitName(name, c.nameW), c.nameW)))
	var meta strings.Builder
	if c.sizeW > 0 {
		meta.WriteString(" ")
		meta.WriteString(fmPadLeft(sizeStr, c.sizeW))
	}
	if c.modeW > 0 {
		meta.WriteString(" ")
		meta.WriteString(fmPadRight(modeStr, c.modeW))
	}
	if c.timeW > 0 {
		meta.WriteString(" ")
		meta.WriteString(fmPadRight(timeStr, c.timeW))
	}
	meta.WriteString(" ")
	b.WriteString(metaP.s(meta.String()))
	return b.String()
}

// fmtTime formats a modification time like ls: time of day for recent
// entries, the year for older ones. Always 12 columns.
func fmtTime(t time.Time) string {
	now := time.Now()
	if t.Year() == now.Year() || (now.Sub(t) < 180*24*time.Hour && now.Sub(t) > -24*time.Hour) {
		return t.Format("Jan 02 15:04")
	}
	return t.Format("Jan 02  2006")
}

// ---- grid ----

// renderGridBody draws a Finder-style icon grid (joined), kept for callers.
func (m filesModel) renderGridBody(side, cw, rows int, isDropTarget bool) string {
	return strings.Join(m.renderGridLines(side, cw, rows, isDropTarget, fmPal()), "\n")
}

// renderGridLines draws cells laid out in gridCols columns, each cell
// gridCellH lines tall (big icon over a centered name).
func (m filesModel) renderGridLines(side, cw, rows int, isDropTarget bool, p *fmPalette) []string {
	pane := m.paneRefConst(side)
	cols := gridCols(cw)
	cellW := cw / cols // distribute slack evenly across the row
	if cellW < 4 {
		cellW = 4
	}
	visRows := rows / gridCellH
	if visRows < 1 {
		visRows = 1
	}
	start := pane.scroll
	end := start + cols*visRows
	if end > len(pane.entries) {
		end = len(pane.entries)
	}
	var lines []string
	for i := start; i < end; i += cols {
		top := strings.Builder{}
		bot := strings.Builder{}
		used := 0
		for c := range cols {
			idx := i + c
			if idx >= end {
				break
			}
			a, b := m.renderGridCellLines(side, idx, cellW, isDropTarget, p)
			top.WriteString(a)
			bot.WriteString(b)
			used += cellW
		}
		if used < cw {
			top.WriteString(p.pad(cw - used))
			bot.WriteString(p.pad(cw - used))
		}
		lines = append(lines, top.String(), bot.String())
	}
	for len(lines) < rows {
		lines = append(lines, p.pad(cw))
	}
	if len(lines) > rows {
		lines = lines[:rows]
	}
	return lines
}

// renderGridCell draws a single icon+name cell (two lines joined by "\n").
func (m filesModel) renderGridCell(side, i, cellW int, dropTargetPane bool) string {
	a, b := m.renderGridCellLines(side, i, cellW, dropTargetPane, fmPal())
	return a + "\n" + b
}

func (m filesModel) renderGridCellLines(side, i, cellW int, dropTargetPane bool, p *fmPalette) (string, string) {
	pane := m.paneRefConst(side)
	item := pane.entries[i]
	focused := side == m.focus
	cursor := i == pane.index
	selected := cursor && focused
	marked := pane.selected[i]
	hovered := side == m.hoverSide && i == m.hoverIndex
	dropHover := dropTargetPane && i == m.hoverIndex && item.isDir

	glyph, kind := iconKindFor(item)
	name := fmSanitize(item.name)
	if (item.isDir || item.linkDir) && item.name != ".." {
		name += "/"
	}
	if marked {
		name = "✓ " + name
	}
	nameLine := centerCell(fmFitName(name, cellW-1), cellW)
	iconLine := centerCell(glyph, cellW)

	switch {
	case selected:
		return p.sel.s(iconLine), p.sel.s(nameLine)
	case dropHover:
		return p.drop.s(iconLine), p.drop.s(nameLine)
	case marked:
		return p.mark.s(iconLine), p.mark.s(nameLine)
	case cursor && !focused:
		return p.cursor.s(iconLine), p.cursor.s(nameLine)
	case hovered && p.hover != (fmPaint{}):
		return p.hover.s(iconLine), p.hover.s(nameLine)
	}
	nameP := p.base
	if item.isDir || item.linkDir {
		nameP = p.dir
	}
	return p.icons[kind].s(iconLine), nameP.s(nameLine)
}

// centerCell centers s within width w (display columns), padding with spaces.
func centerCell(s string, w int) string {
	sw := fmWidth(s)
	if sw >= w {
		return s
	}
	left := (w - sw) / 2
	right := w - sw - left
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
}

// ---- error state ----

// fmErrorInfo is a classified, human-oriented description of a listing error.
type fmErrorInfo struct {
	title string
	hint  []string
}

// classifyPaneError turns a raw listing error into a title and recovery
// hints. Remote errors arrive as text ("invalid_path: path is outside the
// agent's allowed file roots"), so matching is on message content.
func classifyPaneError(err error, pane paneState) fmErrorInfo {
	msg := strings.ToLower(err.Error())
	who := fmSanitize(pane.label())
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(msg, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("outside the agent's allowed file roots", "outside the selected file root", "is not permitted"):
		root := fmSanitize(pane.root)
		info := fmErrorInfo{title: "Outside the allowed file roots"}
		info.hint = append(info.hint, who+" only allows file access inside its --file-root.")
		if root != "" && root != "/" {
			info.hint = append(info.hint, "Allowed root: "+root)
		}
		info.hint = append(info.hint, "~ go to allowed root · ⌫ up · : go to path")
		return info
	case has("permission denied", "access is denied", "operation not permitted", "eacces"):
		return fmErrorInfo{title: "Permission denied", hint: []string{
			"You can't read this folder as the " + agentOrUser(pane) + ".",
			"⌫ go up · g retry · s switch source",
		}}
	case has("no such file", "not found", "does not exist", "cannot find"):
		return fmErrorInfo{title: "Folder not found", hint: []string{
			"It may have been moved or deleted.",
			"⌫ go up · ~ home · g retry",
		}}
	case has("not a directory"):
		return fmErrorInfo{title: "Not a folder", hint: []string{"⌫ go up · g retry"}}
	case pane.remote && has("connection refused", "no route to host", "i/o timeout", "timed out", "timeout",
		"connection reset", "broken pipe", "eof", "unreachable", "not connected", "dial", "handshake",
		"no reverse session", "not online", "offline", "closed network connection"):
		return fmErrorInfo{title: who + " is unreachable", hint: []string{
			"The agent did not answer. Check it with: fleet server reconnect " + who,
			"g retry · s switch source · Tab other pane",
		}}
	}
	return fmErrorInfo{title: "Couldn't list this folder", hint: []string{"g retry · ⌫ go up · s switch source"}}
}

func agentOrUser(p paneState) string {
	if p.remote {
		return "agent's user"
	}
	return "current user"
}

func (m filesModel) renderPaneError(side, cw, rows int, p *fmPalette) []string {
	pane := m.paneRefConst(side)
	info := classifyPaneError(pane.err, pane)
	lines := []fmStyledLine{{"⚠  " + info.title, p.danger}, {"", p.base}}
	raw := fmSanitize(pane.err.Error())
	wrapped := fmWrap(raw, cw-4)
	if len(wrapped) > 3 {
		wrapped = append(wrapped[:2], fmFit(wrapped[2], cw-5)+"…")
	}
	for _, w := range wrapped {
		lines = append(lines, fmStyledLine{w, p.dim})
	}
	lines = append(lines, fmStyledLine{"", p.base})
	for _, h := range info.hint {
		for _, w := range fmWrap(h, cw-4) {
			lines = append(lines, fmStyledLine{w, p.muted})
		}
	}
	return centeredBlock(cw, rows, p, lines)
}

// ============================================================================
// Icons
// ============================================================================

type fmIconKind int

const (
	iconKindDefault fmIconKind = iota
	iconKindUp
	iconKindDir
	iconKindSymlink
	iconKindCode
	iconKindDoc
	iconKindData
	iconKindImage
	iconKindArchive
	iconKindDocsRed
	iconKindMedia
	iconKindExec
	iconKindConfig
)

func fmIconStyles() map[fmIconKind]lipgloss.Style {
	return map[fmIconKind]lipgloss.Style{
		iconKindDefault: lipgloss.NewStyle().Foreground(fmMutedC),
		iconKindUp:      lipgloss.NewStyle().Foreground(fmMutedC).Bold(true),
		iconKindDir:     lipgloss.NewStyle().Foreground(fmDirC).Bold(true),
		iconKindSymlink: lipgloss.NewStyle().Foreground(fmDimC),
		iconKindCode:    lipgloss.NewStyle().Foreground(fmCodeC).Bold(true),
		iconKindDoc:     lipgloss.NewStyle().Foreground(fmDocC),
		iconKindData:    lipgloss.NewStyle().Foreground(fmDataC),
		iconKindImage:   lipgloss.NewStyle().Foreground(fmImageC),
		iconKindArchive: lipgloss.NewStyle().Foreground(fmArchiveC),
		iconKindDocsRed: lipgloss.NewStyle().Foreground(fmDocsRedC),
		iconKindMedia:   lipgloss.NewStyle().Foreground(fmMediaC),
		iconKindExec:    lipgloss.NewStyle().Foreground(fmExecC).Bold(true),
		iconKindConfig:  lipgloss.NewStyle().Foreground(fmConfigC),
	}
}

// iconFor is the single source of truth for a file item's icon: it returns a
// crisp, single-terminal-cell-wide glyph and a palette-consistent lipgloss style
// (color, weight) describing its file-type category. Both the list view and the
// grid/icon view use it (via iconKindFor) so the two stay in sync.
func iconFor(item fileItem) (glyph string, style lipgloss.Style) {
	g, k := iconKindFor(item)
	return g, fmIconStyles()[k]
}

func iconKindFor(item fileItem) (string, fmIconKind) {
	switch {
	case item.name == "..":
		return "⬑", iconKindUp
	case item.isDir:
		return "▣", iconKindDir
	case item.symlink:
		if item.linkDir {
			return "↳", iconKindDir
		}
		return "↳", iconKindSymlink
	}

	ext := strings.ToLower(filepath.Ext(item.name))
	name := strings.ToLower(item.name)

	if strings.HasPrefix(item.name, ".") && (ext == "" || ext == name) {
		return "✦", iconKindConfig
	}

	switch ext {
	case ".go", ".rs", ".c", ".h", ".hpp", ".cpp", ".cc", ".py", ".js", ".jsx",
		".ts", ".tsx", ".java", ".rb", ".sh", ".bash", ".zsh", ".php", ".lua",
		".swift", ".kt", ".scala", ".pl", ".sql", ".html", ".css", ".scss", ".vue":
		return "λ", iconKindCode
	case ".md", ".markdown", ".txt", ".rst", ".log", ".csv", ".tsv", ".rtf", ".tex":
		return "≡", iconKindDoc
	case ".json", ".yaml", ".yml", ".toml", ".ini", ".cfg", ".conf", ".xml",
		".env", ".properties", ".lock":
		return "◈", iconKindData
	case ".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".bmp", ".ico",
		".tiff", ".heic":
		return "❖", iconKindImage
	case ".zip", ".tar", ".gz", ".tgz", ".bz2", ".xz", ".7z", ".rar", ".zst",
		".lz", ".lzma", ".deb", ".rpm":
		return "▤", iconKindArchive
	case ".pdf", ".doc", ".docx", ".ppt", ".pptx", ".xls", ".xlsx", ".odt", ".epub":
		return "▥", iconKindDocsRed
	case ".mp3", ".wav", ".flac", ".ogg", ".m4a", ".aac",
		".mp4", ".mkv", ".mov", ".avi", ".webm", ".flv", ".wmv":
		return "♪", iconKindMedia
	case ".exe", ".bin", ".so", ".dylib", ".dll", ".o", ".a", ".app", ".out":
		return "⚙", iconKindExec
	}
	switch name {
	case "dockerfile", "makefile", "containerfile", "justfile", "vagrantfile":
		return "λ", iconKindCode
	}

	if item.mode&0o111 != 0 {
		return "⚙", iconKindExec
	}

	return "•", iconKindDefault
}

// ============================================================================
// Status bar & footer
// ============================================================================

// fmLevel is the severity of a status message.
type fmLevel int

const (
	levelInfo fmLevel = iota
	levelOK
	levelWarn
	levelError
)

// statusLevelOf returns the severity for the current status text: the level
// recorded by setStatus when it applies to this exact text, otherwise a
// best-effort classification of the wording.
func (m filesModel) statusLevelOf() fmLevel {
	if m.statusLevelText == m.status && m.status != "" {
		return m.statusLevel
	}
	return classifyStatus(m.status)
}

func classifyStatus(s string) fmLevel {
	low := strings.ToLower(s)
	has := func(subs ...string) bool {
		for _, x := range subs {
			if strings.Contains(low, x) {
				return true
			}
		}
		return false
	}
	switch {
	case has("failed", "error", "invalid", "refused", "denied", "unreachable", "collision", "cannot represent", "cannot inspect"):
		return levelError
	case has("cancelled", "select ", "cannot ", "not a recognised", "nothing to", "no ", "already"):
		return levelWarn
	case has("completed", "created", "deleted", "renamed", "saved", "copied", "moved", "extracted",
		"compressed", "duplicated", "set ", "sha-256 ", "bookmarked", "started", "queued"):
		return levelOK
	}
	return levelInfo
}

func (m filesModel) renderStatusLine(l fmLayout, p *fmPalette) string {
	w := l.innerW
	// Right-hand chips.
	var chips []string
	var chipPaints []fmPaint
	add := func(s string, pt fmPaint) {
		chips = append(chips, s)
		chipPaints = append(chipPaints, pt)
	}
	pane := m.paneRefConst(m.focus)
	if st := m.selectionStats(m.focus); st.count > 0 {
		add(fmt.Sprintf(" ✓ %d · %s ", st.count, st.sizeText()), p.statKey)
	}
	add(" "+pane.sortBy.label()+" "+sortArrow(pane.sortDesc)+" ", p.statDim)
	if pane.visual {
		add(" RANGE ", p.statWarn)
	}
	if free, ok := m.freeSpace(m.focus); ok {
		add(" "+humanSize(free)+" free ", p.statDim)
	}
	if !l.footer {
		add(" ? help ", p.statKey)
	}
	chipW := 0
	for _, c := range chips {
		chipW += fmWidth(c)
	}
	// Drop chips from the left while the message area would get too narrow.
	for len(chips) > 1 && w-chipW < 24 {
		chipW -= fmWidth(chips[0])
		chips, chipPaints = chips[1:], chipPaints[1:]
	}

	// Left: the transient message, or a mini status of the focused item.
	msgW := w - chipW
	var left string
	if m.status != "" {
		lvl := m.statusLevelOf()
		icon, pt := "•", p.statInfo
		switch lvl {
		case levelOK:
			icon, pt = "✓", p.statOk
		case levelWarn:
			icon, pt = "!", p.statWarn
		case levelError:
			icon, pt = "✗", p.statErr
		}
		txt := " " + icon + " " + fmSanitize(m.status)
		txt = fmPadRight(txt, msgW)
		left = pt.s(txt)
	} else {
		left = m.miniStatus(msgW, p)
	}
	var b strings.Builder
	b.WriteString(left)
	for i, c := range chips {
		b.WriteString(chipPaints[i].s(c))
	}
	return p.pad(l.padX) + b.String() + p.pad(l.w-l.padX-w)
}

// miniStatus describes the focused entry when no message is showing: its full
// (untruncated) name, size, mode and time, like Midnight Commander's mini
// status line.
func (m filesModel) miniStatus(w int, p *fmPalette) string {
	pane := m.paneRefConst(m.focus)
	it := m.focusedItem(m.focus)
	if pane.loading && pane.listedCwd != pane.cwd {
		return p.statMuted.s(fmPadRight(" loading "+fmSanitize(pane.cwd)+"…", w))
	}
	if it.name == "" || it.name == ".." {
		return p.statMuted.s(fmPadRight(" "+fmSanitize(pane.label())+"  "+fmSanitize(pane.cwd), w))
	}
	var details []string
	switch {
	case it.isDir:
		details = append(details, "folder")
	case it.symlink:
		details = append(details, "symlink")
	default:
		details = append(details, humanSize(it.size))
	}
	if it.mode != 0 {
		details = append(details, os.FileMode(it.mode).String())
	}
	if !it.modTime.IsZero() {
		details = append(details, it.modTime.Format("2006-01-02 15:04"))
	}
	meta := "  " + strings.Join(details, "  ")
	nameW := w - 1 - fmWidth(meta)
	if nameW < 8 {
		meta = ""
		nameW = w - 1
	}
	name := fmFitName(fmSanitize(it.name), nameW)
	return p.statBase.s(" "+name) + p.statDim.s(fmPadRight(meta, w-1-fmWidth(name)))
}

func (m filesModel) renderFooterLine(l fmLayout, p *fmPalette) string {
	w := l.innerW
	hints := [][2]string{
		{"?", "help"}, {"↵", "open"}, {"space", "select"}, {"c/m", "copy/move"},
		{"e", "edit"}, {"P", "preview"}, {"f", "jump"}, {":", "go to"}, {"t", "transfers"},
		{"d", "del"}, {"/", "filter"}, {"drag", "transfer"}, {"q", "quit"},
	}
	var b strings.Builder
	x := 0
	b.WriteString(p.base.s(" "))
	x++
	for _, h := range hints {
		chip := " " + h[0] + " "
		label := " " + h[1] + "  "
		cw := fmWidth(chip) + fmWidth(label)
		if x+cw > w {
			break
		}
		b.WriteString(p.keyChip.s(chip))
		b.WriteString(p.hintLabel.s(label))
		x += cw
	}
	if x < w {
		b.WriteString(p.pad(w - x))
	}
	return p.pad(l.padX) + b.String() + p.pad(l.w-l.padX-w)
}

// ============================================================================
// Overlays (centered modals + cursor-anchored popups)
// ============================================================================

func (m filesModel) composeOverlay(base string) string {
	switch m.overlay {
	case overlaySourcePicker:
		return overlayCenter(base, m.width, m.height, m.renderSourcePicker())
	case overlayContextMenu:
		return overlayAt(base, m.menuX, m.menuY, m.renderContextMenu())
	case overlayCopyMove:
		return overlayAt(base, m.cmX, m.cmY, m.renderCopyMoveMenu())
	case overlayConfirm:
		return overlayCenter(base, m.width, m.height, m.renderConfirm())
	case overlayPrompt:
		return overlayCenter(base, m.width, m.height, m.renderPrompt())
	case overlayProperties:
		return overlayCenter(base, m.width, m.height, m.renderProperties())
	case overlayEditor:
		return m.renderEditor()
	case overlayFilter:
		return overlayCenter(base, m.width, m.height, m.renderFilter())
	case overlayCompress:
		return overlayCenter(base, m.width, m.height, m.renderCompress())
	case overlayHelp:
		return overlayCenter(base, m.width, m.height, m.renderHelp())
	case overlayGoto:
		return overlayCenter(base, m.width, m.height, m.renderGoto())
	case overlayJump:
		return overlayCenter(base, m.width, m.height, m.renderJump())
	case overlayPlaces:
		return overlayCenter(base, m.width, m.height, m.renderPlaces())
	}
	return base
}

// dialogWidth is the usable text width inside a centered overlay box.
func (m filesModel) dialogWidth(want int) int {
	max := m.width - 8 // border (2) + padding (4) + margin
	if max < 20 {
		max = 20
	}
	if want > max {
		return max
	}
	return want
}

func renderInput(value string, w int) string {
	field := fmSanitize(value) + "▏"
	if fmWidth(field) > w-2 {
		field = "…" + fmSuffixWithin(field, w-3)
	}
	return fmInputSty.Width(w).Render(field)
}

// renderCompress draws the archive overlay: a one-line summary of what's being
// compressed, a clickable format chip (← → to cycle), the editable archive-name
// field, and Compress / Cancel actions. The format chip and buttons are
// zone.Mark'ed so they're clickable.
func (m filesModel) renderCompress() string {
	formats := core.ArchiveFormats()
	format := formats[m.compressFormat]
	dw := m.dialogWidth(44)

	var b strings.Builder
	b.WriteString(fmTitleSty.Render("Compress " + fmSanitize(m.paneRefConst(m.compressSide).label())))
	b.WriteString("\n\n")

	what := fmt.Sprintf("%d item(s)", len(m.compressNames))
	if len(m.compressNames) == 1 {
		what = "'" + fmFitName(fmSanitize(m.compressNames[0]), dw-12) + "'"
	}
	b.WriteString(fmStatusSty.Render("Archiving " + what))
	b.WriteString("\n\n")

	// Format chip with arrows hinting it can be cycled.
	chip := lipgloss.NewStyle().
		Background(fmBorderC).Foreground(fmAccent2).Bold(true).Padding(0, 1).
		Render("‹ " + format + " ›")
	b.WriteString(fmHintLabel.Render("Format  "))
	b.WriteString(zone.Mark(fmCompressPrefix+"format", chip))
	b.WriteString("\n\n")

	// Editable name field.
	b.WriteString(fmHintLabel.Render("Name"))
	b.WriteString("\n")
	b.WriteString(renderInput(m.compressName, dw))
	b.WriteString("\n\n")

	okBtn := zone.Mark(fmCompressPrefix+"ok",
		lipgloss.NewStyle().Background(fmAccent).Foreground(fmInk).Bold(true).Padding(0, 1).Render("Compress"))
	cancelBtn := zone.Mark(fmCompressPrefix+"cancel",
		lipgloss.NewStyle().Foreground(fmDimC).Padding(0, 1).Render("Cancel"))
	b.WriteString(okBtn + "  " + cancelBtn)
	b.WriteString("\n\n")
	b.WriteString(fmStatusSty.Render("←→ format · type name · ↵ compress · esc cancel"))
	return fmOverlayBox.Render(b.String())
}

func (m filesModel) renderFilter() string {
	side := m.filterSide
	pane := m.paneRefConst(side)
	dw := m.dialogWidth(44)
	var b strings.Builder
	b.WriteString(fmTitleSty.Render("Filter " + fmSanitize(pane.label()) + " by name"))
	b.WriteString("\n\n")
	b.WriteString(renderInput(pane.filter, dw))
	b.WriteString("\n")
	b.WriteString(fmStatusSty.Render(fmt.Sprintf("  %d match(es)", countReal(pane.entries))))
	b.WriteString("\n\n")
	b.WriteString(fmStatusSty.Render("type to narrow · ↵ apply · esc clear"))
	return fmOverlayBox.Render(b.String())
}

func (m filesModel) renderSourcePicker() string {
	var b strings.Builder
	b.WriteString(fmTitleSty.Render("Open source in " + sideName(m.pickerSide) + " pane"))
	b.WriteString("\n\n")
	for i, it := range m.pickerItems {
		icon := fmLocalGlyph
		dot := ""
		if it != "Local" {
			icon = fmRemoteGlyph
			// A small reachability indicator for servers.
			reachable := false
			for _, s := range m.servers {
				if s.Name == it {
					reachable = s.Observed.Reachable
					break
				}
			}
			if reachable {
				dot = lipgloss.NewStyle().Foreground(fmAccent).Render(" ●")
			} else {
				dot = lipgloss.NewStyle().Foreground(fmDimC).Render(" ○")
			}
		}
		last := ""
		if p, ok := m.lastPath[pickerSource(it)]; ok {
			last = "  " + fmFitPathLeft(fmSanitize(p), 28)
		}
		line := fmt.Sprintf(" %s  %s%s ", icon, fmPadRight(fmSanitize(it), 14), dot)
		var styled string
		if i == m.pickerIndex {
			styled = fmSelRow.Render(line) + fmDimSty.Render(last)
		} else {
			styled = fmTextSty.Render(line) + fmDimSty.Render(last)
		}
		b.WriteString(zone.Mark(fmt.Sprintf("%s%d", fmPickPrefix, i), styled))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(fmStatusSty.Render("↑↓ choose · ↵ open · esc cancel"))
	return fmOverlayBox.Render(b.String())
}

func pickerSource(label string) string {
	if label == "Local" {
		return ""
	}
	return label
}

func (m filesModel) renderContextMenu() string {
	var b strings.Builder
	if m.menuTitle != "" {
		b.WriteString(fmDimSty.Render(" " + m.menuTitle))
		b.WriteString("\n")
	}
	labelW := 0
	for _, it := range m.menuItems {
		if w := fmWidth(it.label); w > labelW {
			labelW = w
		}
	}
	for i, it := range m.menuItems {
		label := fmPadRight(it.label, labelW)
		var line string
		switch {
		case it.separator:
			line = fmRule.Render(strings.Repeat("─", labelW+6))
		case !it.enabled:
			line = lipgloss.NewStyle().Foreground(fmDimC).Render(fmt.Sprintf(" %-2s  %s ", it.key, label))
		case i == m.menuIndex:
			line = fmSelRow.Render(fmt.Sprintf(" %-2s  %s ", it.key, label))
		default:
			key := lipgloss.NewStyle().Foreground(fmAccent2).Render(fmt.Sprintf("%-2s", it.key))
			line = fmt.Sprintf(" %s  %s ", key, fmTextSty.Render(label))
		}
		b.WriteString(zone.Mark(fmt.Sprintf("%s%d", fmMenuPrefix, i), line))
		if i < len(m.menuItems)-1 {
			b.WriteString("\n")
		}
	}
	return fmMenuBox.Render(b.String())
}

func (m filesModel) renderCopyMoveMenu() string {
	copyBtn := " Copy here "
	moveBtn := " Move here "
	cancelBtn := " Cancel "
	if m.cmIndex == 0 {
		copyBtn = fmSelRow.Render(copyBtn)
		moveBtn = fmTextSty.Render(moveBtn)
	} else {
		copyBtn = fmTextSty.Render(copyBtn)
		moveBtn = fmSelRow.Render(moveBtn)
	}
	cancelBtn = lipgloss.NewStyle().Foreground(fmDimC).Render(cancelBtn)
	row := zone.Mark(fmCMPrefix+"copy", copyBtn) +
		fmRule.Render(" · ") +
		zone.Mark(fmCMPrefix+"move", moveBtn) +
		fmRule.Render(" · ") +
		zone.Mark(fmCMPrefix+"cancel", cancelBtn)
	hint := fmStatusSty.Render("c copy · m move · esc cancel")
	return fmMenuBox.Render(row + "\n" + hint)
}

func (m filesModel) renderConfirm() string {
	if m.confirm == confirmTransfer && m.plan != nil {
		return m.renderTransferConfirm()
	}
	title := "Confirm"
	titleStyle := fmWarnSty
	if m.confirm == confirmDelete {
		title = "Delete"
		titleStyle = fmErrSty
	}
	dw := m.dialogWidth(64)
	var b strings.Builder
	b.WriteString(titleStyle.Render("⚠  " + title))
	b.WriteString("\n\n")
	if m.confirm == confirmDelete && len(m.deleteItems) > 0 {
		b.WriteString(m.renderDeleteBody(dw))
	} else {
		for _, ln := range fmWrap(fmSanitize(m.confirmText), dw) {
			b.WriteString(fmTextSty.Render(ln))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	verb := " confirm   "
	if m.confirm == confirmDelete {
		verb = " delete   "
	}
	b.WriteString(fmDoneSty.Render("Enter/y") + fmStatusSty.Render(verb) +
		fmErrSty.Render("Esc/n") + fmStatusSty.Render(" cancel"))
	box := fmOverlayBox
	if m.confirm == confirmDelete {
		box = box.BorderForeground(fmDangerC)
	} else {
		box = box.BorderForeground(fmWarnC)
	}
	return box.Render(b.String())
}

// renderDeleteBody lists exactly what will be deleted: counts, known sizes,
// the location and the first few names.
func (m filesModel) renderDeleteBody(dw int) string {
	pane := m.paneRefConst(m.deleteSide)
	files, dirs := 0, 0
	var bytes int64
	for _, it := range m.deleteItems {
		if it.isDir {
			dirs++
		} else {
			files++
			bytes += it.size
		}
	}
	var parts []string
	if files > 0 {
		parts = append(parts, fmt.Sprintf("%s (%s)", plural(files, "file", "files"), humanSize(bytes)))
	}
	if dirs > 0 {
		parts = append(parts, plural(dirs, "folder", "folders")+" and everything inside")
	}
	var b strings.Builder
	head := fmt.Sprintf("Permanently delete %s from %s?", strings.Join(parts, " + "), fmSanitize(pane.label()))
	for _, ln := range fmWrap(head, dw) {
		b.WriteString(fmTextSty.Render(ln))
		b.WriteString("\n")
	}
	b.WriteString(fmDimSty.Render(fmFitPathLeft(fmSanitize(pane.cwd), dw)))
	b.WriteString("\n\n")
	b.WriteString(itemList(m.deleteItems, dw, 6))
	b.WriteString("\n")
	b.WriteString(fmErrSty.Render("This cannot be undone."))
	b.WriteString("\n")
	return b.String()
}

// itemList renders up to max items as "• name   size" lines plus "+N more".
func itemList(items []fileItem, dw, max int) string {
	var b strings.Builder
	for i, it := range items {
		if i >= max {
			b.WriteString(fmDimSty.Render(fmt.Sprintf("  … and %d more", len(items)-max)))
			b.WriteString("\n")
			break
		}
		name := fmSanitize(it.name)
		meta := humanSize(it.size)
		if it.isDir {
			name += "/"
			meta = "folder"
		}
		nameW := dw - 4 - 10
		if nameW < 8 {
			nameW = 8
		}
		b.WriteString(fmTextSty.Render("  • " + fmPadRight(fmFitName(name, nameW), nameW)))
		b.WriteString(fmDimSty.Render(fmPadLeft(meta, 10)))
		b.WriteString("\n")
	}
	return b.String()
}

func (m filesModel) renderPrompt() string {
	dw := m.dialogWidth(48)
	var b strings.Builder
	for i, ln := range fmWrap(fmSanitize(m.promptLabel), dw) {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(fmTitleSty.Render(ln))
	}
	b.WriteString("\n\n")
	b.WriteString(renderInput(m.promptValue, dw))
	b.WriteString("\n\n")
	b.WriteString(fmStatusSty.Render("↵ confirm · esc cancel · ctrl+u clear"))
	return fmOverlayBox.Render(b.String())
}

func (m filesModel) renderProperties() string {
	dw := m.dialogWidth(76)
	var b strings.Builder
	b.WriteString(fmTitleSty.Render("Properties"))
	b.WriteString("\n\n")
	for _, raw := range strings.Split(m.propsText, "\n") {
		// "Label:   value" — wrap long values under the value column.
		label, value := raw, ""
		if i := strings.Index(raw, ":"); i > 0 && i < 12 {
			label, value = raw[:i+1], strings.TrimLeft(raw[i+1:], " ")
		}
		labelW := 10
		if value == "" {
			for _, ln := range fmWrap(fmSanitize(raw), dw) {
				b.WriteString(fmTextSty.Render(ln))
				b.WriteString("\n")
			}
			continue
		}
		for i, ln := range fmWrap(fmSanitize(value), dw-labelW) {
			if i == 0 {
				b.WriteString(fmDimSty.Render(fmPadRight(label, labelW)))
			} else {
				b.WriteString(strings.Repeat(" ", labelW))
			}
			b.WriteString(fmTextSty.Render(ln))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(fmStatusSty.Render("esc / ↵ close"))
	return fmOverlayBox.Render(b.String())
}

// ============================================================================
// Editor overlay (full-screen viewer + editor)
// ============================================================================

// renderEditor draws the full-screen editor: a titled, bordered box containing
// either the syntax-highlighted read-only viewer or the plain editable textarea,
// with a footer of key hints and inline status. It returns a complete frame
// (it deliberately covers the whole screen, not a small popup).
func (m filesModel) renderEditor() string {
	ed := m.editor
	bw, bh := m.editorAreaSize()

	// Header: file name + source + mode + dirty marker.
	srcLabel := "Local"
	if ed.source != "" {
		srcLabel = ed.source
	}
	modeTag := lipgloss.NewStyle().Foreground(fmDimC).Render("VIEW")
	if ed.mode == editorEdit {
		modeTag = lipgloss.NewStyle().Foreground(fmInk).Background(fmAccent).Bold(true).Padding(0, 1).Render("EDIT")
	}
	dirty := ""
	if ed.dirty {
		dirty = lipgloss.NewStyle().Foreground(fmWarnC).Bold(true).Render(" ●")
	}
	src := fmServerTag.Render("  " + sourceGlyph(ed.source != "") + " " + fmSanitize(srcLabel))
	nameW := bw - lipgloss.Width(src) - lipgloss.Width(modeTag) - 6
	title := fmTitleSty.Render("✎ "+fmFitName(fmSanitize(ed.name), nameW)) + dirty
	headLeft := title + src
	gap := bw - lipgloss.Width(headLeft) - lipgloss.Width(modeTag)
	if gap < 1 {
		gap = 1
	}
	header := headLeft + strings.Repeat(" ", gap) + modeTag

	// Body.
	var body string
	if ed.mode == editorEdit {
		body = ed.area.View()
	} else {
		body = m.renderEditorViewer(bw, bh)
	}

	// Footer hints + inline status.
	hints := []struct{ k, v string }{
		{"tab", "view/edit"}, {"^s", "save"}, {"esc", "close"},
		{"↑↓", "scroll"},
	}
	parts := make([]string, 0, len(hints))
	for _, h := range hints {
		parts = append(parts, fmKeyChip.Render(h.k)+" "+fmHintLabel.Render(h.v))
	}
	footLeft := strings.Join(parts, "  ")
	statusTxt := fmSanitize(ed.status)
	statusSty := fmStatusSty
	switch {
	case strings.HasPrefix(statusTxt, "save failed"), strings.HasPrefix(statusTxt, "open failed"):
		statusSty = fmErrSty
	case strings.HasPrefix(statusTxt, "saved"):
		statusSty = fmDoneSty
	}
	footRight := statusSty.Render(fmFit(statusTxt, bw/2))
	fgap := bw - lipgloss.Width(footLeft) - lipgloss.Width(footRight)
	if fgap < 1 {
		fgap = 1
	}
	footer := footLeft + strings.Repeat(" ", fgap) + footRight

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(fmAccent).
		Background(fmPanelBg).
		Padding(0, 1).
		Width(bw)
	inner := header + "\n" + fmRule.Render(strings.Repeat("─", bw)) + "\n" + body + "\n" +
		fmRule.Render(strings.Repeat("─", bw)) + "\n" + footer
	return pageStyle.Render(box.Render(inner))
}

// renderEditorViewer renders the highlighted read-only content windowed to the
// visible body height, with line numbers.
func (m filesModel) renderEditorViewer(bw, bh int) string {
	ed := m.editor
	lines := ed.viewLines
	if lines == nil {
		lines = highlightLines(ed.name, ed.area.Value())
	}
	total := len(lines)

	top := ed.viewScrl
	if top > total-bh {
		top = total - bh
	}
	if top < 0 {
		top = 0
	}
	end := top + bh
	if end > total {
		end = total
	}

	gutterW := len(strconv.Itoa(total))
	if gutterW < 2 {
		gutterW = 2
	}
	numSty := lipgloss.NewStyle().Foreground(fmDimC)
	contentW := bw - gutterW - 1
	if contentW < 1 {
		contentW = 1
	}

	out := make([]string, 0, bh)
	for i := top; i < end; i++ {
		num := numSty.Render(fmt.Sprintf("%*d", gutterW, i+1))
		line := truncateANSI(lines[i], contentW)
		out = append(out, num+" "+line)
	}
	// Pad to full height so the box stays stable.
	for len(out) < bh {
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}

// ============================================================================
// Drag ghost (floating, follows cursor)
// ============================================================================

func (m filesModel) composeGhost(base string) string {
	if m.drag == nil || !m.drag.active {
		return base
	}
	d := m.drag
	icon, _ := iconKindFor(d.primary)
	name := fmFitName(fmSanitize(d.primary.name), 18)
	label := fmt.Sprintf(" %s %s ", icon, name)
	if len(d.items) > 1 {
		label += lipgloss.NewStyle().Background(fmAccent).Foreground(fmInk).Bold(true).
			Render(fmt.Sprintf(" +%d ", len(d.items)-1))
	}
	ghost := lipgloss.NewStyle().
		Background(fmColor("#10303a")).
		Foreground(fmAccent2).
		Bold(true).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(fmAccent).
		Render(label)

	x, y := m.mouseX+1, m.mouseY-1
	if d.snapping {
		x, y = d.snapX+1, d.snapY-1
	}
	return overlayAt(base, x, y, ghost)
}

// ============================================================================
// Overlay compositing primitives
// ============================================================================

// overlayCenter draws box centered over base.
func overlayCenter(base string, w, h int, box string) string {
	bw, bh := lipgloss.Width(box), lipgloss.Height(box)
	x := (w - bw) / 2
	y := (h - bh) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	return overlayAt(base, x, y, box)
}

// overlayAt composites box onto base with its top-left at (x, y), clamped to the
// screen. It overwrites the underlying cells line-by-line (no alpha blending,
// but lipgloss styling on the box paints its own background).
func overlayAt(base string, x, y int, box string) string {
	baseLines := strings.Split(base, "\n")
	boxLines := strings.Split(box, "\n")
	boxW := lipgloss.Width(box)

	if y < 0 {
		y = 0
	}
	if x < 0 {
		x = 0
	}
	// Keep the box on-screen horizontally when the base is wide enough.
	if len(baseLines) > 0 {
		if bw := lipgloss.Width(baseLines[0]); bw > 0 && x+boxW > bw {
			x = bw - boxW
			if x < 0 {
				x = 0
			}
		}
	}
	// Clamp so the box stays on-screen vertically.
	if y+len(boxLines) > len(baseLines) {
		y = len(baseLines) - len(boxLines)
		if y < 0 {
			y = 0
		}
	}

	for i, bl := range boxLines {
		row := y + i
		if row < 0 || row >= len(baseLines) {
			continue
		}
		baseLines[row] = overlayLine(baseLines[row], bl, x, boxW)
	}
	return strings.Join(baseLines, "\n")
}

// overlayLine splices overlay into base starting at visible column x. It operates
// on rune cells while preserving ANSI styling by re-truncating with lipgloss.
func overlayLine(base, overlay string, x, overlayW int) string {
	baseW := lipgloss.Width(base)
	// left part of base [0, x)
	left := truncateANSI(base, x)
	leftW := lipgloss.Width(left)
	if leftW < x {
		left += strings.Repeat(" ", x-leftW)
	}
	// right part of base after the overlay region.
	rightStart := x + overlayW
	right := ""
	if rightStart < baseW {
		right = dropANSI(base, rightStart)
	}
	return left + "\x1b[0m" + overlay + "\x1b[0m" + right
}

// ============================================================================
// ANSI-aware horizontal slicing helpers
// ============================================================================

// truncateANSI returns the prefix of s that occupies the first n display columns,
// preserving styling.
func truncateANSI(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= n {
		return s
	}
	return truncateVisible(s, n)
}

// dropANSI returns the suffix of s beginning at display column n.
func dropANSI(s string, n int) string {
	if n <= 0 {
		return s
	}
	total := lipgloss.Width(s)
	if n >= total {
		return ""
	}
	return trimVisibleLeft(s, n)
}

// isCSIFinal reports whether b terminates a CSI escape sequence.
func isCSIFinal(r rune) bool { return r >= 0x40 && r <= 0x7e && r != '[' }

// truncateVisible walks the string honoring ANSI escapes (and wide runes),
// returning the prefix covering n visible columns.
func truncateVisible(s string, n int) string {
	var out strings.Builder
	visible := 0
	inEsc := false
	for _, r := range s {
		if r == '\x1b' {
			inEsc = true
			out.WriteRune(r)
			continue
		}
		if inEsc {
			out.WriteRune(r)
			if isCSIFinal(r) {
				inEsc = false
			}
			continue
		}
		rw := fmRuneWidth(r)
		if visible+rw > n {
			break
		}
		out.WriteRune(r)
		visible += rw
	}
	return out.String()
}

// trimVisibleLeft drops the first n visible columns, preserving any active ANSI
// state by carrying through escape sequences.
func trimVisibleLeft(s string, n int) string {
	var out strings.Builder
	visible := 0
	inEsc := false
	for _, r := range s {
		if r == '\x1b' {
			inEsc = true
			out.WriteRune(r)
			continue
		}
		if inEsc {
			out.WriteRune(r)
			if isCSIFinal(r) {
				inEsc = false
			}
			continue
		}
		if visible < n {
			visible += fmRuneWidth(r)
			if visible > n {
				// A wide rune straddled the cut: keep alignment with a space.
				out.WriteByte(' ')
			}
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// ============================================================================
// Misc render helpers
// ============================================================================

func countReal(items []fileItem) int {
	n := 0
	for _, it := range items {
		if it.name != ".." {
			n++
		}
	}
	return n
}

func formatETA(t *transferRow) string {
	if t.rate <= 0 || t.total <= 0 || t.bytesDone >= t.total {
		return "—"
	}
	secs := float64(t.total-t.bytesDone) / t.rate
	return formatDuration(secs)
}

func formatDuration(secs float64) string {
	if secs < 1 {
		return "<1s"
	}
	s := int(secs)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTPE"[exp])
}

// renderBreadcrumbWithin renders only paths at or below root. It falls back to
// filesystem-root breadcrumbs for legacy pane states with no boundary.
func renderBreadcrumbWithin(cwd, root string, style core.TargetPathStyle, width int) string {
	segs := breadcrumbSegmentsWithin(cwd, root, style)
	return renderBreadcrumbSegments(segs, width)
}

func breadcrumbSegmentsWithin(cwd, root string, style core.TargetPathStyle) []string {
	if root == "" {
		return breadcrumbSegments(cwd, style)
	}
	root = style.Clean(root)
	rel, err := style.Relative(root, style.Clean(cwd))
	if err != nil || rel == "." {
		return []string{root}
	}
	return append([]string{root}, strings.Split(rel, "/")...)
}

// renderBreadcrumb renders a path as accented segments separated by ›, trimming
// leading segments to fit the width.
func renderBreadcrumb(cwd string, style core.TargetPathStyle, width int) string {
	return renderBreadcrumbSegments(breadcrumbSegments(cwd, style), width)
}

func renderBreadcrumbSegments(segs []string, width int) string {
	if len(segs) == 0 {
		return ""
	}
	// Build from the right until we run out of width.
	sepGlyph := fmCrumbSep.Render(" › ")
	var rendered []string
	for i, s := range segs {
		style := fmCrumbSty
		if i == len(segs)-1 {
			style = fmCrumbCur
		}
		rendered = append(rendered, style.Render(fmSanitize(s)))
	}
	full := strings.Join(rendered, sepGlyph)
	if lipgloss.Width(full) <= width {
		return full
	}
	// Trim from the left, keeping the tail segments visible.
	for start := 1; start < len(rendered); start++ {
		candidate := fmCrumbSty.Render("…") + sepGlyph + strings.Join(rendered[start:], sepGlyph)
		if lipgloss.Width(candidate) <= width {
			return candidate
		}
	}
	// Fall back to a plain truncated tail.
	return fmCrumbCur.Render(fmFit(fmSanitize(segs[len(segs)-1]), width))
}

// breadcrumbSegments splits an absolute target path without consulting the
// controller filesystem. The first segment is the target root (/, C:\, or a
// UNC share root), followed by each directory component.
func breadcrumbSegments(cwd string, style core.TargetPathStyle) []string {
	current := style.Clean(cwd)
	if current == "." || style.IsRoot(current) {
		return []string{current}
	}

	var tail []string
	for !style.IsRoot(current) {
		parent := style.Dir(current)
		base := style.Base(current)
		if parent == current || base == "." || base == "" {
			return append([]string{current}, tail...)
		}
		tail = append([]string{base}, tail...)
		current = parent
	}
	return append([]string{current}, tail...)
}
