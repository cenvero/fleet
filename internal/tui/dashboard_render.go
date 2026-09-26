// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// The dashboard renders every frame into a fixed width×height grid with a tiny
// purpose-built line builder instead of composing nested lipgloss styles: each
// style is reduced ONCE per colour profile to its SGR prefix, and each visible
// cell is written exactly once. That keeps a full frame at a few hundred
// microseconds regardless of fleet size (only visible rows are rendered) and
// makes layout exact, so the header can never scroll off screen.

// dstyle identifies one entry of the dashboard palette.
type dstyle uint8

const (
	sNone dstyle = iota
	sBold
	sMuted
	sDim
	sAccent
	sAccentBold
	sOK
	sWarn
	sCrit
	sInfo
	sBorder
	sBorderFocus
	sTitle
	sTitleFocus
	sSel
	sSelBlur
	sHdr
	sHdrBrand
	sHdrMuted
	sHdrOK
	sHdrWarn
	sHdrCrit
	sTab
	sTabActive
	sTabCount
	sTabCrit
	sKey
	sColHead
	sColHeadSort
	sPrompt
	sPromptKey
	sTrack
	sOverlayBorder
	numDStyles
)

// styleSpec describes a palette entry. fg/bg hold {dark, light} hex colours; ""
// means the terminal default. The mono* attributes are what the style degrades
// to under NO_COLOR / the ASCII profile, so selection and emphasis stay visible
// without any colour at all.
type dashStyleSpec struct {
	fg, bg        [2]string
	bold          bool
	faint         bool
	monoBold      bool
	monoReverse   bool
	monoUnderline bool
}

var dashStyleSpecs = [numDStyles]dashStyleSpec{
	sNone:          {},
	sBold:          {bold: true, monoBold: true},
	sMuted:         {fg: [2]string{"#8fa7b3", "#52656f"}},
	sDim:           {fg: [2]string{"#5f7480", "#8a9aa5"}},
	sAccent:        {fg: [2]string{"#00d4aa", "#00806a"}},
	sAccentBold:    {fg: [2]string{"#00d4aa", "#00806a"}, bold: true, monoBold: true},
	sOK:            {fg: [2]string{"#00d4aa", "#00806a"}},
	sWarn:          {fg: [2]string{"#ffd166", "#9a6400"}, bold: true},
	sCrit:          {fg: [2]string{"#ff6b6b", "#c62828"}, bold: true, monoBold: true},
	sInfo:          {fg: [2]string{"#74c0fc", "#1c6fb5"}},
	sBorder:        {fg: [2]string{"#2b3d49", "#b4c2ca"}},
	sBorderFocus:   {fg: [2]string{"#00d4aa", "#00806a"}},
	sTitle:         {fg: [2]string{"#c9d6dd", "#2d3b43"}, bold: true, monoBold: true},
	sTitleFocus:    {fg: [2]string{"#00d4aa", "#00806a"}, bold: true, monoBold: true},
	sSel:           {fg: [2]string{"#04231d", "#ffffff"}, bg: [2]string{"#00d4aa", "#00806a"}, bold: true, monoReverse: true},
	sSelBlur:       {bg: [2]string{"#1d2c36", "#dde6eb"}, monoUnderline: true},
	sHdr:           {fg: [2]string{"#e7ecef", "#1d2a31"}, bg: [2]string{"#101a23", "#e4ecf0"}},
	sHdrBrand:      {fg: [2]string{"#04231d", "#ffffff"}, bg: [2]string{"#00d4aa", "#00806a"}, bold: true, monoReverse: true, monoBold: true},
	sHdrMuted:      {fg: [2]string{"#8fa7b3", "#52656f"}, bg: [2]string{"#101a23", "#e4ecf0"}},
	sHdrOK:         {fg: [2]string{"#00d4aa", "#00806a"}, bg: [2]string{"#101a23", "#e4ecf0"}, bold: true},
	sHdrWarn:       {fg: [2]string{"#ffd166", "#9a6400"}, bg: [2]string{"#101a23", "#e4ecf0"}, bold: true, monoBold: true},
	sHdrCrit:       {fg: [2]string{"#ff6b6b", "#c62828"}, bg: [2]string{"#101a23", "#e4ecf0"}, bold: true, monoBold: true},
	sTab:           {fg: [2]string{"#8fa7b3", "#52656f"}},
	sTabActive:     {fg: [2]string{"#04231d", "#ffffff"}, bg: [2]string{"#00d4aa", "#00806a"}, bold: true, monoReverse: true, monoBold: true},
	sTabCount:      {fg: [2]string{"#5f7480", "#8a9aa5"}},
	sTabCrit:       {fg: [2]string{"#ff6b6b", "#c62828"}, bold: true, monoBold: true},
	sKey:           {fg: [2]string{"#36f0c0", "#00806a"}, bold: true, monoBold: true},
	sColHead:       {fg: [2]string{"#6f8793", "#6a7c86"}, bold: true, monoUnderline: true},
	sColHeadSort:   {fg: [2]string{"#00d4aa", "#00806a"}, bold: true, monoBold: true, monoUnderline: true},
	sPrompt:        {fg: [2]string{"#241a00", "#ffffff"}, bg: [2]string{"#ffd166", "#9a6400"}, bold: true, monoReverse: true, monoBold: true},
	sPromptKey:     {fg: [2]string{"#241a00", "#ffffff"}, bg: [2]string{"#ffe7a8", "#b87a00"}, bold: true, monoReverse: true, monoBold: true},
	sTrack:         {fg: [2]string{"#24343f", "#cfd9df"}},
	sOverlayBorder: {fg: [2]string{"#00d4aa", "#00806a"}, bold: true},
}

// dashPalette is the SGR prefix of every style for one colour profile. A style
// with an empty prefix is written without any escape codes.
type dashPalette struct {
	on   [numDStyles]string
	mono bool
}

const dashSGRReset = "\x1b[0m"

type dashPaletteKey struct {
	profile termenv.Profile
	dark    bool
}

var dashPaletteCache struct {
	sync.Mutex
	m map[dashPaletteKey]*dashPalette
}

// paletteFor returns the (cached) palette for the current lipgloss colour
// profile. NO_COLOR selects termenv.Ascii, which yields the attribute-only mono
// palette.
func dashPaletteFor(dark bool) *dashPalette {
	key := dashPaletteKey{profile: lipgloss.ColorProfile(), dark: dark}
	dashPaletteCache.Lock()
	defer dashPaletteCache.Unlock()
	if p, ok := dashPaletteCache.m[key]; ok {
		return p
	}
	if dashPaletteCache.m == nil {
		dashPaletteCache.m = map[dashPaletteKey]*dashPalette{}
	}
	p := dashBuildPalette(key.profile, dark)
	dashPaletteCache.m[key] = p
	return p
}

func dashBuildPalette(profile termenv.Profile, dark bool) *dashPalette {
	p := &dashPalette{mono: profile == termenv.Ascii}
	shade := 0
	if !dark {
		shade = 1
	}
	for i, spec := range dashStyleSpecs {
		var parts []string
		if p.mono {
			if spec.monoBold {
				parts = append(parts, termenv.BoldSeq)
			}
			if spec.monoUnderline {
				parts = append(parts, termenv.UnderlineSeq)
			}
			if spec.monoReverse {
				parts = append(parts, termenv.ReverseSeq)
			}
		} else {
			if spec.bold {
				parts = append(parts, termenv.BoldSeq)
			}
			if spec.faint {
				parts = append(parts, termenv.FaintSeq)
			}
			if c := spec.fg[shade]; c != "" {
				if col := profile.Color(c); col != nil {
					parts = append(parts, col.Sequence(false))
				}
			}
			if c := spec.bg[shade]; c != "" {
				if col := profile.Color(c); col != nil {
					parts = append(parts, col.Sequence(true))
				}
			}
		}
		if len(parts) > 0 {
			p.on[i] = "\x1b[" + strings.Join(parts, ";") + "m"
		}
	}
	return p
}

// ---------------------------------------------------------------------------
// Text measurement and sanitising
// ---------------------------------------------------------------------------

// cleanText makes untrusted text (alert messages, log lines, service
// descriptions, audit details — much of it written by remote agents) safe to
// place on the operator's terminal: control characters, including ESC and the
// C1 range, would otherwise let a remote log line inject terminal escape
// sequences. Tabs and newlines become spaces. Clean input is returned as-is.
func dashClean(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			clean = false
			break
		}
		if c >= 0x80 {
			// Check the non-ASCII tail rune by rune for C1 controls / bad UTF-8.
			for _, r := range s[i:] {
				if r == utf8.RuneError || r < 0x20 || (r >= 0x7f && r < 0xa0) {
					clean = false
					break
				}
			}
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteByte(' ')
		case r < 0x20 || (r >= 0x7f && r < 0xa0):
			b.WriteRune('�')
		case r == utf8.RuneError:
			b.WriteRune('�')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// textWidth is the number of terminal cells s occupies. s must already be
// clean (no escape sequences).
func dashWidth(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return lipgloss.Width(s)
		}
	}
	return len(s)
}

func dashRuneCells(r rune) int {
	if r < 0x80 {
		return 1
	}
	return lipgloss.Width(string(r))
}

// cutText returns the longest prefix of s that fits in w cells, and its width.
func dashCut(s string, w int) (string, int) {
	if w <= 0 {
		return "", 0
	}
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		if len(s) <= w {
			return s, len(s)
		}
		return s[:w], w
	}
	used := 0
	for i, r := range s {
		rw := dashRuneCells(r)
		if used+rw > w {
			return s[:i], used
		}
		used += rw
	}
	return s, used
}

// ---------------------------------------------------------------------------
// Repeated glyph strings (cached to avoid per-frame strings.Repeat)
// ---------------------------------------------------------------------------

type dashGlyphRun struct {
	mu    sync.Mutex
	glyph string
	cache []string
}

func (g *dashGlyphRun) n(count int) string {
	if count <= 0 {
		return ""
	}
	if count > 1024 {
		return strings.Repeat(g.glyph, count)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if count >= len(g.cache) {
		grown := make([]string, count+1)
		copy(grown, g.cache)
		g.cache = grown
	}
	if g.cache[count] == "" {
		g.cache[count] = strings.Repeat(g.glyph, count)
	}
	return g.cache[count]
}

var (
	dashRunSpace = &dashGlyphRun{glyph: " "}
	dashRunHLine = &dashGlyphRun{glyph: "─"}
	dashRunFull  = &dashGlyphRun{glyph: "█"}
	dashRunTrack = &dashGlyphRun{glyph: "░"}
	dashRunMid   = &dashGlyphRun{glyph: "▒"}
)

// eighths are the left-aligned partial blocks for smooth horizontal bars.
var dashEighths = [...]string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉"}

// sparkRunes are the vertical levels of a sparkline.
var dashSparkRunes = [...]string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}

// ---------------------------------------------------------------------------
// Line builder
// ---------------------------------------------------------------------------

// dline accumulates one styled line of an exact cell width.
type dline struct {
	b   []byte
	w   int // cells written
	max int // cell budget
	p   *dashPalette
}

func newDLine(p *dashPalette, width int) *dline {
	return &dline{b: make([]byte, 0, width+32), max: width, p: p}
}

func (l *dline) reset(width int) {
	l.b = l.b[:0]
	l.w = 0
	l.max = width
}

func (l *dline) room() int { return l.max - l.w }

// put writes clean text in style s, truncated to the remaining budget.
func (l *dline) put(s dstyle, text string) {
	if text == "" || l.w >= l.max {
		return
	}
	cut, cw := dashCut(text, l.max-l.w)
	if cw == 0 {
		return
	}
	l.putW(s, cut, cw)
}

// putW writes text of known width w (glyph runs, pre-cut text).
func (l *dline) putW(s dstyle, text string, w int) {
	if text == "" || w <= 0 {
		return
	}
	if l.w+w > l.max {
		text, w = dashCut(text, l.max-l.w)
		if w == 0 {
			return
		}
	}
	on := l.p.on[s]
	if on != "" {
		l.b = append(l.b, on...)
	}
	l.b = append(l.b, text...)
	if on != "" {
		l.b = append(l.b, dashSGRReset...)
	}
	l.w += w
}

// text writes untrusted text: it is sanitised, then truncated with an ellipsis
// when it does not fit in the given width, and padded to exactly that width.
func (l *dline) text(s dstyle, raw string, width int) {
	if width > l.room() {
		width = l.room()
	}
	if width <= 0 {
		return
	}
	txt := dashClean(raw)
	tw := dashWidth(txt)
	if tw > width {
		cut, cw := dashCut(txt, width-1)
		l.putW(s, cut, cw)
		l.putW(s, "…", 1)
		l.pad(s, width-cw-1)
		return
	}
	l.putW(s, txt, tw)
	l.pad(s, width-tw)
}

// textR is text right-aligned within width.
func (l *dline) textR(s dstyle, raw string, width int) {
	if width > l.room() {
		width = l.room()
	}
	txt := dashClean(raw)
	tw := dashWidth(txt)
	if tw >= width {
		l.text(s, txt, width)
		return
	}
	l.pad(s, width-tw)
	l.putW(s, txt, tw)
}

// pad writes n spaces in style s (sNone for plain spaces).
func (l *dline) pad(s dstyle, n int) {
	if n > l.room() {
		n = l.room()
	}
	if n <= 0 {
		return
	}
	l.putW(s, dashRunSpace.n(n), n)
}

// putFit writes the first variant that fits the remaining room; the last
// variant is written (truncated) when none fits.
func (l *dline) putFit(variants ...[]dseg) {
	for i, v := range variants {
		if dsegsWidth(v) <= l.room() || i == len(variants)-1 {
			for _, sg := range v {
				l.put(sg.s, sg.t)
			}
			return
		}
	}
}

// fill pads the rest of the line.
func (l *dline) fill(s dstyle) { l.pad(s, l.room()) }

func (l *dline) String() string {
	l.fill(sNone)
	return string(l.b)
}

// ---------------------------------------------------------------------------
// Widgets
// ---------------------------------------------------------------------------

// bar draws a horizontal gauge of width cells for pct (0..100).
func (l *dline) bar(pct float64, width int, s dstyle) {
	if width <= 0 {
		return
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	total := int(pct/100*float64(width*8) + 0.5)
	full := total / 8
	part := total % 8
	if full > width {
		full, part = width, 0
	}
	l.putW(s, dashRunFull.n(full), full)
	used := full
	if part > 0 && used < width {
		l.putW(s, dashEighths[part], 1)
		used++
	}
	l.putW(sTrack, dashRunTrack.n(width-used), width-used)
}

// spark draws the last width values (0..100) as a sparkline. Missing history
// on the left is padded with blanks.
func (l *dline) spark(values []float64, width int, s dstyle) {
	if width <= 0 {
		return
	}
	if len(values) > width {
		values = values[len(values)-width:]
	}
	l.pad(sNone, width-len(values))
	if len(values) == 0 {
		return
	}
	var buf [256 * 3]byte
	out := buf[:0]
	n := 0
	for _, v := range values {
		idx := int(v / 100 * float64(len(dashSparkRunes)))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(dashSparkRunes) {
			idx = len(dashSparkRunes) - 1
		}
		if len(out)+3 > cap(out) {
			l.putW(s, string(out), n)
			out, n = out[:0], 0
		}
		out = append(out, dashSparkRunes[idx]...)
		n++
	}
	l.putW(s, string(out), n)
}

// pctStyle maps a utilisation percentage to ok/warn/crit using the same
// thresholds the controller raises metric alerts at.
func dashPctStyle(pct, warn, crit float64) dstyle {
	switch {
	case pct >= crit:
		return sCrit
	case pct >= warn:
		return sWarn
	default:
		return sOK
	}
}

// Metric alert thresholds (mirrors core.evaluateMetricAlerts).
const (
	dashCPUWarn, dashCPUCrit   = 80, 90
	dashMemWarn, dashMemCrit   = 85, 95
	dashDiskWarn, dashDiskCrit = 85, 95
)

func dashPct(v float64) string {
	if v >= 99.5 {
		return "100%"
	}
	if v < 10 {
		return strconv.FormatFloat(v, 'f', 1, 64) + "%"
	}
	return strconv.Itoa(int(v+0.5)) + "%"
}

// ---------------------------------------------------------------------------
// Boxes
// ---------------------------------------------------------------------------

// dbox is a rounded panel of exactly w×h cells with a title (and optional
// right-aligned meta) set into its top border.
type dbox struct {
	p       *dashPalette
	w, h    int
	title   string
	meta    string
	metaSty dstyle
	focus   bool
	lines   []string // content lines, each exactly w-4 cells (see body())
}

func (b *dbox) inner() (int, int) { return max(b.w-4, 0), max(b.h-2, 0) }

// render returns the box as h lines of w cells.
func (b *dbox) render() []string {
	out := make([]string, 0, b.h)
	if b.h <= 0 || b.w < 4 {
		return out
	}
	border := sBorder
	titleSty := sTitle
	if b.focus {
		border = sBorderFocus
		titleSty = sTitleFocus
	}
	iw, ih := b.inner()
	l := newDLine(b.p, b.w)
	// Top border: ╭─ Title ──── meta ─╮
	l.putW(border, "╭─", 2)
	title := dashClean(b.title)
	tw := dashWidth(title)
	maxTitle := b.w - 6
	if tw > maxTitle {
		title, tw = dashCut(title, maxTitle)
	}
	if tw > 0 {
		l.putW(sNone, " ", 1)
		l.putW(titleSty, title, tw)
		l.putW(sNone, " ", 1)
	}
	meta := dashClean(b.meta)
	mw := dashWidth(meta)
	remain := b.w - l.w - 1 // leave room for ╮
	if mw > 0 && mw+4 <= remain {
		l.putW(border, dashRunHLine.n(remain-mw-3), remain-mw-3)
		l.putW(sNone, " ", 1)
		ms := b.metaSty
		if ms == sNone {
			ms = sMuted
		}
		l.putW(ms, meta, mw)
		l.putW(sNone, " ", 1)
		l.putW(border, "─", 1)
	} else {
		l.putW(border, dashRunHLine.n(remain), remain)
	}
	l.putW(border, "╮", 1)
	out = append(out, string(l.b))

	blank := dashRunSpace.n(iw)
	left := b.p.on[border] + "│" + dashResetIf(b.p.on[border]) + " "
	right := " " + b.p.on[border] + "│" + dashResetIf(b.p.on[border])
	for i := 0; i < ih; i++ {
		content := blank
		if i < len(b.lines) {
			content = b.lines[i]
		}
		out = append(out, left+content+right)
	}
	l.reset(b.w)
	l.putW(border, "╰", 1)
	l.putW(border, dashRunHLine.n(b.w-2), b.w-2)
	l.putW(border, "╯", 1)
	out = append(out, string(l.b))
	return out
}

func dashResetIf(on string) string {
	if on == "" {
		return ""
	}
	return dashSGRReset
}

// joinCols concatenates equally tall column blocks line by line.
func dashJoinCols(blocks ...[]string) []string {
	if len(blocks) == 0 {
		return nil
	}
	h := len(blocks[0])
	out := make([]string, h)
	for i := 0; i < h; i++ {
		var sb strings.Builder
		for _, blk := range blocks {
			if i < len(blk) {
				sb.WriteString(blk[i])
			}
		}
		out[i] = sb.String()
	}
	return out
}

// blankLines returns n lines of w spaces.
func dashBlankLines(w, n int) []string {
	out := make([]string, n)
	s := dashRunSpace.n(w)
	for i := range out {
		out[i] = s
	}
	return out
}
