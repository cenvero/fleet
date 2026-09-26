// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ============================================================================
// Help overlay (?): every binding, grouped by purpose
// ============================================================================

type fmHelpEntry struct{ keys, desc string }

type fmHelpGroup struct {
	title   string
	entries []fmHelpEntry
}

func fmHelpGroups() []fmHelpGroup {
	return []fmHelpGroup{
		{"Navigate", []fmHelpEntry{
			{"↑ ↓  j k", "move"},
			{"PgUp PgDn", "page up / down"},
			{"Home End", "first / last item"},
			{"↵  →  l", "open folder · file info"},
			{"←  h  ⌫", "parent folder"},
			{"Tab  ⇧Tab", "switch pane"},
			{"Alt+←  H", "back"},
			{"Alt+→  L", "forward"},
			{":  Ctrl+G", "go to path (Tab completes)"},
			{"f  Ctrl+P", "jump to item (fuzzy)"},
			{"~", "home / server's start folder"},
			{"'", "places: bookmarks & recent"},
			{"b", "bookmark this folder"},
			{"=", "mirror navigation (both panes)"},
			{"s", "change pane source"},
		}},
		{"Select", []fmHelpEntry{
			{"Space", "select & advance"},
			{"⇧↑ ⇧↓", "extend selection"},
			{"V", "range-select mode"},
			{"Ctrl+A", "select all"},
			{"*", "invert selection"},
			{"Esc", "clear selection, then filter"},
		}},
		{"Files", []fmHelpEntry{
			{"e", "view / edit with highlighting"},
			{"i", "properties"},
			{"n  N", "new folder / new file"},
			{"r", "rename"},
			{"d  Del", "delete (asks first)"},
			{"D", "duplicate"},
			{"p", "permissions (chmod)"},
			{"#", "SHA-256 checksum"},
			{"z  x", "compress / extract"},
		}},
		{"Transfer", []fmHelpEntry{
			{"c", "copy to other pane"},
			{"m", "move to other pane"},
			{"u", "copy (upload) to other pane"},
			{"t", "focus the transfer queue"},
			{"  ↑↓", "select a transfer"},
			{"  x  X", "cancel queued / all queued"},
			{"  r  R", "retry / retry all failed"},
			{"C", "clear finished transfers"},
		}},
		{"View", []fmHelpEntry{
			{"/", "filter by name"},
			{"o", "cycle sort (or click a column)"},
			{"v", "list / icon view"},
			{"P  F3", "preview pane"},
			{".", "show hidden files"},
			{"g  Ctrl+R", "refresh pane / both"},
			{"?  F1", "this help"},
			{"q  Ctrl+C", "quit"},
		}},
		{"Mouse", []fmHelpEntry{
			{"click", "select · double-click opens"},
			{"wheel", "scroll"},
			{"drag → pane", "copy / move menu"},
			{"drag → folder", "move into it"},
			{"right-click", "context menu"},
			{"⇧click", "select range"},
			{"Ctrl/Alt+click", "toggle selection"},
			{"crumbs", "jump to that folder"},
			{"column title", "sort by it"},
			{"pane title", "change source"},
		}},
		{"Editor", []fmHelpEntry{
			{"Tab  Ctrl+E", "view ⇄ edit"},
			{"Ctrl+S", "save"},
			{"Esc", "close (twice to discard)"},
		}},
	}
}

func (m filesModel) openHelp() filesModel {
	m.overlay = overlayHelp
	m.helpScroll = 0
	return m
}

// helpLines lays the groups out in as many columns as fit, returning styled
// lines.
func (m filesModel) helpLines(width int) []string {
	groups := fmHelpGroups()
	keyW := 14
	descW := 30
	colW := keyW + 1 + descW
	cols := (width + 3) / (colW + 3)
	if cols < 1 {
		cols = 1
		colW = width
		descW = colW - keyW - 1
		if descW < 10 {
			descW = 10
		}
	}
	if cols > 3 {
		cols = 3
	}
	keySty := lipgloss.NewStyle().Foreground(fmAccent2).Bold(true)
	head := lipgloss.NewStyle().Foreground(fmAccent).Bold(true)

	render := func(g fmHelpGroup) []string {
		out := []string{head.Render(g.title)}
		for _, e := range g.entries {
			k := fmPadRight(e.keys, keyW)
			out = append(out, keySty.Render(k)+" "+fmTextSty.Render(fmPadRight(fmFit(e.desc, descW), descW)))
		}
		return append(out, "")
	}
	// Distribute groups into columns, balancing by height.
	columns := make([][]string, cols)
	for _, g := range groups {
		best := 0
		for c := 1; c < cols; c++ {
			if len(columns[c]) < len(columns[best]) {
				best = c
			}
		}
		columns[best] = append(columns[best], render(g)...)
	}
	height := 0
	for _, c := range columns {
		if len(c) > height {
			height = len(c)
		}
	}
	lines := make([]string, 0, height)
	blank := strings.Repeat(" ", colW)
	for r := range height {
		var b strings.Builder
		for c := range cols {
			if c > 0 {
				b.WriteString("   ")
			}
			if r < len(columns[c]) {
				cell := columns[c][r]
				b.WriteString(cell)
				if w := lipgloss.Width(cell); w < colW {
					b.WriteString(strings.Repeat(" ", colW-w))
				}
			} else {
				b.WriteString(blank)
			}
		}
		lines = append(lines, b.String())
	}
	return lines
}

func (m filesModel) helpViewport() (width, rows int) {
	width = m.width - 8
	if width > 3*48+6 {
		width = 3*48 + 6
	}
	if width < 30 {
		width = 30
	}
	rows = m.height - 8
	if rows < 4 {
		rows = 4
	}
	return width, rows
}

func (m filesModel) renderHelp() string {
	width, rows := m.helpViewport()
	lines := m.helpLines(width)
	maxScroll := len(lines) - rows
	if maxScroll < 0 {
		maxScroll = 0
	}
	top := m.helpScroll
	if top > maxScroll {
		top = maxScroll
	}
	if top < 0 {
		top = 0
	}
	end := top + rows
	if end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	title := fmTitleSty.Render("Keyboard & mouse")
	scroll := ""
	if maxScroll > 0 {
		scroll = fmDimSty.Render("   ↑↓ scroll")
	}
	b.WriteString(title + scroll + fmDimSty.Render("   esc close"))
	b.WriteString("\n\n")
	b.WriteString(strings.Join(lines[top:end], "\n"))
	return fmOverlayBox.Padding(0, 2).Render(b.String())
}

func (m filesModel) handleHelpKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	_, rows := m.helpViewport()
	width, _ := m.helpViewport()
	maxScroll := len(m.helpLines(width)) - rows
	if maxScroll < 0 {
		maxScroll = 0
	}
	switch msg.String() {
	case "esc", "q", "?", "f1", "enter":
		m.overlay = overlayNone
	case "up", "k":
		m.helpScroll--
	case "down", "j":
		m.helpScroll++
	case "pgup":
		m.helpScroll -= rows
	case "pgdown", " ":
		m.helpScroll += rows
	case "home", "g":
		m.helpScroll = 0
	case "end", "G":
		m.helpScroll = maxScroll
	case "ctrl+c":
		return m, tea.Quit
	}
	if m.helpScroll > maxScroll {
		m.helpScroll = maxScroll
	}
	if m.helpScroll < 0 {
		m.helpScroll = 0
	}
	return m, nil
}
