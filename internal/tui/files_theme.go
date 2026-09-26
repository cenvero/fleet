// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// ============================================================================
// Paints: precomputed ANSI prefix/suffix pairs
//
// lipgloss.Style.Render is convenient but costs a few microseconds and several
// allocations per call (it splits lines, measures, pads and re-styles). A file
// listing frame needs hundreds of tiny styled segments, so the hot paths use a
// "fmPaint": the escape sequence a style would emit before and after its text,
// captured once per colour profile. Painting a segment is then two string
// concatenations. The palette is rebuilt automatically if the colour profile
// changes (tests force TrueColor; NO_COLOR yields the Ascii profile).
// ============================================================================

type fmPaint struct{ pre, suf string }

// s paints text. Empty text stays empty so callers can concatenate freely.
func (p fmPaint) s(text string) string {
	if text == "" {
		return ""
	}
	return p.pre + text + p.suf
}

const fmPaintMarker = ""

func fmPaintOf(st lipgloss.Style) fmPaint {
	r := st.Render(fmPaintMarker)
	i := strings.Index(r, fmPaintMarker)
	if i < 0 {
		return fmPaint{}
	}
	return fmPaint{pre: r[:i], suf: r[i+len(fmPaintMarker):]}
}

// Page and chrome colours (the item/icon palette lives in files_view.go).
var (
	fmPageBg    = fmColor("#0a0e14")
	fmBarBg     = fmColor("#0e1620")
	fmToolBg    = fmColor("#0b121a")
	fmPaneRuleC = fmColor("#23384a")
	fmHoverBg   = fmColor("#15212c")
	fmCursorBg  = fmColor("#1b2c38")
	fmMarkBg    = fmColor("#10303a")
	fmDropBg    = fmColor("#0f3b32")
	fmOkC       = fmColor("#7ee787")
)

// fmColor builds a colour with explicit 256- and 16-colour fallbacks.
//
// termenv (v0.16) degrades hex colours to the xterm-256 palette with a bug in
// its grey-ramp branch (it averages the 0–5 cube indices instead of the 0–255
// channel values), which maps light greys such as the file manager's text
// colour #e7ecef to 232 — near-black — in every 256-colour terminal (tmux,
// screen, many SSH sessions). Computing the nearest xterm colour ourselves
// keeps the palette legible there.
func fmColor(hex string) lipgloss.CompleteColor {
	r, g, b, ok := parseHex(hex)
	if !ok {
		return lipgloss.CompleteColor{TrueColor: hex, ANSI256: "7", ANSI: "7"}
	}
	return lipgloss.CompleteColor{
		TrueColor: hex,
		ANSI256:   strconv.Itoa(nearestXterm256(r, g, b)),
		ANSI:      strconv.Itoa(nearestANSI16(r, g, b)),
	}
}

func parseHex(hex string) (r, g, b int, ok bool) {
	if len(hex) != 7 || hex[0] != '#' {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(hex[1:], 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff), true
}

func colorDist(r1, g1, b1, r2, g2, b2 int) int {
	// Weighted RGB distance ("redmean" approximation of perceptual distance).
	rm := (r1 + r2) / 2
	dr, dg, db := r1-r2, g1-g2, b1-b2
	return (((512 + rm) * dr * dr) >> 8) + 4*dg*dg + (((767 - rm) * db * db) >> 8)
}

// nearestXterm256 picks the closest colour among the 6×6×6 cube (16–231) and
// the grey ramp (232–255).
func nearestXterm256(r, g, b int) int {
	levels := [6]int{0, 0x5f, 0x87, 0xaf, 0xd7, 0xff}
	best, bestD := 16, -1
	for ri, rv := range levels {
		for gi, gv := range levels {
			for bi, bv := range levels {
				if d := colorDist(r, g, b, rv, gv, bv); bestD < 0 || d < bestD {
					best, bestD = 16+36*ri+6*gi+bi, d
				}
			}
		}
	}
	for i := range 24 {
		v := 8 + 10*i
		if d := colorDist(r, g, b, v, v, v); d < bestD {
			best, bestD = 232+i, d
		}
	}
	return best
}

// nearestANSI16 picks the closest of the 16 basic terminal colours (using the
// common xterm defaults).
func nearestANSI16(r, g, b int) int {
	basic := [16][3]int{
		{0, 0, 0}, {205, 0, 0}, {0, 205, 0}, {205, 205, 0},
		{0, 0, 238}, {205, 0, 205}, {0, 205, 205}, {229, 229, 229},
		{127, 127, 127}, {255, 0, 0}, {0, 255, 0}, {255, 255, 0},
		{92, 92, 255}, {255, 0, 255}, {0, 255, 255}, {255, 255, 255},
	}
	best, bestD := 0, -1
	for i, c := range basic {
		if d := colorDist(r, g, b, c[0], c[1], c[2]); bestD < 0 || d < bestD {
			best, bestD = i, d
		}
	}
	return best
}

// fmPalette holds every fmPaint the base frame uses.
type fmPalette struct {
	profile termenv.Profile
	noColor bool

	// page background variants
	base, muted, dim, rule, paneRule, accent, accentB, accent2, dir, danger, warn, ok fmPaint
	// zebra (alternate row) variants
	zBase, zMuted, zDim, zDir fmPaint
	// full-row states
	sel, cursor, hover, mark, drop fmPaint
	// header / toolbar / status bars
	barBase, barBrand, barMuted, barDim, barAccent  fmPaint
	toolBase, toolKey, toolLabel, toolSep, toolHot  fmPaint
	statBase, statMuted, statDim, statChip, statKey fmPaint
	statInfo, statOk, statWarn, statErr             fmPaint
	keyChip, hintLabel                              fmPaint
	// transfer bar
	barFill, barTrack fmPaint
	// icons: plain + zebra
	icons  map[fmIconKind]fmPaint
	iconsZ map[fmIconKind]fmPaint
}

var fmPaletteCache atomic.Pointer[fmPalette]

// pal returns the palette for the current colour profile.
func fmPal() *fmPalette {
	prof := lipgloss.ColorProfile()
	if p := fmPaletteCache.Load(); p != nil && p.profile == prof {
		return p
	}
	p := fmBuildPalette(prof)
	fmPaletteCache.Store(p)
	return p
}

func fmBuildPalette(prof termenv.Profile) *fmPalette {
	p := &fmPalette{profile: prof, noColor: prof == termenv.Ascii}
	on := func(bg lipgloss.TerminalColor) lipgloss.Style { return lipgloss.NewStyle().Background(bg) }
	pg := func() lipgloss.Style { return on(fmPageBg) }
	zb := func() lipgloss.Style { return on(fmZebraC) }

	p.base = fmPaintOf(pg().Foreground(fmText))
	p.muted = fmPaintOf(pg().Foreground(fmMutedC))
	p.dim = fmPaintOf(pg().Foreground(fmDimC))
	p.rule = fmPaintOf(pg().Foreground(fmBorderC))
	p.paneRule = fmPaintOf(pg().Foreground(fmPaneRuleC))
	p.accent = fmPaintOf(pg().Foreground(fmAccent))
	p.accentB = fmPaintOf(pg().Foreground(fmAccent).Bold(true))
	p.accent2 = fmPaintOf(pg().Foreground(fmAccent2))
	p.dir = fmPaintOf(pg().Foreground(fmDirC).Bold(true))
	p.danger = fmPaintOf(pg().Foreground(fmDangerC).Bold(true))
	p.warn = fmPaintOf(pg().Foreground(fmWarnC))
	p.ok = fmPaintOf(pg().Foreground(fmOkC))

	p.zBase = fmPaintOf(zb().Foreground(fmText))
	p.zMuted = fmPaintOf(zb().Foreground(fmMutedC))
	p.zDim = fmPaintOf(zb().Foreground(fmDimC))
	p.zDir = fmPaintOf(zb().Foreground(fmDirC).Bold(true))

	if p.noColor {
		// Without colour (NO_COLOR / dumb terminals) termenv drops every SGR
		// sequence, attributes included, so row state is drawn with raw
		// attribute codes: reverse video for the cursor, underline for the
		// cursor of the inactive pane, bold for marked rows. NO_COLOR forbids
		// colour, not emphasis.
		p.sel = fmPaint{pre: "\x1b[7;1m", suf: "\x1b[0m"}
		p.cursor = fmPaint{pre: "\x1b[4m", suf: "\x1b[0m"}
		p.hover = fmPaint{}
		p.mark = fmPaint{pre: "\x1b[1m", suf: "\x1b[0m"}
		p.drop = fmPaint{pre: "\x1b[7m", suf: "\x1b[0m"}
	} else {
		p.sel = fmPaintOf(on(fmAccent).Foreground(fmInk).Bold(true))
		p.cursor = fmPaintOf(on(fmCursorBg).Foreground(fmText))
		p.hover = fmPaintOf(on(fmHoverBg).Foreground(fmText))
		p.mark = fmPaintOf(on(fmMarkBg).Foreground(fmAccent2))
		p.drop = fmPaintOf(on(fmDropBg).Foreground(fmAccent2).Bold(true))
	}

	bar := func() lipgloss.Style { return on(fmBarBg) }
	p.barBase = fmPaintOf(bar().Foreground(fmText))
	p.barBrand = fmPaintOf(bar().Foreground(fmAccent).Bold(true))
	p.barMuted = fmPaintOf(bar().Foreground(fmMutedC))
	p.barDim = fmPaintOf(bar().Foreground(fmDimC))
	p.barAccent = fmPaintOf(bar().Foreground(fmAccent2))

	tool := func() lipgloss.Style { return on(fmToolBg) }
	p.toolBase = fmPaintOf(tool().Foreground(fmMutedC))
	p.toolKey = fmPaintOf(tool().Foreground(fmAccent2).Bold(true))
	p.toolLabel = fmPaintOf(tool().Foreground(fmMutedC))
	p.toolSep = fmPaintOf(tool().Foreground(fmBorderC))
	if p.noColor {
		p.toolHot = fmPaint{pre: "\x1b[7m", suf: "\x1b[0m"}
	} else {
		p.toolHot = fmPaintOf(on(fmHoverBg).Foreground(fmText))
	}

	p.statBase = fmPaintOf(bar().Foreground(fmText))
	p.statMuted = fmPaintOf(bar().Foreground(fmMutedC))
	p.statDim = fmPaintOf(bar().Foreground(fmDimC))
	p.statChip = fmPaintOf(on(fmBorderC).Foreground(fmText))
	p.statKey = fmPaintOf(bar().Foreground(fmAccent2).Bold(true))
	p.statInfo = fmPaintOf(bar().Foreground(fmMutedC))
	p.statOk = fmPaintOf(bar().Foreground(fmOkC).Bold(true))
	p.statWarn = fmPaintOf(bar().Foreground(fmWarnC).Bold(true))
	p.statErr = fmPaintOf(bar().Foreground(fmDangerC).Bold(true))

	p.keyChip = fmPaintOf(on(fmBorderC).Foreground(fmAccent2).Bold(true))
	p.hintLabel = fmPaintOf(pg().Foreground(fmMutedC))

	p.barFill = fmPaintOf(pg().Foreground(fmAccent))
	p.barTrack = fmPaintOf(pg().Foreground(fmBorderC))

	p.icons = map[fmIconKind]fmPaint{}
	p.iconsZ = map[fmIconKind]fmPaint{}
	for k, st := range fmIconStyles() {
		p.icons[k] = fmPaintOf(st.Background(fmPageBg))
		p.iconsZ[k] = fmPaintOf(st.Background(fmZebraC))
	}
	return p
}

// pageLine pads a painted line (whose visible width is known) to w columns
// with page background so the whole screen stays one colour.
func (p *fmPalette) pad(n int) string {
	if n <= 0 {
		return ""
	}
	return p.base.s(strings.Repeat(" ", n))
}
