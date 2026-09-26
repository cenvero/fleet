// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// ============================================================================
// Screen geometry
//
// Every region of the file manager is placed by fmLayout, a pure function of
// the model. The renderer draws exactly these rectangles and the mouse
// handlers hit-test against the same numbers, so clicks resolve in O(1)
// without scanning bubblezone markers for every visible row on every motion
// event (the old approach cost ~40 zone lookups per mouse move).
//
//	row 0            header bar
//	row 1            toolbar (never wraps; overflow goes to "≡ More")
//	row panesY..     pane boxes: top border, breadcrumb, column header,
//	                 `rows` item lines, bottom border
//	row xferY..      transfer queue (0, 1 or several lines)
//	row statusY      status bar
//	row footerY      key hints (only when the terminal is tall enough)
// ============================================================================

type fmLayout struct {
	w, h   int
	padX   int // left/right page margin
	innerW int

	headerY, toolbarY int

	panesY int // top border row of the pane boxes
	boxH   int // pane box height including borders
	rows   int // item lines in a pane body
	bodyY  int // first item line

	paneX     [2]int
	paneW     [2]int // full box width including borders
	paneShown [2]bool
	single    bool // narrow terminal: only the focused pane is drawn

	previewShown bool
	previewX     int
	previewW     int

	xferY, xferH int
	statusY      int
	footerY      int
	footer       bool
	tooSmall     bool
}

// fmPaneChromeRows is the number of non-item lines in a pane box: top border,
// breadcrumb, column header and bottom border.
const fmPaneChromeRows = 4

func (m filesModel) layout() fmLayout {
	l := fmLayout{w: m.width, h: m.height}
	if l.w < 30 || l.h < 10 {
		l.tooSmall = true
	}
	if l.w >= 70 {
		l.padX = 1
	}
	l.innerW = l.w - 2*l.padX
	if l.innerW < 20 {
		l.innerW = 20
	}
	l.headerY, l.toolbarY = 0, 1
	l.panesY = 2

	l.footer = l.h >= 28
	footerH := 0
	if l.footer {
		footerH = 1
	}
	l.xferH = m.transferPanelHeight()
	fixed := 2 + fmPaneChromeRows + 1 + footerH // header+toolbar, pane chrome, status
	l.rows = l.h - fixed - l.xferH
	if l.rows < 3 && l.xferH > 1 {
		l.xferH = 1 // collapse the queue to its summary line before starving panes
		l.rows = l.h - fixed - l.xferH
	}
	if l.rows < 2 && l.footer {
		l.footer, footerH = false, 0
		l.rows = l.h - (2 + fmPaneChromeRows + 1) - l.xferH
	}
	if l.rows < 1 {
		l.rows = 1
	}
	l.boxH = l.rows + fmPaneChromeRows
	l.bodyY = l.panesY + 3
	l.xferY = l.panesY + l.boxH
	l.statusY = l.xferY + l.xferH
	l.footerY = l.statusY + 1

	// Horizontal split.
	x0 := l.padX
	const gap = 1
	switch {
	case l.innerW < 56:
		// One pane at a time; Tab flips which one is shown.
		l.single = true
		side := m.focus
		if side != 0 && side != 1 {
			side = 0
		}
		l.paneShown[side] = true
		l.paneX[side] = x0
		l.paneW[side] = l.innerW
		l.paneX[1-side] = x0
		l.paneW[1-side] = l.innerW
	case m.preview.on && l.innerW >= 130:
		// Three columns: left | right | preview.
		pw := l.innerW * 30 / 100
		if pw > 72 {
			pw = 72
		}
		rest := l.innerW - pw - 2*gap
		lw := rest / 2
		rw := rest - lw
		l.paneShown = [2]bool{true, true}
		l.paneX[0], l.paneW[0] = x0, lw
		l.paneX[1], l.paneW[1] = x0+lw+gap, rw
		l.previewShown = true
		l.previewX, l.previewW = x0+lw+gap+rw+gap, pw
	case m.preview.on:
		// Midnight Commander style quick view: the preview takes the place of
		// the pane that does not have focus.
		lw := (l.innerW - gap) / 2
		rw := l.innerW - gap - lw
		f := m.focus
		if f != 1 {
			f = 0
		}
		l.paneShown[f] = true
		if f == 0 {
			l.paneX[0], l.paneW[0] = x0, lw
			l.previewX, l.previewW = x0+lw+gap, rw
			l.paneX[1], l.paneW[1] = l.previewX, rw
		} else {
			l.previewX, l.previewW = x0, lw
			l.paneX[1], l.paneW[1] = x0+lw+gap, rw
			l.paneX[0], l.paneW[0] = x0, lw
		}
		l.previewShown = true
	default:
		lw := (l.innerW - gap) / 2
		rw := l.innerW - gap - lw
		l.paneShown = [2]bool{true, true}
		l.paneX[0], l.paneW[0] = x0, lw
		l.paneX[1], l.paneW[1] = x0+lw+gap, rw
	}
	return l
}

// contentW is the width inside a pane's border.
func (l fmLayout) contentW(side int) int {
	w := l.paneW[side] - 2
	if w < 1 {
		w = 1
	}
	return w
}

// ============================================================================
// Hit testing
// ============================================================================

// fmHitKind names the region a mouse event landed on.
type fmHitKind int

const (
	fmHitNone fmHitKind = iota
	fmHitToolbar
	fmHitTitle
	fmHitCrumb
	fmHitColumn
	fmHitRow  // a row (index may be -1 for blank space inside the pane body)
	fmHitPane // pane chrome that is not otherwise interactive
	fmHitTransfer
	fmHitStatus
)

type fmHit struct {
	kind   fmHitKind
	side   int
	index  int    // row index (hitRow), crumb segment (fmHitCrumb), transfer row (fmHitTransfer)
	action string // toolbar action or column sort key name
}

func (m filesModel) hitTest(x, y int) fmHit {
	l := m.layout()
	if l.tooSmall {
		return fmHit{kind: fmHitNone, side: -1, index: -1}
	}
	if y == l.toolbarY {
		for _, sp := range m.toolbarLayout(l.innerW).spans {
			if x >= l.padX+sp.x0 && x < l.padX+sp.x1 {
				return fmHit{kind: fmHitToolbar, side: -1, index: -1, action: sp.action}
			}
		}
		return fmHit{kind: fmHitNone, side: -1, index: -1}
	}
	if y >= l.panesY && y < l.panesY+l.boxH {
		for side := range 2 {
			if !l.paneShown[side] {
				continue
			}
			x0, w := l.paneX[side], l.paneW[side]
			if x < x0 || x >= x0+w {
				continue
			}
			cx := x - x0 - 1 // content-relative column
			switch {
			case y == l.panesY:
				return fmHit{kind: fmHitTitle, side: side, index: -1}
			case y == l.panesY+1:
				if seg := m.crumbHit(side, l.contentW(side), cx); seg >= 0 {
					return fmHit{kind: fmHitCrumb, side: side, index: seg}
				}
				return fmHit{kind: fmHitTitle, side: side, index: -1}
			case y == l.panesY+2:
				return fmHit{kind: fmHitColumn, side: side, index: -1,
					action: m.columnHit(side, l.contentW(side), cx)}
			case y >= l.bodyY && y < l.bodyY+l.rows:
				if cx < 0 || cx >= l.contentW(side) {
					return fmHit{kind: fmHitPane, side: side, index: -1}
				}
				return fmHit{kind: fmHitRow, side: side, index: m.rowAt(side, l, cx, y-l.bodyY)}
			default:
				return fmHit{kind: fmHitPane, side: side, index: -1}
			}
		}
		return fmHit{kind: fmHitNone, side: -1, index: -1}
	}
	if l.xferH > 0 && y >= l.xferY && y < l.xferY+l.xferH {
		return fmHit{kind: fmHitTransfer, side: -1, index: m.transferRowAt(y - l.xferY)}
	}
	if y == l.statusY {
		return fmHit{kind: fmHitStatus, side: -1, index: -1}
	}
	return fmHit{kind: fmHitNone, side: -1, index: -1}
}

// rowAt maps a body line (and content column, for grid view) to an entry
// index, or -1 for blank space below the last entry.
func (m filesModel) rowAt(side int, l fmLayout, cx, line int) int {
	pane := m.paneRefConst(side)
	if pane.view == viewGrid {
		cw := l.contentW(side)
		cols := gridCols(cw)
		cellW := cw / cols
		if cellW < 4 {
			cellW = 4
		}
		col := cx / cellW
		if col >= cols {
			return -1
		}
		idx := pane.scroll + (line/gridCellH)*cols + col
		lo, hi := m.visibleRange(side)
		if idx < lo || idx >= hi {
			return -1
		}
		return idx
	}
	idx := pane.scroll + line
	if idx < 0 || idx >= len(pane.entries) {
		return -1
	}
	return idx
}

// hitRow keeps the historical (side, rowIndex, inSomePane) contract used by
// the drag/drop and context-menu code: rowIndex is -1 inside a pane but not
// over an entry.
func (m filesModel) hitRow(msg tea.MouseMsg) (int, int, bool) {
	h := m.hitTest(msg.X, msg.Y)
	switch h.kind {
	case fmHitRow:
		return h.side, h.index, true
	case fmHitPane, fmHitTitle, fmHitCrumb, fmHitColumn:
		return h.side, -1, true
	}
	return -1, -1, false
}

// visibleRange returns the [lo, hi) entry indices the renderer currently draws
// for a pane, matching renderListBody/renderGridBody's own windowing.
func (m filesModel) visibleRange(side int) (int, int) {
	pane := m.paneRefConst(side)
	count := len(pane.entries)
	lo := pane.scroll
	if lo < 0 {
		lo = 0
	}
	if lo > count {
		lo = count
	}
	span := m.visibleRows()
	if pane.view == viewGrid {
		cols, visRows := m.gridDims()
		span = cols * visRows
	}
	hi := lo + span
	if hi > count {
		hi = count
	}
	return lo, hi
}
