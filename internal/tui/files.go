// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"fmt"
	"os"
	"slices"
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

	// bubblezone tracks clickable regions inside popups (menus, dialogs). The
	// base frame is hit-tested arithmetically from the layout instead.
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
		frames:     &frameCache{},
		lastPath:   map[string]string{},
		bookmarks:  loadBookmarks(app.ConfigDir),
	}
	m.preview.cache = &previewCache{}
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
	style := core.NativePathStyle()
	root := style.DefaultRoot()
	cwd := style.DefaultRoot()
	if remote {
		style = core.TargetPathPOSIX
		root = style.DefaultRoot()
		cwd = style.DefaultRoot()
		if server, err := app.GetServer(source); err == nil {
			// Include the effective configured RemoteDir (server override or global
			// default) before deriving style and the initial browse location.
			if defaults, defaultsErr := app.FileTransferDefaultsFor(source); defaultsErr == nil {
				server.FileTransfer.RemoteDir = defaults.RemoteDir
			}
			style = core.TargetPathStyleForServer(server)
			root = core.RemotePathBoundary(server)
			cwd = core.InitialRemotePath(server)
		}
	} else if wd, err := os.Getwd(); err == nil && wd != "" {
		cwd = style.Clean(wd)
	}
	if _, err := style.Relative(root, cwd); err != nil {
		cwd = root
	}
	return paneState{
		source: source, remote: remote, pathStyle: style, root: root, cwd: cwd, home: cwd,
		loading: true, selected: map[int]bool{},
	}
}

// ---- core types ----

// viewMode selects how a pane lays out its directory, Finder-style.
type viewMode int

const (
	viewList viewMode = iota // one full-width item per row (default)
	viewGrid                 // multi-column grid of icon + name cells
)

type fileItem struct {
	name      string
	lowerName string
	isDir     bool
	size      int64
	mode      uint32
	modTime   time.Time
	symlink   bool
	linkDir   bool // a symlink whose target is a directory (navigable)
}

type paneState struct {
	source    string // "" = local, else server name
	remote    bool
	pathStyle core.TargetPathStyle
	root      string // highest browsable path (target-advertised file root)
	home      string // initial folder for this source ("~" in go-to)
	cwd       string
	listedCwd string     // folder the current entries belong to
	entries   []fileItem // the visible (filtered + sorted) listing
	allItems  []fileItem // the full directory listing before filtering
	index     int
	scroll    int
	loading   bool
	err       error
	selected  map[int]bool // multi-selection (excludes "..")
	view      viewMode     // list (default) or grid/icons
	free      int64        // free bytes on a local pane's filesystem (0 = unknown)

	// focusName is the entry to put the cursor on when the next listing
	// arrives (e.g. the folder we just came up out of).
	focusName string

	// range selection
	anchor    int
	anchorSet bool
	rangeBase map[int]bool
	visual    bool

	// sort + filter
	sortBy   sortKey
	sortDesc bool
	filter   string // case-insensitive name substring; "" = no filter

	// allItems is sorted in place; these remember for which key/slice so a
	// filter keystroke on a 50k-entry folder does not re-sort.
	sortedFor  *fileItem
	sortedLen  int
	sortedBy   sortKey
	sortedDesc bool

	// rev changes whenever anything this pane renders from changes (listing,
	// sort, filter, selection). The frame cache keys off it instead of hashing
	// the entry slice on every frame — see frameCache.
	rev uint64
}

// touch marks the pane's rendered content as changed, invalidating the frame
// cache. Call from every site that mutates entries/allItems/selected.
func (p *paneState) touch() { p.rev++ }

// label is the human source name for headers/menus.
func (p paneState) label() string {
	if p.source == "" {
		return "Local"
	}
	return p.source
}

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
	overlayConfirm  // generic confirmation (delete, transfer plan)
	overlayPrompt   // text input (new folder / rename / new file)
	overlayProperties
	overlayEditor   // full-screen file viewer/editor with syntax highlighting
	overlayFilter   // per-pane name filter input
	overlayCompress // archive: pick a format + name, then compress the selection
	overlayHelp     // keybinding reference
	overlayGoto     // go-to-path input with completion
	overlayJump     // fuzzy quick jump within the listing
	overlayPlaces   // bookmarks + recent folders
)

// confirmKind distinguishes what a confirm overlay will do on Enter.
type confirmKind int

const (
	confirmDelete confirmKind = iota
	confirmTransfer
)

// dirTransferKind selects which recursive op a confirmed dir transfer runs.
type dirTransferKind int

const (
	dtCopy dirTransferKind = iota
	dtMove
)

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

// frameCache memoises the last rendered frame. It is a pointer field on
// filesModel so the cache survives Bubble Tea's value-copy of the model between
// Update and View.
//
// This exists because the program runs with tea.WithMouseAllMotion: the
// terminal emits a motion event per cursor movement and Bubble Tea calls View
// after every message. Re-rendering only when something the frame depends on
// actually changed makes idle mouse movement free.
type frameCache struct {
	key   string
	frame string
	valid bool

	// selection statistics memo (see selectionStats)
	selSide  int
	selRev   uint64
	selN     int
	sel      selStats
	selValid bool
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
	frames        *frameCache

	// ver is bumped by Update for every message except pure mouse motion; it
	// is part of the frame-cache key.
	ver uint64

	// status severity + expiry
	statusLevel     fmLevel
	statusLevelText string
	statusSeen      string
	statusSeq       int

	// mouse / drag
	drag       *dragState
	mouseX     int
	mouseY     int
	hoverSide  int // pane index hovered, -1 = none
	hoverIndex int // row index hovered, -1 = none
	hoverTool  string

	// overlay state
	overlay overlayKind

	// source picker
	pickerSide  int
	pickerIndex int
	pickerItems []string

	// context menu (also used for the toolbar's "≡ More" menu)
	menuItems []contextMenuItem
	menuIndex int
	menuX     int
	menuY     int
	menuSide  int
	menuRow   int
	menuTitle string

	// copy/move menu
	cmIndex  int
	cmX      int
	cmY      int
	cmDrag   *dragState
	cmTarget int // destination row index (-1 = pane cwd)

	// confirm modal
	confirm     confirmKind
	confirmText string
	plan        *transferPlan
	deleteSide  int
	deleteItems []fileItem

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
	propsKey  string

	// editor overlay (a pointer: textarea.Model is ~12 KB, and Bubble Tea
	// boxes the whole model into an interface on every message)
	editor *editorState

	// filter input
	filterSide int

	// help
	helpScroll int

	// go to path
	gotoSide  int
	gotoValue string
	gotoErr   string
	gotoSugg  []string
	gotoIndex int

	// quick jump
	jumpSide    int
	jumpQuery   string
	jumpResults []int
	jumpIndex   int

	// places
	placesItems []fmPlace
	placesIndex int

	// navigation memory
	hist      [2]navHistory
	lastPath  map[string]string
	recent    []fmLoc
	bookmarks []fmLoc
	mirror    bool

	// preview pane
	preview previewState

	// transfer queue panel
	xferFocus  bool
	xferIndex  int
	refreshSeq int
	batchFrom  int // first transfer id of the current burst of work
}

// ---- messages ----

type paneLoadedMsg struct {
	side   int
	source string
	cwd    string // resolved folder
	req    string // folder that was requested (stale-load detection)
	items  []fileItem
	err    error
	free   int64
}

type progressTickMsg struct {
	id int
	u  core.ProgressUpdate
}

type transferDoneMsg struct {
	id    int
	label string
	err   error
	last  *core.ProgressUpdate
}

// dirScanMsg carries the asynchronous size estimate for a pending transfer.
type dirScanMsg struct {
	files int
	bytes int64
	err   error
	plan  *transferPlan
}

// snapTickMsg ends the drop "snap" animation.
type snapTickMsg struct{}

// statusExpireMsg clears a transient status message.
type statusExpireMsg struct{ seq int }

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
			return paneLoadedMsg{side: side, source: "", cwd: cwd, req: cwd, err: err}
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
			if fi.symlink {
				// Follow the link only to learn whether it can be entered; the
				// entry itself stays a symlink for every file operation.
				if st, err := os.Stat(joinPath(cwd, name, core.NativePathStyle())); err == nil && st.IsDir() {
					fi.linkDir = true
				}
			}
			items = append(items, fi)
		}
		free, _ := localDiskFree(cwd)
		return paneLoadedMsg{side: side, source: "", cwd: cwd, req: cwd, items: items, free: free}
	}
}

func loadRemoteCmd(side int, app *core.App, server, cwd string, showHidden bool) tea.Cmd {
	return func() tea.Msg {
		if app == nil {
			return paneLoadedMsg{side: side, source: server, cwd: cwd, req: cwd, err: fmt.Errorf("no controller")}
		}
		result, err := app.ListRemoteDirHidden(server, cwd, showHidden)
		if err != nil {
			return paneLoadedMsg{side: side, source: server, cwd: cwd, req: cwd, err: err}
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
		return paneLoadedMsg{side: side, source: server, cwd: resolved, req: cwd, items: items}
	}
}

// reapplyPane rebuilds a pane's visible `entries` from its raw `allItems` by
// applying the current sort key/direction (dirs always first) and the
// case-insensitive name filter, then prepending ".." unless at the filesystem
// root. It is called after a load and whenever the sort or filter changes so the
// listing stays consistent without re-fetching from disk/network.
//
// The cursor and the multi-selection follow their entries by name, so
// re-sorting never silently moves the selection onto different files.
func (m *filesModel) reapplyPane(side int) {
	pane := m.paneRef(side)
	pane.touch()

	// Remember what the cursor and selection point at before rebuilding.
	focusName := ""
	if pane.index >= 0 && pane.index < len(pane.entries) {
		focusName = pane.entries[pane.index].name
	}
	var selNames map[string]bool
	if len(pane.selected) > 0 {
		selNames = make(map[string]bool, len(pane.selected))
		for i := range pane.selected {
			if i >= 0 && i < len(pane.entries) {
				selNames[pane.entries[i].name] = true
			}
		}
	}

	// Ensure lowerName is populated for fast case-insensitive filter/sort
	for i := range pane.allItems {
		if pane.allItems[i].lowerName == "" && pane.allItems[i].name != "" {
			pane.allItems[i].lowerName = strings.ToLower(pane.allItems[i].name)
		}
	}

	// Sort allItems in place, but only when the key or the listing changed.
	var first *fileItem
	if len(pane.allItems) > 0 {
		first = &pane.allItems[0]
	}
	if first != pane.sortedFor || len(pane.allItems) != pane.sortedLen ||
		pane.sortedBy != pane.sortBy || pane.sortedDesc != pane.sortDesc {
		sortItems(pane.allItems, pane.sortBy, pane.sortDesc)
		pane.sortedFor, pane.sortedLen = first, len(pane.allItems)
		pane.sortedBy, pane.sortedDesc = pane.sortBy, pane.sortDesc
	}

	root := paneBrowseRoot(pane)
	atRoot := pane.pathStyle.Clean(pane.cwd) == pane.pathStyle.Clean(root)
	if pane.pathStyle.IsWindows() {
		atRoot = strings.EqualFold(pane.pathStyle.Clean(pane.cwd), pane.pathStyle.Clean(root))
	}

	needle := strings.ToLower(strings.TrimSpace(pane.filter))
	n := len(pane.allItems) + 1
	if needle != "" {
		n = 1
	}
	entries := make([]fileItem, 0, n)
	if !atRoot {
		entries = append(entries, fileItem{name: "..", lowerName: "..", isDir: true})
	}
	if needle == "" {
		entries = append(entries, pane.allItems...)
	} else {
		for i := range pane.allItems {
			if strings.Contains(pane.allItems[i].lowerName, needle) {
				entries = append(entries, pane.allItems[i])
			}
		}
	}
	pane.entries = entries

	if len(selNames) > 0 {
		sel := make(map[int]bool, len(selNames))
		for i := range pane.entries {
			if selNames[pane.entries[i].name] {
				sel[i] = true
			}
		}
		pane.selected = sel
	}
	if focusName != "" && (pane.index >= len(pane.entries) || pane.entries[pane.index].name != focusName) {
		for i := range pane.entries {
			if pane.entries[i].name == focusName {
				pane.index = i
				break
			}
		}
	}
	pane.anchorSet = pane.anchorSet && pane.visual

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
	lower := func(it *fileItem) string {
		if it.lowerName != "" {
			return it.lowerName
		}
		return strings.ToLower(it.name)
	}
	less := func(a, b *fileItem) int {
		switch key {
		case sortSize:
			if a.size != b.size {
				if a.size < b.size {
					return -1
				}
				return 1
			}
		case sortModified:
			if c := a.modTime.Compare(b.modTime); c != 0 {
				return c
			}
		}
		return strings.Compare(lower(a), lower(b))
	}
	slices.SortStableFunc(items, func(a, b fileItem) int {
		if a.isDir != b.isDir {
			if a.isDir {
				return -1 // dirs first regardless of direction
			}
			return 1
		}
		if desc {
			return less(&b, &a)
		}
		return less(&a, &b)
	})
}

// Update wraps the message handlers with the cross-cutting bookkeeping every
// state change needs: frame-cache versioning, status-message expiry and
// preview syncing.
func (m filesModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	isMotion := false
	if mm, ok := msg.(tea.MouseMsg); ok && mm.Action == tea.MouseActionMotion {
		isMotion = true
	}
	model, cmd := m.update(msg)
	fm, ok := model.(filesModel)
	if !ok {
		return model, cmd
	}
	if !isMotion {
		fm.ver++
	}
	var extra []tea.Cmd
	if fm.status != fm.statusSeen {
		fm.statusSeen = fm.status
		fm.statusSeq++
		if fm.status != "" {
			seq := fm.statusSeq
			ttl := 6 * time.Second
			switch fm.statusLevelOf() {
			case levelError:
				ttl = 20 * time.Second
			case levelWarn:
				ttl = 10 * time.Second
			}
			extra = append(extra, tea.Tick(ttl, func(time.Time) tea.Msg { return statusExpireMsg{seq: seq} }))
		}
	}
	if !isMotion && fm.preview.on {
		if c := fm.syncPreview(); c != nil {
			extra = append(extra, c)
		}
	}
	if len(extra) > 0 {
		cmd = tea.Batch(append([]tea.Cmd{cmd}, extra...)...)
	}
	return fm, cmd
}

// setStatus records a status message with an explicit severity.
func (m *filesModel) setStatus(level fmLevel, text string) {
	m.status = text
	m.statusLevel = level
	m.statusLevelText = text
}

func (m filesModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clampScroll(0)
		m.clampScroll(1)
		if m.overlay == overlayEditor && m.editor != nil && m.editor.mode == editorEdit {
			w, h := m.editorAreaSize()
			m.editor.area.SetWidth(w)
			m.editor.area.SetHeight(h)
		}
		return m, nil

	case paneLoadedMsg:
		return m.onPaneLoaded(msg)

	case progressTickMsg:
		m.applyProgress(msg.id, msg.u)
		if c, ok := m.chans[msg.id]; ok {
			return m, pollTransferCmd(msg.id, c)
		}
		return m, nil

	case transferDoneMsg:
		return m.handleTransferDone(msg)

	case refreshTickMsg:
		if msg.seq != m.refreshSeq {
			return m, nil
		}
		m.left.loading, m.right.loading = true, true
		return m, m.refreshBoth()

	case dirScanMsg:
		if m.plan != nil && (msg.plan == nil || msg.plan == m.plan) {
			m.plan.scanned = true
			m.plan.scanFiles = msg.files
			m.plan.scanBytes = msg.bytes
			m.plan.scanErr = msg.err
		}
		return m, nil

	case snapTickMsg:
		if m.drag != nil && m.drag.snapping {
			m.drag = nil
		}
		return m, nil

	case statusExpireMsg:
		if msg.seq == m.statusSeq && m.overlay == overlayNone {
			m.status = ""
			m.statusSeen = ""
		}
		return m, nil

	case previewDebounceMsg:
		return m.onPreviewDebounce(msg)

	case previewLoadedMsg:
		return m.onPreviewLoaded(msg)

	case editorLoadedMsg:
		return m.onEditorLoaded(msg)

	case editorSavedMsg:
		return m.onEditorSaved(msg)

	case fileOpDoneMsg:
		return m.onFileOpDone(msg)

	case batchOpDoneMsg:
		return m.onBatchOpDone(msg)

	case propsStatMsg:
		return m.onPropsStat(msg)

	case checksumDoneMsg:
		return m.onChecksumDone(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)
	}
	return m, nil
}

// onPaneLoaded installs a listing. A reload of the folder already on screen
// keeps the cursor, selection and filter; a new folder starts fresh (with the
// cursor on focusName when navigating up).
func (m filesModel) onPaneLoaded(msg paneLoadedMsg) (tea.Model, tea.Cmd) {
	pane := m.paneRef(msg.side)
	// Ignore a stale load for a source the pane has since switched away from,
	// or for a folder it has since navigated away from.
	if pane.source != msg.source {
		return m, nil
	}
	if msg.req != "" && pane.pathStyle.Clean(msg.req) != pane.pathStyle.Clean(pane.cwd) {
		return m, nil
	}
	refresh := pane.listedCwd != "" && pane.listedCwd == pane.pathStyle.Clean(msg.cwd) && msg.err == nil && pane.err == nil
	if msg.err == nil {
		pane.cwd = pane.pathStyle.Clean(msg.cwd)
	}
	pane.allItems = msg.items
	pane.sortedFor = nil
	pane.err = msg.err
	pane.loading = false
	pane.free = msg.free
	if msg.err != nil {
		pane.entries = nil
		pane.selected = map[int]bool{}
		pane.listedCwd = pane.cwd
		pane.index, pane.scroll = 0, 0
		pane.touch()
		return m, nil
	}
	if !refresh {
		pane.selected = map[int]bool{}
		// A fresh listing replaces the filter (Esc-clear semantics on navigation).
		pane.filter = ""
		pane.anchorSet, pane.visual = false, false
		pane.index, pane.scroll = 0, 0
	}
	pane.listedCwd = pane.cwd
	m.reapplyPane(msg.side)
	if pane.focusName != "" {
		for i, it := range pane.entries {
			if it.name == pane.focusName {
				pane.index = i
				break
			}
		}
		pane.focusName = ""
	}
	if pane.index >= len(pane.entries) {
		pane.index = 0
	}
	m.clampScroll(msg.side)
	if !refresh {
		m.rememberVisit(msg.side)
	}
	return m, nil
}

// ---- keyboard ----

func (m filesModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Overlays capture keys first.
	if m.overlay != overlayNone {
		return m.handleOverlayKey(msg)
	}
	if m.xferFocus {
		return m.handleTransferKey(msg)
	}

	key := msg.String()
	pane := m.paneRef(m.focus)
	// Range-select mode turns plain movement into range extension.
	if pane.visual {
		switch key {
		case "up", "k":
			m.extendRange(m.focus, -m.vStep(m.focus))
			return m, nil
		case "down", "j":
			m.extendRange(m.focus, m.vStep(m.focus))
			return m, nil
		case "pgup":
			m.extendRange(m.focus, -m.pageStep(m.focus))
			return m, nil
		case "pgdown":
			m.extendRange(m.focus, m.pageStep(m.focus))
			return m, nil
		case "home":
			m.extendRange(m.focus, -1<<30)
			return m, nil
		case "end":
			m.extendRange(m.focus, 1<<30)
			return m, nil
		case "esc", "V":
			m.toggleVisual(m.focus)
			return m, nil
		}
	}

	switch key {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab", "shift+tab":
		m.focus = 1 - m.focus
		return m, nil
	case "up", "k":
		pane.anchorSet = false
		m.movePane(m.focus, -m.vStep(m.focus))
		return m, nil
	case "down", "j":
		pane.anchorSet = false
		m.movePane(m.focus, m.vStep(m.focus))
		return m, nil
	case "shift+up", "K":
		m.extendRange(m.focus, -m.vStep(m.focus))
		return m, nil
	case "shift+down", "J":
		m.extendRange(m.focus, m.vStep(m.focus))
		return m, nil
	case "shift+home":
		m.extendRange(m.focus, -1<<30)
		return m, nil
	case "shift+end":
		m.extendRange(m.focus, 1<<30)
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
	case "ctrl+a":
		m.selectAll(m.focus)
		return m, nil
	case "*":
		m.invertSelection(m.focus)
		return m, nil
	case "V":
		m.toggleVisual(m.focus)
		return m, nil
	case "esc":
		if m.clearSelection(m.focus) {
			m.setStatus(levelInfo, "selection cleared")
			return m, nil
		}
		if pane.filter != "" {
			pane.filter = ""
			m.reapplyPane(m.focus)
			m.clampScroll(m.focus)
			m.setStatus(levelInfo, "filter cleared")
		}
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
	case "e", "f4":
		return m.openEditor(m.focus)
	case "/":
		return m.openFilter(m.focus), nil
	case "o":
		return m.cycleSort(m.focus), nil
	case "r":
		return m.openRenamePrompt(m.focus), nil
	case "d", "delete", "f8":
		return m.openDeleteConfirm(m.focus), nil
	case "c", "f5":
		return m.copyToOtherPane(m.focus)
	case "m", "f6":
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
	case "?", "f1":
		return m.openHelp(), nil
	case "P", "f3":
		return m.togglePreview()
	case ":", "ctrl+g":
		return m.openGoto(m.focus), nil
	case "f", "ctrl+p":
		return m.openJump(m.focus), nil
	case "'":
		return m.openPlaces(), nil
	case "b":
		return m.toggleBookmark(m.focus), nil
	case "~":
		return m.goHome(m.focus)
	case "=":
		m.mirror = !m.mirror
		if m.mirror {
			m.setStatus(levelInfo, "mirror navigation on — the other pane follows into same-named folders")
		} else {
			m.setStatus(levelInfo, "mirror navigation off")
		}
		return m, nil
	case "alt+left", "H":
		return m.historyBack(m.focus)
	case "alt+right", "L":
		return m.historyForward(m.focus)
	case "t":
		if len(m.transfers) == 0 {
			m.setStatus(levelInfo, "no transfers yet — c/m copies or moves the selection to the other pane")
			return m, nil
		}
		m.xferFocus = true
		m.xferIndex = 0
		return m, nil
	case "C":
		n := m.clearFinished()
		m.setStatus(levelInfo, fmt.Sprintf("cleared %d finished transfer(s)", n))
		return m, nil
	case "R":
		return m, m.retryFailed()
	case "f9":
		return m.openActionsMenu(m.focus, m.width/2-12, 3, m.toolbarLayout(m.layout().innerW).overflow, "Actions"), nil
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

func paneBrowseRoot(pane *paneState) string {
	if pane.root != "" {
		return pane.pathStyle.Clean(pane.root)
	}
	current := pane.pathStyle.Clean(pane.cwd)
	for !pane.pathStyle.IsRoot(current) {
		parent := pane.pathStyle.Dir(current)
		if parent == current {
			return pane.pathStyle.DefaultRoot()
		}
		current = parent
	}
	return current
}

func (m filesModel) enterParent(side int) (tea.Model, tea.Cmd) {
	pane := m.paneRef(side)
	root := paneBrowseRoot(pane)
	parent := parentDir(pane.cwd, pane.pathStyle)
	if _, err := pane.pathStyle.Relative(root, parent); err != nil {
		parent = pane.pathStyle.Clean(root)
	}
	if parent == pane.cwd {
		return m, nil
	}
	came := pane.pathStyle.Base(pane.cwd)
	cmd := m.navigate(side, parent, came, true)
	return m, tea.Batch(cmd, m.mirrorParent(side))
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
	if item.isDir || item.linkDir {
		next := joinPath(pane.cwd, item.name, pane.pathStyle)
		cmd := m.navigate(side, next, "", true)
		return m, tea.Batch(cmd, m.mirrorEnter(side, item.name))
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
	pane.anchorSet = false
	if pane.entries[pane.index].name == ".." {
		m.movePane(side, 1)
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
	pane.touch()
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
		slices.Sort(idxs)
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
	return m.layout().rows
}

// ---- grid geometry ----

// gridCellW is the fixed display width of one icon/grid cell (including its
// inter-cell gutter). gridCellH is how many text lines a cell occupies
// (icon line + name line).
const (
	gridCellW = 16
	gridCellH = 2
)

// paneInnerWidth is the content width inside a pane's border, matching the
// renderer. Both panes are always the same width (or only one is shown).
func (m filesModel) paneInnerWidth() int {
	l := m.layout()
	return l.contentW(m.focus)
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

func parentDir(cwd string, style core.TargetPathStyle) string {
	return style.Dir(cwd)
}

func joinPath(cwd, name string, style core.TargetPathStyle) string {
	return style.Join(cwd, name)
}

func onOff(b bool) string {
	if b {
		return "shown"
	}
	return "hidden"
}

// ---- zone ids (popups only) ----

const fmMenuPrefix = "fm-menu-"
const fmPickPrefix = "fm-pick-"
const fmCMPrefix = "fm-cm-"
const fmCompressPrefix = "fm-compress-"
const fmPlacePrefix = "fm-place-"
