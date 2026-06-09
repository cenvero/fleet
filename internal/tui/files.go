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
	"time"

	"github.com/cenvero/fleet/internal/core"
	tea "github.com/charmbracelet/bubbletea"
	zone "github.com/lrstanley/bubblezone"
)

// RunFiles launches the desktop-grade dual-pane file manager. Each pane has a
// "source": the local filesystem ("") or a managed server name. With no
// arguments the left pane is Local and the right is the first available server.
// With arguments the panes are bound to those sources in order, enabling
// local↔server and server↔server browsing and transfers.
//
//	fleet files            -> Local | <first server>
//	fleet files a          -> Local | <a>
//	fleet files a b        -> <a>   | <b>
func RunFiles(configDir string, servers ...string) error {
	app, err := core.Open(configDir)
	if err != nil {
		return err
	}
	defer app.Close()

	// bubblezone is the content-anchored way to track mouse zones in Bubble Tea:
	// it records every zone.Mark during zone.Scan (in View) and answers InBounds
	// queries on the next mouse event, surviving scroll/resize/border offsets.
	zone.NewGlobal()

	available, _ := app.ListServers()

	leftSrc, rightSrc := resolveSources(servers, available)
	left := newPaneSource(app, leftSrc)
	right := newPaneSource(app, rightSrc)

	m := filesModel{
		app:        app,
		servers:    available,
		width:      120,
		height:     36,
		left:       left,
		right:      right,
		focus:      0,
		chans:      make(map[int]*transferChans),
		hoverSide:  -1,
		hoverIndex: -1,
		showHidden: false,
	}
	_, err = tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseAllMotion()).Run()
	return err
}

// resolveSources picks the left/right pane sources ("" = Local) so the default
// is always useful and never duplicates a source:
//
//	fleet files          -> Local | <first server>   (or Local | Local if none)
//	fleet files a         -> Local | a                 (one server, Local on the left)
//	fleet files a b       -> a     | b
func resolveSources(args []string, available []core.ServerRecord) (left, right string) {
	switch len(args) {
	case 0:
		if len(available) > 0 {
			return "", available[0].Name
		}
		return "", ""
	case 1:
		return "", args[0]
	default:
		return args[0], args[1]
	}
}

func newPaneSource(app *core.App, source string) paneState {
	remote := source != ""
	cwd := "/"
	if remote {
		cwd = "/"
		if d, err := app.FileTransferDefaultsFor(source); err == nil && d.RemoteDir != "" {
			cwd = d.RemoteDir
		}
	} else {
		if wd, err := os.Getwd(); err == nil && wd != "" {
			cwd = wd
		}
	}
	return paneState{source: source, remote: remote, cwd: cwd, loading: true, selected: map[int]bool{}}
}

// ---- core types ----

type fileItem struct {
	name    string
	isDir   bool
	size    int64
	mode    uint32
	modTime time.Time
	symlink bool
}

type paneState struct {
	source   string // "" = local, else server name
	remote   bool
	cwd      string
	entries  []fileItem
	index    int
	scroll   int
	loading  bool
	err      error
	selected map[int]bool // multi-selection (excludes "..")
}

// label is the human source name for headers/menus.
func (p paneState) label() string {
	if p.source == "" {
		return "Local"
	}
	return p.source
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

// dragState tracks an in-progress mouse drag of one or more items.
type dragState struct {
	fromSide int
	items    []fileItem
	primary  fileItem // the row the drag started on
	active   bool     // crossed the press threshold into a real drag
	// snap animation target (set on release)
	snapping  bool
	snapUntil time.Time
	snapX     int
	snapY     int
}

// overlayKind enumerates the modal/popup overlays. Only one is active at a time.
type overlayKind int

const (
	overlayNone overlayKind = iota
	overlaySourcePicker
	overlayContextMenu
	overlayCopyMove // Finder-style "Copy here · Move here · Cancel"
	overlayConfirm  // generic confirmation (delete, dir transfer)
	overlayPrompt   // text input (new folder / rename)
	overlayProperties
)

// confirmKind distinguishes what a confirm overlay will do on Enter.
type confirmKind int

const (
	confirmDelete confirmKind = iota
	confirmDirTransfer
)

// dirTransferKind selects which recursive op a confirmed dir transfer runs.
type dirTransferKind int

const (
	dtCopy dirTransferKind = iota
	dtMove
)

// pendingDirTransfer holds a directory copy/move awaiting confirmation, with a
// live estimate of its size that fills in asynchronously.
type pendingDirTransfer struct {
	kind     dirTransferKind
	fromSide int
	item     fileItem
	files    int
	bytes    int64
	scanned  bool
	scanErr  error
}

// promptKind selects what a text-prompt overlay does on submit.
type promptKind int

const (
	promptNewFolder promptKind = iota
	promptRename
)

type filesModel struct {
	app           *core.App
	servers       []core.ServerRecord
	width, height int
	left, right   paneState
	focus         int // 0 = left, 1 = right
	status        string
	transfers     []*transferRow
	nextID        int
	chans         map[int]*transferChans
	showHidden    bool

	// mouse / drag
	drag       *dragState
	mouseX     int
	mouseY     int
	hoverSide  int // pane index hovered, -1 = none
	hoverIndex int // row index hovered, -1 = none

	// overlay state
	overlay overlayKind

	// source picker
	pickerSide  int
	pickerIndex int
	pickerItems []string

	// context menu
	menuItems []contextMenuItem
	menuIndex int
	menuX     int
	menuY     int
	menuSide  int
	menuRow   int

	// copy/move menu
	cmIndex  int
	cmX      int
	cmY      int
	cmDrag   *dragState
	cmTarget int // destination row index (-1 = pane cwd)

	// confirm modal
	confirm        confirmKind
	confirmText    string
	pendingDir     *pendingDirTransfer
	pendingDirDest string
	pendingDirTo   int
	dirScanCmd     tea.Cmd
	deleteSide     int
	deleteItems    []fileItem

	// prompt modal
	prompt      promptKind
	promptLabel string
	promptValue string
	promptSide  int
	promptItem  fileItem

	// properties modal
	propsText string
}

// ---- messages ----

type paneLoadedMsg struct {
	side   int
	source string
	cwd    string
	items  []fileItem
	err    error
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

// dirScanMsg carries the asynchronous size estimate for a pending dir transfer.
type dirScanMsg struct {
	files int
	bytes int64
	err   error
}

// snapTickMsg ends the drop "snap" animation.
type snapTickMsg struct{}

func (m filesModel) Init() tea.Cmd {
	return tea.Batch(
		m.loadCmd(0, m.left.source, m.left.cwd),
		m.loadCmd(1, m.right.source, m.right.cwd),
	)
}

// loadCmd builds the right loader for a pane's source (local vs remote).
func (m filesModel) loadCmd(side int, source, cwd string) tea.Cmd {
	app := m.app
	showHidden := m.showHidden
	if source == "" {
		return loadLocalCmd(side, cwd, showHidden)
	}
	return loadRemoteCmd(side, app, source, cwd, showHidden)
}

func loadLocalCmd(side int, cwd string, showHidden bool) tea.Cmd {
	return func() tea.Msg {
		entries, err := os.ReadDir(cwd)
		if err != nil {
			return paneLoadedMsg{side: side, source: "", cwd: cwd, err: err}
		}
		items := make([]fileItem, 0, len(entries))
		for _, e := range entries {
			name := e.Name()
			if !showHidden && strings.HasPrefix(name, ".") {
				continue
			}
			fi := fileItem{name: name, isDir: e.IsDir()}
			if info, err := e.Info(); err == nil {
				fi.size = info.Size()
				fi.mode = uint32(info.Mode())
				fi.modTime = info.ModTime()
				fi.symlink = info.Mode()&os.ModeSymlink != 0
			}
			items = append(items, fi)
		}
		return paneLoadedMsg{side: side, source: "", cwd: cwd, items: withParent(cwd, items, false)}
	}
}

func loadRemoteCmd(side int, app *core.App, server, cwd string, showHidden bool) tea.Cmd {
	return func() tea.Msg {
		result, err := app.ListRemoteDirHidden(server, cwd, showHidden)
		if err != nil {
			return paneLoadedMsg{side: side, source: server, cwd: cwd, err: err}
		}
		resolved := result.Path
		if resolved == "" {
			resolved = cwd
		}
		items := make([]fileItem, 0, len(result.Entries))
		for _, e := range result.Entries {
			items = append(items, fileItem{
				name: e.Name, isDir: e.IsDir, size: e.Size,
				mode: e.Mode, modTime: e.ModTime, symlink: e.IsSymlink,
			})
		}
		return paneLoadedMsg{side: side, source: server, cwd: resolved, items: withParent(resolved, items, true)}
	}
}

// withParent sorts (dirs first, case-insensitive) and prepends ".." unless at
// the filesystem root.
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
		m.clampScroll(0)
		m.clampScroll(1)
		return m, nil

	case paneLoadedMsg:
		pane := m.paneRef(msg.side)
		// Ignore a stale load for a source the pane has since switched away from.
		if pane.source != msg.source {
			return m, nil
		}
		pane.cwd = msg.cwd
		pane.entries = msg.items
		pane.err = msg.err
		pane.loading = false
		pane.selected = map[int]bool{}
		if pane.index >= len(pane.entries) {
			pane.index = 0
		}
		m.clampScroll(msg.side)
		return m, nil

	case progressTickMsg:
		m.applyProgress(msg.id, msg.u)
		if c, ok := m.chans[msg.id]; ok {
			return m, pollTransferCmd(msg.id, c)
		}
		return m, nil

	case transferDoneMsg:
		return m.handleTransferDone(msg)

	case dirScanMsg:
		if m.pendingDir != nil {
			m.pendingDir.scanned = true
			m.pendingDir.files = msg.files
			m.pendingDir.bytes = msg.bytes
			m.pendingDir.scanErr = msg.err
			m.confirmText = m.dirConfirmText()
		}
		return m, nil

	case snapTickMsg:
		if m.drag != nil && m.drag.snapping {
			m.drag = nil
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)
	}
	return m, nil
}

func (m filesModel) handleTransferDone(msg transferDoneMsg) (tea.Model, tea.Cmd) {
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
	// Refresh both panes so new/removed files show up immediately.
	return m, m.refreshBoth()
}

func (m filesModel) refreshBoth() tea.Cmd {
	return tea.Batch(
		m.loadCmd(0, m.left.source, m.left.cwd),
		m.loadCmd(1, m.right.source, m.right.cwd),
	)
}

// ---- keyboard ----

func (m filesModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Overlays capture keys first.
	if m.overlay != overlayNone {
		return m.handleOverlayKey(msg)
	}

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
	case "pgup":
		m.movePane(m.focus, -m.visibleRows())
		return m, nil
	case "pgdown":
		m.movePane(m.focus, m.visibleRows())
		return m, nil
	case "home":
		m.movePane(m.focus, -1<<30)
		return m, nil
	case "end":
		m.movePane(m.focus, 1<<30)
		return m, nil
	case "left", "h", "backspace":
		return m.enterParent(m.focus)
	case "right", "l", "enter":
		return m.activate(m.focus)
	case " ":
		m.toggleSelect(m.focus)
		return m, nil
	case "g":
		return m, m.reload(m.focus)
	case "ctrl+r":
		m.left.loading, m.right.loading = true, true
		return m, m.refreshBoth()
	case ".":
		m.showHidden = !m.showHidden
		m.left.loading, m.right.loading = true, true
		m.status = fmt.Sprintf("hidden files %s", onOff(m.showHidden))
		return m, m.refreshBoth()
	case "s":
		return m.openSourcePicker(m.focus), nil
	case "n":
		return m.openNewFolderPrompt(m.focus), nil
	case "r":
		return m.openRenamePrompt(m.focus), nil
	case "d", "delete":
		return m.openDeleteConfirm(m.focus), nil
	case "c":
		return m.copyToOtherPane(m.focus)
	case "m":
		return m.moveToOtherPane(m.focus)
	case "i":
		return m.openProperties(m.focus)
	case "u":
		// explicit upload: only meaningful local -> remote
		return m.explicitTransfer(m.focus)
	}
	return m, nil
}

// ---- navigation ----

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
	m.clampScroll(side)
}

func (m *filesModel) clampScroll(side int) {
	pane := m.paneRef(side)
	visible := m.visibleRows()
	if pane.index < pane.scroll {
		pane.scroll = pane.index
	}
	if pane.index >= pane.scroll+visible {
		pane.scroll = pane.index - visible + 1
	}
	if pane.scroll < 0 {
		pane.scroll = 0
	}
	maxScroll := len(pane.entries) - visible
	if maxScroll < 0 {
		maxScroll = 0
	}
	if pane.scroll > maxScroll {
		pane.scroll = maxScroll
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
	pane.selected = map[int]bool{}
	return m, m.loadCmd(side, pane.source, parent)
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
		pane.selected = map[int]bool{}
		return m, m.loadCmd(side, pane.source, next)
	}
	// A file: show its properties (open == inspect; transfers are explicit).
	return m.openProperties(side)
}

func (m filesModel) reload(side int) tea.Cmd {
	pane := m.paneRef(side)
	pane.loading = true
	return m.loadCmd(side, pane.source, pane.cwd)
}

// ---- selection ----

func (m *filesModel) toggleSelect(side int) {
	pane := m.paneRef(side)
	if pane.index < 0 || pane.index >= len(pane.entries) {
		return
	}
	if pane.entries[pane.index].name == ".." {
		return
	}
	if pane.selected == nil {
		pane.selected = map[int]bool{}
	}
	if pane.selected[pane.index] {
		delete(pane.selected, pane.index)
	} else {
		pane.selected[pane.index] = true
	}
	m.movePane(side, 1) // advance like a real file manager
}

// selectionItems returns the items the user is acting on: the multi-selection if
// any, else the focused row. "" entries are excluded.
func (m filesModel) selectionItems(side int) []fileItem {
	pane := m.paneRefConst(side)
	if len(pane.selected) > 0 {
		idxs := make([]int, 0, len(pane.selected))
		for i := range pane.selected {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		out := make([]fileItem, 0, len(idxs))
		for _, i := range idxs {
			if i >= 0 && i < len(pane.entries) && pane.entries[i].name != ".." {
				out = append(out, pane.entries[i])
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	it := m.focusedItem(side)
	if it.name == "" || it.name == ".." {
		return nil
	}
	return []fileItem{it}
}

// ---- transfer plumbing ----

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

func (m filesModel) paneRefConst(side int) paneState {
	if side == 1 {
		return m.right
	}
	return m.left
}

func (m filesModel) focusedItem(side int) fileItem {
	pane := m.paneRefConst(side)
	if pane.index < 0 || pane.index >= len(pane.entries) {
		return fileItem{}
	}
	return pane.entries[pane.index]
}

func (m filesModel) other(side int) int { return 1 - side }

// visibleRows is the number of file rows that fit in a pane body.
func (m filesModel) visibleRows() int {
	// header(1) + toolbar(1) + blank(1) + pane chrome (border2 + breadcrumb +
	// rule = 4) + status(1) + transfers dock(varies ~6) + footer(1) + padding(2).
	h := m.height - 16
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

func onOff(b bool) string {
	if b {
		return "shown"
	}
	return "hidden"
}

// ---- zone ids ----

func rowZoneID(side, i int) string { return fmt.Sprintf("fm-row-%d-%d", side, i) }
func paneZoneID(side int) string   { return fmt.Sprintf("fm-pane-%d", side) }
func headerZoneID(side int) string { return fmt.Sprintf("fm-head-%d", side) }

const fmActPrefix = "fm-act-"
const fmMenuPrefix = "fm-menu-"
const fmPickPrefix = "fm-pick-"
const fmCMPrefix = "fm-cm-"
