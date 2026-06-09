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

	// Reject unknown server names up front so we never launch the TUI against a
	// source that doesn't exist. Local ("") is always valid.
	if err := validateServerArgs(servers, available); err != nil {
		return err
	}

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

// validateServerArgs verifies every non-Local server argument names a server
// that actually exists (case-sensitive). It returns a clear error listing the
// known servers when an argument is unknown, so the caller can abort before
// launching the Bubble Tea program. Local ("") is always valid.
func validateServerArgs(args []string, available []core.ServerRecord) error {
	known := make(map[string]bool, len(available))
	for _, s := range available {
		known[s.Name] = true
	}
	for _, a := range args {
		if a == "" {
			continue // Local pane
		}
		if !known[a] {
			names := make([]string, 0, len(available))
			for _, s := range available {
				names = append(names, s.Name)
			}
			return fmt.Errorf("unknown server %q; known servers: %s", a, strings.Join(names, ", "))
		}
	}
	return nil
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

// viewMode selects how a pane lays out its directory, Finder-style.
type viewMode int

const (
	viewList viewMode = iota // one full-width item per row (default)
	viewGrid                 // multi-column grid of icon + name cells
)

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
	entries  []fileItem // the visible (filtered + sorted) listing
	allItems []fileItem // the full directory listing before filtering
	index    int
	scroll   int
	loading  bool
	err      error
	selected map[int]bool // multi-selection (excludes "..")
	view     viewMode     // list (default) or grid/icons

	// sort + filter
	sortBy   sortKey
	sortDesc bool
	filter   string // case-insensitive name substring; "" = no filter
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
	overlayPrompt   // text input (new folder / rename / new file)
	overlayProperties
	overlayEditor   // full-screen file viewer/editor with syntax highlighting
	overlayFilter   // per-pane name filter input
	overlayCompress // archive: pick a format + name, then compress the selection
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
	promptNewFile
	promptChmod
)

// sortKey selects how a pane's entries are ordered (dirs always come first).
type sortKey int

const (
	sortName sortKey = iota
	sortSize
	sortModified
)

func (s sortKey) label() string {
	switch s {
	case sortSize:
		return "Size"
	case sortModified:
		return "Modified"
	default:
		return "Name"
	}
}

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

	// compress (archive) overlay
	compressSide    int
	compressNames   []string // base names being archived
	compressFormat  int      // index into core.ArchiveFormats()
	compressName    string   // editable archive base name
	compressEditing bool     // true once the user edits the name field

	// properties modal
	propsText string

	// editor overlay
	editor editorState

	// filter input
	filterSide int
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

// fileOpDoneMsg carries the result of an asynchronous file operation (compress,
// extract, duplicate) so the pane can refresh and report status off the UI loop.
type fileOpDoneMsg struct {
	side int
	verb string // human label for the status line ("compressed", "extracted", …)
	what string // the affected name
	err  error
}

// checksumDoneMsg carries an asynchronous SHA-256 computation result. The hash is
// surfaced in the properties overlay (copyable) and the status line.
type checksumDoneMsg struct {
	side int
	name string
	path string
	sum  string
	err  error
}

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
		return paneLoadedMsg{side: side, source: "", cwd: cwd, items: items}
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
		return paneLoadedMsg{side: side, source: server, cwd: resolved, items: items}
	}
}

// reapplyPane rebuilds a pane's visible `entries` from its raw `allItems` by
// applying the current sort key/direction (dirs always first) and the
// case-insensitive name filter, then prepending ".." unless at the filesystem
// root. It is called after a load and whenever the sort or filter changes so the
// listing stays consistent without re-fetching from disk/network.
func (m *filesModel) reapplyPane(side int) {
	pane := m.paneRef(side)

	filtered := make([]fileItem, 0, len(pane.allItems))
	needle := strings.ToLower(strings.TrimSpace(pane.filter))
	for _, it := range pane.allItems {
		if needle != "" && !strings.Contains(strings.ToLower(it.name), needle) {
			continue
		}
		filtered = append(filtered, it)
	}

	sortItems(filtered, pane.sortBy, pane.sortDesc)

	atRoot := pane.cwd == "/" || (!pane.remote && filepath.Dir(pane.cwd) == pane.cwd)
	if atRoot {
		pane.entries = filtered
	} else {
		pane.entries = append([]fileItem{{name: "..", isDir: true}}, filtered...)
	}

	if pane.index >= len(pane.entries) {
		pane.index = len(pane.entries) - 1
	}
	if pane.index < 0 {
		pane.index = 0
	}
}

// sortItems orders items in place: directories always come first, then by the
// chosen key. Name is the stable tiebreaker so the order is deterministic.
func sortItems(items []fileItem, key sortKey, desc bool) {
	less := func(a, b fileItem) bool {
		switch key {
		case sortSize:
			if a.size != b.size {
				return a.size < b.size
			}
		case sortModified:
			if !a.modTime.Equal(b.modTime) {
				return a.modTime.Before(b.modTime)
			}
		}
		return strings.ToLower(a.name) < strings.ToLower(b.name)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].isDir != items[j].isDir {
			return items[i].isDir // dirs first regardless of direction
		}
		if desc {
			return less(items[j], items[i])
		}
		return less(items[i], items[j])
	})
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
		pane.allItems = msg.items
		pane.err = msg.err
		pane.loading = false
		pane.selected = map[int]bool{}
		// A fresh listing replaces the filter (Esc-clear semantics on navigation).
		pane.filter = ""
		m.reapplyPane(msg.side)
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

	case editorLoadedMsg:
		return m.onEditorLoaded(msg)

	case editorSavedMsg:
		return m.onEditorSaved(msg)

	case fileOpDoneMsg:
		return m.onFileOpDone(msg)

	case checksumDoneMsg:
		return m.onChecksumDone(msg)

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
		m.movePane(m.focus, -m.vStep(m.focus))
		return m, nil
	case "down", "j":
		m.movePane(m.focus, m.vStep(m.focus))
		return m, nil
	case "pgup":
		m.movePane(m.focus, -m.pageStep(m.focus))
		return m, nil
	case "pgdown":
		m.movePane(m.focus, m.pageStep(m.focus))
		return m, nil
	case "home":
		m.movePane(m.focus, -1<<30)
		return m, nil
	case "end":
		m.movePane(m.focus, 1<<30)
		return m, nil
	case "left":
		// In grid view a bare ← moves the selection one cell left; in list view
		// it navigates to the parent directory (classic behaviour).
		if m.paneRefConst(m.focus).view == viewGrid {
			m.movePane(m.focus, -1)
			return m, nil
		}
		return m.enterParent(m.focus)
	case "h", "backspace":
		return m.enterParent(m.focus)
	case "right":
		if m.paneRefConst(m.focus).view == viewGrid {
			m.movePane(m.focus, 1)
			return m, nil
		}
		return m.activate(m.focus)
	case "l", "enter":
		return m.activate(m.focus)
	case " ":
		m.toggleSelect(m.focus)
		return m, nil
	case "v":
		m.cycleView(m.focus)
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
	case "N":
		return m.openNewFilePrompt(m.focus), nil
	case "e":
		return m.openEditor(m.focus)
	case "/":
		return m.openFilter(m.focus), nil
	case "o":
		return m.cycleSort(m.focus), nil
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
	case "z":
		return m.openCompress(m.focus), nil
	case "x":
		return m.extractFocused(m.focus)
	case "p":
		return m.openChmodPrompt(m.focus), nil
	case "#":
		return m.checksumFocused(m.focus)
	case "D":
		return m.duplicateFocused(m.focus)
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
	if pane.view == viewGrid {
		m.clampScrollGrid(side)
		return
	}
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

// clampScrollGrid keeps pane.scroll aligned to grid-row boundaries (a multiple of
// the column count) so cells never tear across a partial scroll, and ensures the
// focused item's cell-row stays visible.
func (m *filesModel) clampScrollGrid(side int) {
	pane := m.paneRef(side)
	cols, visRows := m.gridDims()
	n := len(pane.entries)
	if n == 0 {
		pane.scroll = 0
		return
	}
	idxRow := pane.index / cols
	topRow := pane.scroll / cols
	if idxRow < topRow {
		topRow = idxRow
	}
	if idxRow >= topRow+visRows {
		topRow = idxRow - visRows + 1
	}
	if topRow < 0 {
		topRow = 0
	}
	totalRows := (n + cols - 1) / cols
	maxTopRow := totalRows - visRows
	if maxTopRow < 0 {
		maxTopRow = 0
	}
	if topRow > maxTopRow {
		topRow = maxTopRow
	}
	pane.scroll = topRow * cols
}

// vStep is the index delta for a single up/down keypress: one cell-row in grid
// (the column count) or one item in list view.
func (m filesModel) vStep(side int) int {
	if m.paneRefConst(side).view == viewGrid {
		cols, _ := m.gridDims()
		return cols
	}
	return 1
}

// pageStep is the index delta for PgUp/PgDn: a full page of visible items.
func (m filesModel) pageStep(side int) int {
	if m.paneRefConst(side).view == viewGrid {
		cols, visRows := m.gridDims()
		return cols * visRows
	}
	return m.visibleRows()
}

// cycleView toggles the focused pane's layout List → Grid → List and re-clamps
// the scroll for the new geometry.
func (m *filesModel) cycleView(side int) {
	pane := m.paneRef(side)
	if pane.view == viewList {
		pane.view = viewGrid
		m.status = pane.label() + " pane: Icon view"
	} else {
		pane.view = viewList
		m.status = pane.label() + " pane: List view"
	}
	m.clampScroll(side)
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

// visibleRows is the number of text lines available in a pane body. In list view
// this equals the number of file rows; in grid view it is divided among cell-rows.
func (m filesModel) visibleRows() int {
	// header(1) + toolbar(1) + blank(1) + pane chrome (border2 + breadcrumb +
	// rule = 4) + status(1) + transfers dock(varies ~6) + footer(1) + padding(2).
	h := m.height - 16
	if h < 4 {
		h = 4
	}
	return h
}

// ---- grid geometry ----

// gridCellW is the fixed display width of one icon/grid cell (including its
// inter-cell gutter). gridCellH is how many text lines a cell occupies
// (icon line + name line).
const (
	gridCellW = 16
	gridCellH = 2
)

// paneInnerWidth is the content width inside a pane's rounded border, matching
// renderPane's cw. Used so navigation can compute the grid column count exactly
// as the renderer does.
func (m filesModel) paneInnerWidth() int {
	innerW := m.width - 4
	if innerW < 48 {
		innerW = 48
	}
	sepW := 3
	paneWidth := (innerW - sepW) / 2
	if paneWidth < 28 {
		paneWidth = 28
	}
	return paneWidth - 2 // inside the rounded border
}

// gridCols is the number of columns that fit in a pane of content width cw.
func gridCols(cw int) int {
	cols := cw / gridCellW
	if cols < 1 {
		cols = 1
	}
	return cols
}

// gridDims returns (cols, visibleCellRows) for a pane: how many columns fit and
// how many rows-of-cells are visible given the available text lines.
func (m filesModel) gridDims() (cols, visRows int) {
	cols = gridCols(m.paneInnerWidth())
	visRows = m.visibleRows() / gridCellH
	if visRows < 1 {
		visRows = 1
	}
	return cols, visRows
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
const fmCompressPrefix = "fm-compress-"
