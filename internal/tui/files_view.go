// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"
)

// ============================================================================
// Palette & styles (self-contained, Charm-grade polish)
// ============================================================================

var (
	fmAccent   = lipgloss.Color("#00d4aa")
	fmAccent2  = lipgloss.Color("#36f0c0")
	fmInk      = lipgloss.Color("#04231d")
	fmText     = lipgloss.Color("#e7ecef")
	fmMutedC   = lipgloss.Color("#8fa7b3")
	fmDimC     = lipgloss.Color("#5f7480")
	fmBorderC  = lipgloss.Color("#1c2b36")
	fmZebraC   = lipgloss.Color("#0c141d")
	fmDirC     = lipgloss.Color("#7ad7ff")
	fmDangerC  = lipgloss.Color("#ff6b6b")
	fmWarnC    = lipgloss.Color("#ffce6b")
	fmHeaderBg = lipgloss.Color("#0e1620")
	fmPanelBg  = lipgloss.Color("#0d131b")
	fmDropC    = lipgloss.Color("#36f0c0")

	fmHeaderBar = lipgloss.NewStyle().Background(fmHeaderBg)
	fmBrand     = lipgloss.NewStyle().Foreground(fmAccent).Bold(true)
	fmTag       = lipgloss.NewStyle().Foreground(fmDimC)
	fmServerTag = lipgloss.NewStyle().Foreground(fmMutedC)

	fmRule  = lipgloss.NewStyle().Foreground(fmBorderC)
	fmCount = lipgloss.NewStyle().Foreground(fmDimC)

	fmSelRow   = lipgloss.NewStyle().Background(fmAccent).Foreground(fmInk).Bold(true)
	fmHoverRow = lipgloss.NewStyle().Background(lipgloss.Color("#15212c")).Foreground(fmText)
	fmDropRow  = lipgloss.NewStyle().Background(lipgloss.Color("#0f3b32")).Foreground(fmAccent2).Bold(true)
	fmMarkRow  = lipgloss.NewStyle().Background(lipgloss.Color("#10303a")).Foreground(fmAccent2)
	fmDirRow   = lipgloss.NewStyle().Foreground(fmDirC).Bold(true)
	fmFileRow  = lipgloss.NewStyle().Foreground(fmText)
	fmSizeCol  = lipgloss.NewStyle().Foreground(fmDimC)

	fmKeyChip   = lipgloss.NewStyle().Background(fmBorderC).Foreground(fmAccent2).Bold(true).Padding(0, 1)
	fmHintLabel = lipgloss.NewStyle().Foreground(fmMutedC)
	fmStatusSty = lipgloss.NewStyle().Foreground(fmMutedC)
	fmDoneSty   = lipgloss.NewStyle().Foreground(fmAccent).Bold(true)
	fmErrSty    = lipgloss.NewStyle().Foreground(fmDangerC).Bold(true)

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

	fmToolBtn = lipgloss.NewStyle().Foreground(fmMutedC).Padding(0, 1)
)

// ============================================================================
// Top-level View
// ============================================================================

func (m filesModel) View() string {
	innerW := m.width - 4 // page padding (1,2)
	if innerW < 48 {
		innerW = 48
	}

	header := m.renderHeader(innerW)
	toolbar := m.renderToolbar(innerW)

	sepW := 3
	paneWidth := (innerW - sepW) / 2
	if paneWidth < 28 {
		paneWidth = 28
	}
	rows := m.visibleRows()

	left := m.renderPane(0, paneWidth, rows)
	right := m.renderPane(1, paneWidth, rows)
	sep := sepColumn(lipgloss.Height(left))
	panes := lipgloss.JoinHorizontal(lipgloss.Top, left, sep, right)

	status := m.status
	if status == "" {
		status = "ready"
	}
	statusLine := fmStatusSty.Render("  " + truncate(status, innerW-4))
	transfers := m.renderTransfers(innerW)
	footer := m.renderFooter()

	parts := []string{header, toolbar, "", panes, statusLine}
	if transfers != "" {
		parts = append(parts, transfers)
	}
	parts = append(parts, footer)
	body := lipgloss.JoinVertical(lipgloss.Left, parts...)

	rendered := pageStyle.Render(body)

	// Compose overlays on top of the base frame, then drag ghost on top of all.
	rendered = m.composeOverlay(rendered)
	rendered = m.composeGhost(rendered)

	// zone.Scan records every marked zone and strips the markers. Run once per
	// frame at the root.
	return zone.Scan(rendered)
}

// ============================================================================
// Header (brand + sources)
// ============================================================================

func (m filesModel) renderHeader(innerW int) string {
	brand := fmBrand.Render("◆ Cenvero Fleet") + fmTag.Render("  ·  files")
	right := fmServerTag.Render(m.left.label() + "  ⇄  " + m.right.label())
	gap := innerW - lipgloss.Width(brand) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	line := " " + brand + strings.Repeat(" ", gap) + right + " "
	return fmHeaderBar.Width(innerW).Render(line)
}

// ============================================================================
// Toolbar (thin action strip; every button has a key)
// ============================================================================

type toolButton struct {
	action string
	key    string
	label  string
}

func (m filesModel) renderToolbar(innerW int) string {
	btns := []toolButton{
		{"source", "s", "Source"},
		{"newfolder", "n", "New"},
		{"rename", "r", "Rename"},
		{"delete", "d", "Delete"},
		{"copy", "c", "Copy →"},
		{"move", "m", "Move →"},
		{"props", "i", "Info"},
		{"view", "v", viewLabel(m.paneRefConst(m.focus).view)},
		{"hidden", ".", hiddenLabel(m.showHidden)},
		{"refresh", "g", "Refresh"},
		{"quit", "q", "Quit"},
	}
	parts := make([]string, 0, len(btns))
	for _, b := range btns {
		txt := fmt.Sprintf("%s %s", b.key, b.label)
		styled := fmToolBtn.Render(txt)
		parts = append(parts, zone.Mark(fmActPrefix+b.action, styled))
	}
	bar := strings.Join(parts, fmRule.Render("│"))
	return lipgloss.NewStyle().Background(fmHeaderBg).Width(innerW).Render(" " + bar)
}

func hiddenLabel(on bool) string {
	if on {
		return "Hidden ✓"
	}
	return "Hidden"
}

// viewLabel names the toolbar/menu button for the focused pane's current layout
// (showing what `v` will offer next, Finder-style).
func viewLabel(v viewMode) string {
	if v == viewGrid {
		return "View: Icons"
	}
	return "View: List"
}

// ============================================================================
// Pane rendering (breadcrumb header + big rows)
// ============================================================================

func (m filesModel) renderPane(side, paneWidth, rows int) string {
	pane := m.paneRefConst(side)
	cw := paneWidth - 2 // content width inside the rounded border
	focused := side == m.focus

	// Whether this pane is the active drop target during a drag.
	isDropTarget := m.drag != nil && m.drag.active && m.hoverSide == side && side != m.drag.fromSide

	header := zone.Mark(headerZoneID(side), m.renderPaneHeader(side, cw, focused))

	var b strings.Builder
	b.WriteString(header + "\n")
	b.WriteString(fmRule.Render(strings.Repeat("─", cw)) + "\n")

	switch {
	case pane.loading:
		b.WriteString(fmStatusSty.Render("loading…"))
	case pane.err != nil:
		b.WriteString(fmErrSty.Render("error: " + truncate(pane.err.Error(), cw-7)))
	case len(pane.entries) == 0:
		b.WriteString(fmStatusSty.Render("(empty)"))
	case pane.view == viewGrid:
		b.WriteString(m.renderGridBody(side, cw, rows, isDropTarget))
	default:
		b.WriteString(m.renderListBody(side, cw, rows, isDropTarget))
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(fmBorderC).
		Width(cw).
		Height(rows + 2) // header + rule + rows
	switch {
	case isDropTarget:
		box = box.BorderForeground(fmDropC)
	case focused:
		box = box.BorderForeground(fmAccent)
	}
	return zone.Mark(paneZoneID(side), box.Render(b.String()))
}

// renderListBody draws the classic one-item-per-row list, padded to a stable
// height. Every row is zone.Mark'ed by its item index so hit-testing, hover, and
// drag-drop resolve clicks back to the right entry.
func (m filesModel) renderListBody(side, cw, rows int, isDropTarget bool) string {
	pane := m.paneRefConst(side)
	end := pane.scroll + rows
	if end > len(pane.entries) {
		end = len(pane.entries)
	}
	lines := make([]string, 0, rows)
	for i := pane.scroll; i < end; i++ {
		lines = append(lines, zone.Mark(rowZoneID(side, i), m.renderRow(side, i, cw, isDropTarget)))
	}
	// Pad the body so the box height is stable while scrolling.
	for len(lines) < rows {
		lines = append(lines, strings.Repeat(" ", cw))
	}
	return strings.Join(lines, "\n")
}

// renderGridBody draws a Finder-style icon grid: cells laid out in gridCols
// columns, each cell gridCellH lines tall (big icon over a centered name). Each
// cell is zone.Mark'ed with the SAME rowZoneID as its list row, so hitRow,
// hover, selection, and drag-drop work identically in both views.
func (m filesModel) renderGridBody(side, cw, rows int, isDropTarget bool) string {
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
		cells := make([]string, 0, cols)
		for c := range cols {
			idx := i + c
			if idx >= end {
				cells = append(cells, blankCell(cellW))
				continue
			}
			cell := m.renderGridCell(side, idx, cellW, isDropTarget)
			cells = append(cells, zone.Mark(rowZoneID(side, idx), cell))
		}
		// Each cell is gridCellH lines; join them side-by-side per cell-row.
		lines = append(lines, lipgloss.JoinHorizontal(lipgloss.Top, cells...))
	}
	body := strings.Join(lines, "\n")

	// Pad the body to a stable height (rows text lines).
	cur := 0
	if body != "" {
		cur = lipgloss.Height(body)
	}
	for cur < rows {
		body += "\n" + strings.Repeat(" ", cw)
		cur++
	}
	return body
}

// blankCell is an empty grid cell (gridCellH lines of spaces) used to pad short
// final rows so the grid keeps a rectangular shape.
func blankCell(cellW int) string {
	line := strings.Repeat(" ", cellW)
	parts := make([]string, gridCellH)
	for i := range parts {
		parts[i] = line
	}
	return strings.Join(parts, "\n")
}

// renderGridCell draws a single icon+name cell of width cellW and height
// gridCellH. Styling precedence matches renderRow: selected > drop-hover >
// marked > hover > default, color-coded by kind.
func (m filesModel) renderGridCell(side, i, cellW int, dropTargetPane bool) string {
	pane := m.paneRefConst(side)
	item := pane.entries[i]
	focused := side == m.focus
	selected := i == pane.index && focused
	marked := pane.selected[i]
	hovered := side == m.hoverSide && i == m.hoverIndex
	dropHover := dropTargetPane && i == m.hoverIndex && item.isDir

	icon := gridGlyphFor(item)
	name := item.name
	if item.isDir && item.name != ".." {
		name += "/"
	}
	if marked {
		name = "✓ " + name
	}

	// Color the icon by kind; selection/drop styling repaints the whole cell.
	iconFg := fmText
	switch {
	case item.name == "..":
		iconFg = fmMutedC
	case item.isDir:
		iconFg = fmDirC
	case item.symlink:
		iconFg = fmAccent2
	}

	nameLine := centerCell(truncate(name, cellW), cellW)
	iconLine := centerCell(icon, cellW)

	switch {
	case selected:
		return fmSelRow.Width(cellW).Render(iconLine) + "\n" + fmSelRow.Width(cellW).Render(nameLine)
	case dropHover:
		return fmDropRow.Width(cellW).Render(iconLine) + "\n" + fmDropRow.Width(cellW).Render(nameLine)
	case marked:
		return fmMarkRow.Width(cellW).Render(iconLine) + "\n" + fmMarkRow.Width(cellW).Render(nameLine)
	case hovered:
		return fmHoverRow.Width(cellW).Render(iconLine) + "\n" + fmHoverRow.Width(cellW).Render(nameLine)
	default:
		iconStyled := lipgloss.NewStyle().Foreground(iconFg).Bold(true).Width(cellW).Render(iconLine)
		nameFg := fmText
		if item.isDir {
			nameFg = fmDirC
		}
		nameStyled := lipgloss.NewStyle().Foreground(nameFg).Width(cellW).Render(nameLine)
		return iconStyled + "\n" + nameStyled
	}
}

// centerCell centers s within width w (display columns), padding with spaces.
func centerCell(s string, w int) string {
	sw := lipgloss.Width(s)
	if sw >= w {
		return s
	}
	left := (w - sw) / 2
	right := w - sw - left
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
}

// gridGlyphFor returns a larger, Finder-like glyph for the icon cell, color
// distinct by kind. It uses bold single-width geometric glyphs (kept single-width
// on purpose so cell alignment never tears) bucketed like glyphFor.
func gridGlyphFor(item fileItem) string {
	if item.name == ".." {
		return "⤴"
	}
	if item.isDir {
		return "▰"
	}
	if item.symlink {
		return "↪"
	}
	ext := strings.ToLower(filepath.Ext(item.name))
	switch ext {
	case ".go", ".rs", ".c", ".h", ".cpp", ".py", ".js", ".ts", ".java", ".rb", ".sh":
		return "ƒ"
	case ".md", ".txt", ".rst", ".log":
		return "≣"
	case ".json", ".yaml", ".yml", ".toml", ".ini", ".cfg", ".conf", ".xml":
		return "⚙"
	case ".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".bmp":
		return "▦"
	case ".zip", ".tar", ".gz", ".tgz", ".bz2", ".xz", ".7z", ".rar":
		return "▤"
	case ".pdf":
		return "▥"
	case ".mp3", ".wav", ".flac", ".ogg", ".mp4", ".mkv", ".mov", ".avi":
		return "♪"
	default:
		return "▢"
	}
}

func (m filesModel) renderPaneHeader(side, cw int, focused bool) string {
	pane := m.paneRefConst(side)

	srcStyle := lipgloss.NewStyle().Foreground(fmMutedC).Bold(true)
	icon := "🖥"
	if pane.remote {
		icon = "☁"
	}
	if focused {
		srcStyle = srcStyle.Foreground(fmAccent)
	}
	src := srcStyle.Render(icon + " " + pane.label())

	count := ""
	if !pane.loading {
		count = fmCount.Render(fmt.Sprintf("(%d)", countReal(pane.entries)))
	}
	sel := ""
	if n := len(pane.selected); n > 0 {
		sel = lipgloss.NewStyle().Foreground(fmAccent2).Render(fmt.Sprintf(" • %d sel", n))
	}

	crumbW := cw - lipgloss.Width(src) - lipgloss.Width(count) - lipgloss.Width(sel) - 2
	if crumbW < 6 {
		crumbW = 6
	}
	crumb := renderBreadcrumb(pane.cwd, pane.remote, crumbW)

	line := src + "  " + crumb
	// right-align the count + selection
	used := lipgloss.Width(line) + lipgloss.Width(count) + lipgloss.Width(sel)
	pad := cw - used
	if pad < 1 {
		pad = 1
	}
	return line + strings.Repeat(" ", pad) + count + sel
}

// renderBreadcrumb renders a path as accented segments separated by ›, trimming
// leading segments to fit the width.
func renderBreadcrumb(cwd string, remote bool, width int) string {
	sep := func(p string) []string {
		if remote {
			p = path.Clean(p)
		} else {
			p = filepath.Clean(p)
		}
		if p == "/" || p == "." {
			return []string{"/"}
		}
		parts := strings.Split(strings.Trim(p, "/"), "/")
		return append([]string{"/"}, parts...)
	}
	segs := sep(cwd)
	// Build from the right until we run out of width.
	sepGlyph := fmCrumbSep.Render(" › ")
	var rendered []string
	for i, s := range segs {
		style := fmCrumbSty
		if i == len(segs)-1 {
			style = fmCrumbCur
		}
		txt := s
		if s == "/" {
			txt = "/"
		}
		rendered = append(rendered, style.Render(txt))
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
	return fmCrumbCur.Render(truncate(segs[len(segs)-1], width))
}

// renderRow draws one big, full-width file row.
func (m filesModel) renderRow(side, i, cw int, dropTargetPane bool) string {
	pane := m.paneRefConst(side)
	item := pane.entries[i]
	focused := side == m.focus
	selected := i == pane.index && focused
	marked := pane.selected[i]
	hovered := side == m.hoverSide && i == m.hoverIndex
	dropHover := dropTargetPane && i == m.hoverIndex && item.isDir

	icon := glyphFor(item)
	name := item.name
	if item.isDir && item.name != ".." {
		name += "/"
	}

	// Column layout: " <mark> <icon>  <name....>  <size>  <time> ".
	mark := " "
	if marked {
		mark = "✓"
	}
	sizeStr := ""
	if !item.isDir {
		sizeStr = humanSize(item.size)
	}
	timeStr := ""
	if !item.modTime.IsZero() {
		timeStr = item.modTime.Format("Jan 02 15:04")
	}
	sizeW := 9
	timeW := 12
	// leading space + mark(1) + sp + icon(1) + sp + ... + sizeW + sp + timeW + trailing sp
	nameW := cw - sizeW - timeW - 9
	if nameW < 6 {
		nameW = 6
		timeW = 0
		timeStr = ""
	}

	plain := fmt.Sprintf(" %s %s  %-*s %*s  %-*s ",
		mark, icon, nameW, truncate(name, nameW), sizeW, sizeStr, timeW, timeStr)

	// Style precedence: selected > drop-hover > marked > hover > zebra.
	switch {
	case selected:
		return fmSelRow.Width(cw).Render(plain)
	case dropHover:
		return fmDropRow.Width(cw).Render(plain)
	case marked:
		return fmMarkRow.Width(cw).Render(plain)
	case hovered:
		style := fmHoverRow
		if item.isDir {
			style = style.Foreground(fmDirC)
		}
		return style.Width(cw).Render(plain)
	default:
		base := fmFileRow
		if item.isDir {
			base = fmDirRow
		}
		if i%2 == 1 {
			base = base.Background(fmZebraC)
		}
		return base.Width(cw).Render(plain)
	}
}

// glyphFor returns a prominent left glyph by kind/extension.
func glyphFor(item fileItem) string {
	if item.name == ".." {
		return "↩"
	}
	if item.isDir {
		return "▸"
	}
	if item.symlink {
		return "↪"
	}
	ext := strings.ToLower(filepath.Ext(item.name))
	switch ext {
	case ".go", ".rs", ".c", ".h", ".cpp", ".py", ".js", ".ts", ".java", ".rb", ".sh":
		return "ƒ"
	case ".md", ".txt", ".rst", ".log":
		return "≣"
	case ".json", ".yaml", ".yml", ".toml", ".ini", ".cfg", ".conf", ".xml":
		return "⚙"
	case ".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".bmp":
		return "▦"
	case ".zip", ".tar", ".gz", ".tgz", ".bz2", ".xz", ".7z", ".rar":
		return "▤"
	case ".pdf":
		return "▥"
	case ".mp3", ".wav", ".flac", ".ogg", ".mp4", ".mkv", ".mov", ".avi":
		return "♪"
	default:
		return "·"
	}
}

// ============================================================================
// Transfers dock
// ============================================================================

func (m filesModel) renderTransfers(innerW int) string {
	if len(m.transfers) == 0 {
		return ""
	}
	var b strings.Builder
	active := 0
	for _, t := range m.transfers {
		if !t.done {
			active++
		}
	}
	title := fmt.Sprintf(" Transfers  (%d active)", active)
	b.WriteString(lipgloss.NewStyle().Foreground(fmText).Bold(true).Render(title))
	b.WriteString("\n")
	start := 0
	if len(m.transfers) > 4 {
		start = len(m.transfers) - 4
	}
	barW := 24
	for _, t := range m.transfers[start:] {
		pct := 0
		if t.total > 0 {
			pct = int(t.bytesDone * 100 / t.total)
		}
		if t.done && t.err == nil {
			pct = 100
		}
		var meta string
		switch {
		case t.err != nil:
			meta = fmErrSty.Render("failed: " + truncate(t.err.Error(), 30))
		case t.done:
			meta = fmDoneSty.Render("done · " + humanSize(t.total))
		default:
			meta = fmSizeCol.Render(fmt.Sprintf("%s/s · %d streams · %s left",
				humanSize(int64(t.rate)), t.streams, formatETA(t)))
		}
		label := lipgloss.NewStyle().Foreground(fmText).Render(fmt.Sprintf(" %-18s", truncate(t.label, 18)))
		fmt.Fprintf(&b, "%s %s  %s\n", label, smoothBar(pct, barW), meta)
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(fmBorderC).
		Width(innerW - 2).
		Render(strings.TrimRight(b.String(), "\n"))
}

// smoothBar renders a sub-cell-precise progress bar using eighth blocks.
func smoothBar(pct, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	eighths := pct * width * 8 / 100
	full := eighths / 8
	rem := eighths % 8
	bar := strings.Repeat("█", full)
	used := full
	if rem > 0 && full < width {
		bar += string([]rune("▏▎▍▌▋▊▉")[rem-1])
		used++
	}
	empty := width - used
	if empty < 0 {
		empty = 0
	}
	filled := lipgloss.NewStyle().Foreground(fmAccent).Render(bar)
	rest := lipgloss.NewStyle().Foreground(fmBorderC).Render(strings.Repeat("░", empty))
	return filled + rest + lipgloss.NewStyle().Foreground(fmMutedC).Render(fmt.Sprintf(" %3d%%", pct))
}

// ============================================================================
// Footer hints
// ============================================================================

func (m filesModel) renderFooter() string {
	hints := [][2]string{
		{"↑↓", "move"}, {"↵/→", "open"}, {"←", "up"}, {"space", "select"},
		{"s", "source"}, {"c/m", "copy/move"}, {"n", "new"}, {"d", "del"},
		{"i", "info"}, {"v", "view"}, {".", "hidden"}, {"drag", "transfer"}, {"q", "quit"},
	}
	parts := make([]string, 0, len(hints))
	for _, h := range hints {
		parts = append(parts, fmKeyChip.Render(h[0])+" "+fmHintLabel.Render(h[1]))
	}
	return " " + strings.Join(parts, "  ")
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
	}
	return base
}

func (m filesModel) renderSourcePicker() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Foreground(fmAccent).Bold(true).Render("Open source in " + sideName(m.pickerSide) + " pane"))
	b.WriteString("\n\n")
	for i, it := range m.pickerItems {
		icon := "🖥"
		dot := ""
		if it != "Local" {
			icon = "☁"
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
		line := fmt.Sprintf(" %s  %-14s%s ", icon, it, dot)
		var styled string
		if i == m.pickerIndex {
			styled = fmSelRow.Render(line)
		} else {
			styled = lipgloss.NewStyle().Foreground(fmText).Render(line)
		}
		b.WriteString(zone.Mark(fmt.Sprintf("%s%d", fmPickPrefix, i), styled))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(fmStatusSty.Render("↑↓ choose · ↵ open · esc cancel"))
	return fmOverlayBox.Render(b.String())
}

func (m filesModel) renderContextMenu() string {
	var b strings.Builder
	for i, it := range m.menuItems {
		key := lipgloss.NewStyle().Foreground(fmAccent2).Render(fmt.Sprintf("%-2s", it.key))
		label := it.label
		var line string
		switch {
		case !it.enabled:
			line = lipgloss.NewStyle().Foreground(fmDimC).Render(fmt.Sprintf(" %s  %s ", key, label))
		case i == m.menuIndex:
			line = fmSelRow.Render(fmt.Sprintf(" %s  %s ", it.key, label))
		default:
			line = fmt.Sprintf(" %s  %s ", key, lipgloss.NewStyle().Foreground(fmText).Render(label))
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
		moveBtn = lipgloss.NewStyle().Foreground(fmText).Render(moveBtn)
	} else {
		copyBtn = lipgloss.NewStyle().Foreground(fmText).Render(copyBtn)
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
	title := "Confirm"
	titleStyle := lipgloss.NewStyle().Foreground(fmWarnC).Bold(true)
	if m.confirm == confirmDelete {
		title = "Delete"
		titleStyle = lipgloss.NewStyle().Foreground(fmDangerC).Bold(true)
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("⚠  " + title))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Foreground(fmText).Render(m.confirmText))
	b.WriteString("\n\n")
	b.WriteString(fmDoneSty.Render("Enter") + fmStatusSty.Render(" confirm   ") +
		fmErrSty.Render("Esc") + fmStatusSty.Render(" cancel"))
	box := fmOverlayBox
	if m.confirm == confirmDelete {
		box = box.BorderForeground(fmDangerC)
	} else {
		box = box.BorderForeground(fmWarnC)
	}
	return box.Render(b.String())
}

func (m filesModel) renderPrompt() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Foreground(fmAccent).Bold(true).Render(m.promptLabel))
	b.WriteString("\n\n")
	field := m.promptValue + "▏"
	inputW := 40
	if lipgloss.Width(field) > inputW {
		inputW = lipgloss.Width(field)
	}
	input := lipgloss.NewStyle().
		Background(lipgloss.Color("#101822")).
		Foreground(fmText).
		Width(inputW).
		Padding(0, 1).
		Render(field)
	b.WriteString(input)
	b.WriteString("\n\n")
	b.WriteString(fmStatusSty.Render("↵ confirm · esc cancel"))
	return fmOverlayBox.Render(b.String())
}

func (m filesModel) renderProperties() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Foreground(fmAccent).Bold(true).Render("Properties"))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Foreground(fmText).Render(m.propsText))
	b.WriteString("\n\n")
	b.WriteString(fmStatusSty.Render("esc / ↵ close"))
	return fmOverlayBox.Render(b.String())
}

// ============================================================================
// Drag ghost (floating, follows cursor)
// ============================================================================

func (m filesModel) composeGhost(base string) string {
	if m.drag == nil || !m.drag.active {
		return base
	}
	d := m.drag
	icon := glyphFor(d.primary)
	name := truncate(d.primary.name, 18)
	label := fmt.Sprintf(" %s %s ", icon, name)
	if len(d.items) > 1 {
		label += lipgloss.NewStyle().Background(fmAccent).Foreground(fmInk).Bold(true).
			Render(fmt.Sprintf(" +%d ", len(d.items)-1))
	}
	ghost := lipgloss.NewStyle().
		Background(lipgloss.Color("#10303a")).
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
	return left + overlay + right
}

// ============================================================================
// ANSI-aware horizontal slicing helpers
// ============================================================================

// truncateANSI returns the prefix of s that occupies the first n display columns,
// preserving styling, using lipgloss's truncation.
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
	// Take the full string, drop the first n visible cols by truncating the head
	// then removing it. lipgloss has no native left-trim, so walk rune cells.
	return trimVisibleLeft(s, n)
}

// truncateVisible walks the string honoring ANSI escapes, returning the prefix
// covering n visible columns.
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
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if visible >= n {
			break
		}
		out.WriteRune(r)
		visible++
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
			if visible >= n {
				out.WriteRune(r)
			}
			continue
		}
		if inEsc {
			if visible >= n {
				out.WriteRune(r)
			}
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if visible < n {
			visible++
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// ============================================================================
// Misc render helpers
// ============================================================================

func sepColumn(height int) string {
	line := fmRule.Render(" ┃ ")
	lines := make([]string, height)
	for i := range lines {
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

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
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
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
