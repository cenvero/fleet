// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cenvero/fleet/internal/core"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"
)

// RunFiles launches the dual-pane file manager for a server. The left pane is
// the local filesystem, the right pane is the remote server. Files are moved by
// dragging between panes (mouse) or with u/d (keyboard).
func RunFiles(configDir, server string) error {
	app, err := core.Open(configDir)
	if err != nil {
		return err
	}
	defer app.Close()

	// bubblezone is the safe, content-anchored way to track mouse zones in
	// Bubble Tea — it replaces brittle manual x/y coordinate math. The global
	// manager records every zone.Mark during zone.Scan (in View) and answers
	// InBounds queries on the next mouse event.
	zone.NewGlobal()

	cwd, _ := os.Getwd()
	if cwd == "" {
		cwd = "/"
	}
	remoteStart := "/"
	if d, err := app.FileTransferDefaultsFor(server); err == nil && d.RemoteDir != "" {
		remoteStart = d.RemoteDir
	}

	m := filesModel{
		app:        app,
		server:     server,
		width:      120,
		height:     36,
		left:       paneState{cwd: cwd, loading: true},
		right:      paneState{cwd: remoteStart, remote: true, loading: true},
		focus:      0,
		chans:      make(map[int]*transferChans),
		hoverSide:  -1,
		hoverIndex: -1,
	}
	_, err = tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseAllMotion()).Run()
	return err
}

type fileItem struct {
	name  string
	isDir bool
	size  int64
}

type paneState struct {
	cwd     string
	entries []fileItem
	index   int
	scroll  int
	loading bool
	err     error
	remote  bool
}

type transferRow struct {
	id        int
	label     string
	bytesDone int64
	total     int64
	rate      float64
	streams   int
	done      bool
	err       error
}

type transferChans struct {
	progress chan core.ProgressUpdate
	done     chan transferOutcome
}

type transferOutcome struct{ err error }

type dragState struct {
	fromSide  int
	fromIndex int
	item      fileItem
}

type filesModel struct {
	app           *core.App
	server        string
	width, height int
	left, right   paneState
	focus         int // 0 = left/local, 1 = right/remote
	status        string
	transfers     []*transferRow
	nextID        int
	chans         map[int]*transferChans
	drag          *dragState
	hoverSide     int // pane index currently hovered by the mouse, -1 = none
	hoverIndex    int // row index currently hovered, -1 = none
}

// ---- messages ----

type paneLoadedMsg struct {
	side  int
	cwd   string
	items []fileItem
	err   error
}

type progressTickMsg struct {
	id int
	u  core.ProgressUpdate
}

type transferDoneMsg struct {
	id    int
	label string
	err   error
}

func (m filesModel) Init() tea.Cmd {
	return tea.Batch(
		loadLocalCmd(0, m.left.cwd),
		loadRemoteCmd(1, m.app, m.server, m.right.cwd),
	)
}

func loadLocalCmd(side int, cwd string) tea.Cmd {
	return func() tea.Msg {
		entries, err := os.ReadDir(cwd)
		if err != nil {
			return paneLoadedMsg{side: side, cwd: cwd, err: err}
		}
		items := make([]fileItem, 0, len(entries))
		for _, e := range entries {
			fi := fileItem{name: e.Name(), isDir: e.IsDir()}
			if info, err := e.Info(); err == nil {
				fi.size = info.Size()
			}
			items = append(items, fi)
		}
		return paneLoadedMsg{side: side, cwd: cwd, items: withParent(cwd, items, false)}
	}
}

func loadRemoteCmd(side int, app *core.App, server, cwd string) tea.Cmd {
	return func() tea.Msg {
		result, err := app.ListRemoteDir(server, cwd)
		if err != nil {
			return paneLoadedMsg{side: side, cwd: cwd, err: err}
		}
		resolved := result.Path
		if resolved == "" {
			resolved = cwd
		}
		items := make([]fileItem, 0, len(result.Entries))
		for _, e := range result.Entries {
			items = append(items, fileItem{name: e.Name, isDir: e.IsDir, size: e.Size})
		}
		return paneLoadedMsg{side: side, cwd: resolved, items: withParent(resolved, items, true)}
	}
}

// withParent prepends a ".." entry unless at the filesystem root.
func withParent(cwd string, items []fileItem, remote bool) []fileItem {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].isDir != items[j].isDir {
			return items[i].isDir
		}
		return strings.ToLower(items[i].name) < strings.ToLower(items[j].name)
	})
	atRoot := cwd == "/" || (!remote && filepath.Dir(cwd) == cwd)
	if atRoot {
		return items
	}
	return append([]fileItem{{name: "..", isDir: true}}, items...)
}

func (m filesModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case paneLoadedMsg:
		pane := &m.left
		if msg.side == 1 {
			pane = &m.right
		}
		pane.cwd = msg.cwd
		pane.entries = msg.items
		pane.err = msg.err
		pane.loading = false
		if pane.index >= len(pane.entries) {
			pane.index = 0
		}
		return m, nil

	case progressTickMsg:
		m.applyProgress(msg.id, msg.u)
		if c, ok := m.chans[msg.id]; ok {
			return m, pollTransferCmd(msg.id, c)
		}
		return m, nil

	case transferDoneMsg:
		for _, t := range m.transfers {
			if t.id == msg.id {
				t.done = true
				t.err = msg.err
				if msg.err == nil {
					t.bytesDone = t.total
				}
			}
		}
		delete(m.chans, msg.id)
		if msg.err == nil {
			m.status = "completed: " + msg.label
		} else {
			m.status = "failed: " + msg.label + " — " + msg.err.Error()
		}
		// Refresh the destination pane so the new file shows up.
		return m, tea.Batch(loadLocalCmd(0, m.left.cwd), loadRemoteCmd(1, m.app, m.server, m.right.cwd))

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)
	}
	return m, nil
}

func (m filesModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab":
		m.focus = 1 - m.focus
		return m, nil
	case "up", "k":
		m.movePane(m.focus, -1)
		return m, nil
	case "down", "j":
		m.movePane(m.focus, 1)
		return m, nil
	case "left", "h":
		return m.enterParent(m.focus)
	case "right", "l", "enter":
		return m.activate(m.focus)
	case "r":
		m.left.loading, m.right.loading = true, true
		return m, tea.Batch(loadLocalCmd(0, m.left.cwd), loadRemoteCmd(1, m.app, m.server, m.right.cwd))
	case "u":
		return m.startTransfer(0, m.focusedItem(0))
	case "d":
		return m.startTransfer(1, m.focusedItem(1))
	}
	return m, nil
}

func (m *filesModel) movePane(side, delta int) {
	pane := m.paneRef(side)
	if len(pane.entries) == 0 {
		return
	}
	pane.index += delta
	if pane.index < 0 {
		pane.index = 0
	}
	if pane.index >= len(pane.entries) {
		pane.index = len(pane.entries) - 1
	}
	visible := m.visibleRows()
	if pane.index < pane.scroll {
		pane.scroll = pane.index
	}
	if pane.index >= pane.scroll+visible {
		pane.scroll = pane.index - visible + 1
	}
}

func (m filesModel) enterParent(side int) (tea.Model, tea.Cmd) {
	pane := m.paneRef(side)
	parent := parentDir(pane.cwd, pane.remote)
	if parent == pane.cwd {
		return m, nil
	}
	pane.cwd = parent
	pane.index, pane.scroll = 0, 0
	pane.loading = true
	return m, m.reloadPane(side, parent)
}

func (m filesModel) activate(side int) (tea.Model, tea.Cmd) {
	pane := m.paneRef(side)
	item := m.focusedItem(side)
	if item.name == "" {
		return m, nil
	}
	if item.name == ".." {
		return m.enterParent(side)
	}
	if item.isDir {
		next := joinPath(pane.cwd, item.name, pane.remote)
		pane.cwd = next
		pane.index, pane.scroll = 0, 0
		pane.loading = true
		return m, m.reloadPane(side, next)
	}
	// A file: trigger the transfer to the opposite pane.
	return m.startTransfer(side, item)
}

func (m filesModel) reloadPane(side int, cwd string) tea.Cmd {
	if side == 0 {
		return loadLocalCmd(0, cwd)
	}
	return loadRemoteCmd(1, m.app, m.server, cwd)
}

// zone ids
func rowZoneID(side, i int) string { return fmt.Sprintf("fm-row-%d-%d", side, i) }
func paneZoneID(side int) string   { return fmt.Sprintf("fm-pane-%d", side) }

const fmActPrefix = "fm-act-"

var footerActions = []string{"switch", "open", "upload", "download", "refresh", "quit"}

// hitRow resolves a mouse event to (paneSide, rowIndex, inSomePane) using the
// zones recorded during the last View. rowIndex is -1 when the event is inside a
// pane but not over a row.
func (m filesModel) hitRow(msg tea.MouseMsg) (int, int, bool) {
	for side := range 2 {
		if !zone.Get(paneZoneID(side)).InBounds(msg) {
			continue
		}
		pane := m.paneRefConst(side)
		for i := range pane.entries {
			if zone.Get(rowZoneID(side, i)).InBounds(msg) {
				return side, i, true
			}
		}
		return side, -1, true
	}
	return -1, -1, false
}

func hitFooterAction(msg tea.MouseMsg) (string, bool) {
	for _, name := range footerActions {
		if zone.Get(fmActPrefix + name).InBounds(msg) {
			return name, true
		}
	}
	return "", false
}

func (m filesModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if side, _, ok := m.hitRow(msg); ok {
			m.movePane(side, -1)
		}
		return m, nil
	case tea.MouseButtonWheelDown:
		if side, _, ok := m.hitRow(msg); ok {
			m.movePane(side, 1)
		}
		return m, nil
	}

	switch msg.Action {
	case tea.MouseActionMotion:
		if side, idx, ok := m.hitRow(msg); ok && idx >= 0 {
			m.hoverSide, m.hoverIndex = side, idx
		} else {
			m.hoverSide, m.hoverIndex = -1, -1
		}
		return m, nil

	case tea.MouseActionPress:
		if msg.Button != tea.MouseButtonLeft {
			return m, nil
		}
		side, idx, ok := m.hitRow(msg)
		if !ok {
			return m, nil
		}
		m.focus = side
		if idx >= 0 {
			pane := m.paneRef(side)
			pane.index = idx
			m.drag = &dragState{fromSide: side, fromIndex: idx, item: pane.entries[idx]}
		}
		return m, nil

	case tea.MouseActionRelease:
		if msg.Button != tea.MouseButtonLeft {
			return m, nil
		}
		// Footer buttons take priority over pane interactions.
		if act, ok := hitFooterAction(msg); ok {
			m.drag = nil
			return m.applyAction(act)
		}
		drag := m.drag
		m.drag = nil
		if drag == nil {
			return m, nil
		}
		relSide, relIdx, inPane := m.hitRow(msg)
		switch {
		case inPane && relSide == drag.fromSide && relIdx == drag.fromIndex:
			// A click on the row it started on: open a directory, ignore a file.
			if drag.item.isDir || drag.item.name == ".." {
				return m.activate(drag.fromSide)
			}
			return m, nil
		case inPane && relSide != drag.fromSide && !drag.item.isDir && drag.item.name != "..":
			// Dragged a file onto the other pane → transfer it.
			return m.startTransfer(drag.fromSide, drag.item)
		}
		return m, nil
	}
	return m, nil
}

func (m filesModel) applyAction(name string) (tea.Model, tea.Cmd) {
	switch name {
	case "switch":
		m.focus = 1 - m.focus
		return m, nil
	case "open":
		return m.activate(m.focus)
	case "upload":
		return m.startTransfer(0, m.focusedItem(0))
	case "download":
		return m.startTransfer(1, m.focusedItem(1))
	case "refresh":
		m.left.loading, m.right.loading = true, true
		return m, tea.Batch(loadLocalCmd(0, m.left.cwd), loadRemoteCmd(1, m.app, m.server, m.right.cwd))
	case "quit":
		return m, tea.Quit
	}
	return m, nil
}

// startTransfer uploads (local item -> remote cwd) or downloads (remote item ->
// local cwd) the given item. side is the source pane.
func (m filesModel) startTransfer(side int, item fileItem) (tea.Model, tea.Cmd) {
	if item.name == "" || item.name == ".." || item.isDir {
		m.status = "select a file to transfer"
		return m, nil
	}
	id := m.nextID
	m.nextID++
	c := &transferChans{
		progress: make(chan core.ProgressUpdate, 1),
		done:     make(chan transferOutcome, 1),
	}
	m.chans[id] = c

	var label string
	app, server := m.app, m.server
	progress := func(u core.ProgressUpdate) {
		select {
		case c.progress <- u:
		default: // drop to keep transfer workers unblocked
		}
	}

	if side == 0 {
		// upload local -> remote
		localPath := filepath.Join(m.left.cwd, item.name)
		remotePath := joinPath(m.right.cwd, item.name, true)
		label = fmt.Sprintf("↑ %s", item.name)
		go func() {
			_, err := app.UploadFile(server, localPath, remotePath, core.FileTransferOptions{}, progress)
			c.done <- transferOutcome{err: err}
		}()
	} else {
		// download remote -> local
		remotePath := joinPath(m.right.cwd, item.name, true)
		localPath := filepath.Join(m.left.cwd, item.name)
		label = fmt.Sprintf("↓ %s", item.name)
		go func() {
			_, err := app.DownloadFile(server, remotePath, localPath, core.FileTransferOptions{}, progress)
			c.done <- transferOutcome{err: err}
		}()
	}

	m.transfers = append(m.transfers, &transferRow{id: id, label: label, total: item.size})
	m.status = "started " + label
	return m, pollTransferCmd(id, c)
}

func pollTransferCmd(id int, c *transferChans) tea.Cmd {
	return func() tea.Msg {
		select {
		case u := <-c.progress:
			return progressTickMsg{id: id, u: u}
		case out := <-c.done:
			return transferDoneMsg{id: id, err: out.err}
		}
	}
}

func (m *filesModel) applyProgress(id int, u core.ProgressUpdate) {
	for _, t := range m.transfers {
		if t.id == id {
			t.bytesDone = u.BytesDone
			if u.TotalBytes > 0 {
				t.total = u.TotalBytes
			}
			t.rate = u.RatePerSec
			t.streams = u.ActiveStreams
		}
	}
}

// ---- helpers for panes ----

func (m *filesModel) paneRef(side int) *paneState {
	if side == 1 {
		return &m.right
	}
	return &m.left
}

func (m filesModel) focusedItem(side int) fileItem {
	pane := m.paneRefConst(side)
	if pane.index < 0 || pane.index >= len(pane.entries) {
		return fileItem{}
	}
	return pane.entries[pane.index]
}

func (m filesModel) paneRefConst(side int) paneState {
	if side == 1 {
		return m.right
	}
	return m.left
}

func (m filesModel) visibleRows() int {
	// header(1) + blank(1) + pane chrome(border2 + title + path + rule = 5) +
	// status(1) + footer(1) + page padding(2) + transfers headroom(~3).
	h := m.height - 14
	if h < 4 {
		h = 4
	}
	return h
}

// ---- path helpers ----

func parentDir(cwd string, remote bool) string {
	if remote {
		return path.Dir(cwd)
	}
	return filepath.Dir(cwd)
}

func joinPath(cwd, name string, remote bool) string {
	if remote {
		return path.Join(cwd, name)
	}
	return filepath.Join(cwd, name)
}

// ---- file-manager palette & styles (self-contained, Charm-grade polish) ----

var (
	fmAccent   = lipgloss.Color("#00d4aa")
	fmAccent2  = lipgloss.Color("#36f0c0")
	fmInk      = lipgloss.Color("#04231d")
	fmText     = lipgloss.Color("#e7ecef")
	fmMutedC   = lipgloss.Color("#8fa7b3")
	fmDimC     = lipgloss.Color("#5f7480")
	fmBorderC  = lipgloss.Color("#1c2b36")
	fmDirC     = lipgloss.Color("#7ad7ff")
	fmDangerC  = lipgloss.Color("#ff6b6b")
	fmHeaderBg = lipgloss.Color("#0e1620")

	fmHeaderBar = lipgloss.NewStyle().Background(fmHeaderBg)
	fmBrand     = lipgloss.NewStyle().Foreground(fmAccent).Bold(true)
	fmTag       = lipgloss.NewStyle().Foreground(fmDimC)
	fmServerTag = lipgloss.NewStyle().Foreground(fmMutedC)

	fmRule    = lipgloss.NewStyle().Foreground(fmBorderC)
	fmPathSty = lipgloss.NewStyle().Foreground(fmDimC).Italic(true)
	fmCount   = lipgloss.NewStyle().Foreground(fmDimC)

	fmSelRow   = lipgloss.NewStyle().Background(fmAccent).Foreground(fmInk).Bold(true)
	fmHoverRow = lipgloss.NewStyle().Background(lipgloss.Color("#15212c")).Foreground(fmText)
	fmDirRow   = lipgloss.NewStyle().Foreground(fmDirC)
	fmFileRow  = lipgloss.NewStyle().Foreground(fmText)
	fmSizeCol  = lipgloss.NewStyle().Foreground(fmDimC)

	fmKeyChip   = lipgloss.NewStyle().Background(fmBorderC).Foreground(fmAccent2).Bold(true).Padding(0, 1)
	fmHintLabel = lipgloss.NewStyle().Foreground(fmMutedC)
	fmStatusSty = lipgloss.NewStyle().Foreground(fmMutedC)
	fmDoneSty   = lipgloss.NewStyle().Foreground(fmAccent).Bold(true)
	fmErrSty    = lipgloss.NewStyle().Foreground(fmDangerC).Bold(true)
)

func (m filesModel) View() string {
	innerW := m.width - 4 // page padding (1,2)
	if innerW < 40 {
		innerW = 40
	}

	header := m.renderHeader(innerW)

	sepW := 3
	paneWidth := (innerW - sepW) / 2
	if paneWidth < 24 {
		paneWidth = 24
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
	statusLine := fmStatusSty.Render("  " + status)
	transfers := m.renderTransfers(innerW)
	footer := footerHints()

	parts := []string{header, "", panes, statusLine}
	if transfers != "" {
		parts = append(parts, transfers)
	}
	parts = append(parts, footer)
	body := lipgloss.JoinVertical(lipgloss.Left, parts...)
	// zone.Scan records every marked zone's position and strips the markers.
	// Must wrap the final root output and run exactly once per frame.
	return zone.Scan(pageStyle.Render(body))
}

func (m filesModel) renderHeader(innerW int) string {
	brand := fmBrand.Render("◆ Cenvero Fleet") + fmTag.Render("  ·  files")
	server := fmServerTag.Render(m.server)
	gap := innerW - lipgloss.Width(brand) - lipgloss.Width(server) - 2
	if gap < 1 {
		gap = 1
	}
	line := " " + brand + strings.Repeat(" ", gap) + server + " "
	return fmHeaderBar.Width(innerW).Render(line)
}

func (m filesModel) renderPane(side, paneWidth, rows int) string {
	pane := m.paneRefConst(side)
	cw := paneWidth - 2 // content width inside the rounded border
	focused := side == m.focus

	label := "LOCAL"
	if side == 1 {
		label = "REMOTE"
	}
	titleStyle := lipgloss.NewStyle().Foreground(fmMutedC).Bold(true)
	if focused {
		titleStyle = titleStyle.Foreground(fmAccent)
	}
	title := titleStyle.Render(" " + label + " ")
	if !pane.loading {
		title += fmCount.Render(fmt.Sprintf("  (%d)", countReal(pane.entries)))
	}

	var b strings.Builder
	b.WriteString(title + "\n")
	b.WriteString(fmPathSty.Render(truncate(pane.cwd, cw)) + "\n")
	b.WriteString(fmRule.Render(strings.Repeat("─", cw)) + "\n")

	switch {
	case pane.loading:
		b.WriteString(fmStatusSty.Render("loading…"))
	case pane.err != nil:
		b.WriteString(fmErrSty.Render("error: " + truncate(pane.err.Error(), cw-7)))
	case len(pane.entries) == 0:
		b.WriteString(fmStatusSty.Render("(empty)"))
	default:
		end := pane.scroll + rows
		if end > len(pane.entries) {
			end = len(pane.entries)
		}
		lines := make([]string, 0, rows)
		for i := pane.scroll; i < end; i++ {
			selected := i == pane.index && focused
			hovered := side == m.hoverSide && i == m.hoverIndex
			// zone.Mark anchors this row's clickable region to its content, so
			// hit detection survives scrolling, resizing, and border offsets.
			lines = append(lines, zone.Mark(rowZoneID(side, i), renderRow(pane.entries[i], selected, hovered, cw)))
		}
		b.WriteString(strings.Join(lines, "\n"))
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(fmBorderC).
		Width(cw).
		Height(rows + 3) // title + path + rule + rows
	if focused {
		box = box.BorderForeground(fmAccent)
	}
	return zone.Mark(paneZoneID(side), box.Render(b.String()))
}

func renderRow(item fileItem, selected, hovered bool, cw int) string {
	sizeW := 9
	nameW := cw - sizeW - 5 // leading space + glyph + space + gap + trailing space
	if nameW < 4 {
		nameW = 4
	}
	glyph := "·"
	if item.isDir {
		glyph = "▸"
	}
	size := ""
	if !item.isDir {
		size = humanSize(item.size)
	}
	plain := fmt.Sprintf(" %s %-*s %*s ", glyph, nameW, truncate(item.name, nameW), sizeW, size)
	switch {
	case selected:
		return fmSelRow.Render(plain)
	case hovered:
		style := fmHoverRow
		if item.isDir {
			style = style.Foreground(fmDirC)
		}
		return style.Render(plain)
	case item.isDir:
		return fmDirRow.Render(plain)
	default:
		return fmFileRow.Render(plain)
	}
}

func (m filesModel) renderTransfers(innerW int) string {
	if len(m.transfers) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Foreground(fmText).Bold(true).Render(" Transfers"))
	b.WriteString("\n")
	start := 0
	if len(m.transfers) > 4 {
		start = len(m.transfers) - 4
	}
	barW := 22
	for _, t := range m.transfers[start:] {
		pct := 0
		if t.total > 0 {
			pct = int(t.bytesDone * 100 / t.total)
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
		label := lipgloss.NewStyle().Foreground(fmText).Render(fmt.Sprintf(" %-16s", truncate(t.label, 16)))
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

func footerHints() string {
	// key, label, action ("" = hint only, not clickable)
	hints := [][3]string{
		{"tab", "switch", "switch"}, {"↵", "open", "open"}, {"u", "upload", "upload"},
		{"d", "download", "download"}, {"drag", "move", ""}, {"r", "refresh", "refresh"}, {"q", "quit", "quit"},
	}
	parts := make([]string, 0, len(hints))
	for _, h := range hints {
		chip := fmKeyChip.Render(h[0]) + " " + fmHintLabel.Render(h[1])
		if h[2] != "" {
			chip = zone.Mark(fmActPrefix+h[2], chip) // clickable footer button
		}
		parts = append(parts, chip)
	}
	return " " + strings.Join(parts, "   ")
}

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
