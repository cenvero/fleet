// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/core"
	tea "github.com/charmbracelet/bubbletea"
	zone "github.com/lrstanley/bubblezone"
)

// dragThreshold and snapDuration tune the macOS-style drag feel.
const snapDuration = 140 * time.Millisecond

// ============================================================================
// Mouse handling
// ============================================================================

func (m filesModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	m.mouseX, m.mouseY = msg.X, msg.Y

	// Wheel scrolls whichever pane (or overlay) is under the cursor.
	switch msg.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		delta := 1
		if msg.Button == tea.MouseButtonWheelUp {
			delta = -1
		}
		switch m.overlay {
		case overlayNone:
			if side, _, ok := m.hitRow(msg); ok {
				m.movePane(side, delta*m.vStep(side))
			} else if h := m.hitTest(msg.X, msg.Y); h.kind == fmHitTransfer && len(m.transfers) > 0 {
				m.xferIndex += delta
				if m.xferIndex < 0 {
					m.xferIndex = 0
				}
				if m.xferIndex >= len(m.transfers) {
					m.xferIndex = len(m.transfers) - 1
				}
			}
		case overlayHelp:
			m.helpScroll += delta * 3
			if m.helpScroll < 0 {
				m.helpScroll = 0
			}
		case overlayEditor:
			if m.editor != nil && m.editor.mode == editorView {
				m.editor.viewScrl += delta * 3
				m.clampEditorScroll()
			}
		}
		return m, nil
	}

	// Overlays intercept all clicks while open.
	if m.overlay != overlayNone {
		return m.handleOverlayMouse(msg)
	}

	switch msg.Action {
	case tea.MouseActionMotion:
		return m.handleMouseMotion(msg)
	case tea.MouseActionPress:
		return m.handleMousePress(msg)
	case tea.MouseActionRelease:
		return m.handleMouseRelease(msg)
	}
	return m, nil
}

func (m filesModel) handleMouseMotion(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	m.mouseX, m.mouseY = msg.X, msg.Y
	h := m.hitTest(msg.X, msg.Y)
	// Update hover target (O(1): the layout maps y straight to a row).
	if h.kind == fmHitRow && h.index >= 0 {
		m.hoverSide, m.hoverIndex = h.side, h.index
	} else if m.drag != nil && m.drag.active && h.side >= 0 {
		m.hoverSide, m.hoverIndex = h.side, -1
	} else {
		m.hoverSide, m.hoverIndex = -1, -1
	}
	if h.kind == fmHitToolbar {
		m.hoverTool = h.action
	} else {
		m.hoverTool = ""
	}
	// Promote a press into an active drag once the mouse is held and moves.
	if m.drag != nil && msg.Button == tea.MouseButtonLeft {
		m.drag.active = true
	}
	return m, nil
}

func (m filesModel) handleMousePress(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	h := m.hitTest(msg.X, msg.Y)

	// Right-click opens the context menu on the row/pane under the cursor.
	if msg.Button == tea.MouseButtonRight {
		side, idx, ok := m.hitRow(msg)
		if !ok {
			return m, nil
		}
		m.focus = side
		m.xferFocus = false
		if idx >= 0 && idx < len(m.paneRefConst(side).entries) {
			m.paneRef(side).index = idx
		}
		return m.openContextMenu(side, idx, msg.X, msg.Y), nil
	}

	if msg.Button != tea.MouseButtonLeft {
		return m, nil
	}

	switch h.kind {
	case fmHitToolbar:
		if h.action == "more" {
			fit := m.toolbarLayout(m.layout().innerW)
			return m.openActionsMenu(m.focus, msg.X-10, msg.Y+1, fit.overflow, "More actions"), nil
		}
		return m.applyAction(h.action)
	case fmHitTitle:
		// Pane title click opens the source picker for that pane.
		m.focus = h.side
		m.xferFocus = false
		return m.openSourcePicker(h.side), nil
	case fmHitCrumb:
		m.focus = h.side
		m.xferFocus = false
		l := m.layout()
		if path := m.crumbPath(h.side, l.contentW(h.side), h.index); path != "" {
			pane := m.paneRefConst(h.side)
			if path != pane.cwd {
				focus := ""
				if rel, err := pane.pathStyle.Relative(path, pane.cwd); err == nil && rel != "." {
					focus = strings.SplitN(rel, "/", 2)[0]
				}
				return m, m.navigate(h.side, path, focus, true)
			}
		}
		return m, nil
	case fmHitColumn:
		m.focus = h.side
		m.xferFocus = false
		if h.action != "" {
			return m.setSortByColumn(h.side, h.action), nil
		}
		return m, nil
	case fmHitTransfer:
		if len(m.transfers) == 0 {
			return m, nil
		}
		m.xferFocus = true
		if h.index >= 0 {
			m.xferIndex = h.index
		}
		return m, nil
	}

	side, idx, ok := m.hitRow(msg)
	if !ok {
		return m, nil
	}
	m.focus = side
	m.xferFocus = false
	if idx >= 0 {
		pane := m.paneRef(side)
		// Modifier clicks select without starting a drag.
		switch {
		case msg.Ctrl || msg.Alt:
			pane.index = idx
			if pane.entries[idx].name != ".." {
				if pane.selected == nil {
					pane.selected = map[int]bool{}
				}
				if pane.selected[idx] {
					delete(pane.selected, idx)
				} else {
					pane.selected[idx] = true
				}
				pane.touch()
			}
			m.drag = nil
			return m, nil
		case msg.Shift:
			delta := idx - pane.index
			m.extendRange(side, delta)
			m.drag = nil
			return m, nil
		}
		pane.index = idx
		pane.anchorSet = false
		// Begin a potential drag from this row (or the multi-selection).
		items := m.selectionItems(side)
		if len(items) == 0 || pane.entries[idx].name == ".." {
			m.drag = nil
		} else {
			// If the clicked row isn't part of the selection, drag just it.
			if !pane.selected[idx] {
				items = []fileItem{pane.entries[idx]}
			}
			m.drag = &dragState{fromSide: side, items: items, primary: pane.entries[idx]}
		}
	}
	return m, nil
}

func (m filesModel) handleMouseRelease(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Button != tea.MouseButtonLeft && msg.Button != tea.MouseButtonNone {
		return m, nil
	}
	drag := m.drag
	m.drag = nil
	if drag == nil {
		return m, nil
	}

	relSide, relIdx, inPane := m.hitRow(msg)

	// A plain click (no real drag motion): single-click SELECTS, never acts.
	if !drag.active {
		// Detect a double-click: two clicks on the same row open it.
		if inPane && relSide == drag.fromSide && m.isDoubleClick(relSide, relIdx) {
			m.paneRef(relSide).index = relIdx
			lastClickIdx = -1
			return m.activate(relSide)
		}
		if inPane && relIdx >= 0 {
			m.paneRef(relSide).index = relIdx
			m.recordClick(relSide, relIdx)
		}
		return m, nil
	}

	// A real drag was released.
	if !inPane {
		return m, nil // dropped outside any pane: no-op
	}

	if relSide == drag.fromSide {
		// Same-pane drag onto a folder = immediate Move (rename).
		if relIdx >= 0 {
			pane := m.paneRefConst(relSide)
			if relIdx < len(pane.entries) {
				dst := pane.entries[relIdx]
				if dst.isDir && !sameItem(dst, drag.primary) {
					return m.sameDirMove(drag, dst)
				}
			}
		}
		return m, nil
	}

	// Cross-pane drop → Finder-style Copy/Move menu at the drop point.
	m.cmTarget = m.dropTargetIndex(relSide, relIdx)
	return m.openCopyMoveMenu(drag, msg.X, msg.Y), nil
}

// dropTargetIndex returns the destination row index if the drop landed on a
// directory row, else -1 to mean "into the destination pane's cwd".
func (m filesModel) dropTargetIndex(side, idx int) int {
	if idx < 0 {
		return -1
	}
	pane := m.paneRefConst(side)
	if idx >= len(pane.entries) {
		return -1
	}
	if pane.entries[idx].isDir && pane.entries[idx].name != ".." {
		return idx
	}
	return -1
}

// double-click tracking
var lastClickSide = -1
var lastClickIdx = -1
var lastClickAt time.Time

func (m filesModel) isDoubleClick(side, idx int) bool {
	return side == lastClickSide && idx == lastClickIdx && idx >= 0 &&
		time.Since(lastClickAt) < 400*time.Millisecond
}

func (m filesModel) recordClick(side, idx int) {
	lastClickSide, lastClickIdx, lastClickAt = side, idx, time.Now()
}

func sameItem(a, b fileItem) bool { return a.name == b.name && a.isDir == b.isDir }

func (m filesModel) applyAction(name string) (tea.Model, tea.Cmd) {
	switch name {
	case "source":
		return m.openSourcePicker(m.focus), nil
	case "edit":
		return m.openEditor(m.focus)
	case "newfolder":
		return m.openNewFolderPrompt(m.focus), nil
	case "newfile":
		return m.openNewFilePrompt(m.focus), nil
	case "filter":
		return m.openFilter(m.focus), nil
	case "sort":
		return m.cycleSort(m.focus), nil
	case "rename":
		return m.openRenamePrompt(m.focus), nil
	case "delete":
		return m.openDeleteConfirm(m.focus), nil
	case "copy":
		return m.copyToOtherPane(m.focus)
	case "move":
		return m.moveToOtherPane(m.focus)
	case "compress":
		return m.openCompress(m.focus), nil
	case "extract":
		return m.extractFocused(m.focus)
	case "chmod":
		return m.openChmodPrompt(m.focus), nil
	case "props":
		return m.openProperties(m.focus)
	case "checksum":
		return m.checksumFocused(m.focus)
	case "duplicate":
		return m.duplicateFocused(m.focus)
	case "view":
		m.cycleView(m.focus)
		return m, nil
	case "preview":
		return m.togglePreview()
	case "hidden":
		m.showHidden = !m.showHidden
		m.left.loading, m.right.loading = true, true
		m.status = "hidden files " + onOff(m.showHidden)
		return m, m.refreshBoth()
	case "refresh":
		m.left.loading, m.right.loading = true, true
		return m, m.refreshBoth()
	case "help":
		return m.openHelp(), nil
	case "goto":
		return m.openGoto(m.focus), nil
	case "jump":
		return m.openJump(m.focus), nil
	case "places":
		return m.openPlaces(), nil
	case "bookmark":
		return m.toggleBookmark(m.focus), nil
	case "transfers":
		if len(m.transfers) > 0 {
			m.xferFocus = true
		}
		return m, nil
	case "quit":
		return m, tea.Quit
	}
	return m, nil
}

// ============================================================================
// Source picker overlay
// ============================================================================

func (m filesModel) openSourcePicker(side int) filesModel {
	items := []string{"Local"}
	for _, s := range m.servers {
		items = append(items, s.Name)
	}
	m.overlay = overlaySourcePicker
	m.pickerSide = side
	m.pickerItems = items
	// Preselect the pane's current source.
	cur := m.paneRefConst(side).label()
	m.pickerIndex = 0
	for i, it := range items {
		if it == cur {
			m.pickerIndex = i
			break
		}
	}
	return m
}

func (m filesModel) chooseSource(idx int) (tea.Model, tea.Cmd) {
	if idx < 0 || idx >= len(m.pickerItems) {
		m.overlay = overlayNone
		return m, nil
	}
	choice := m.pickerItems[idx]
	source := ""
	if choice != "Local" {
		source = choice
	}
	side := m.pickerSide
	pane := m.paneRef(side)
	m.pushHistory(side, fmLoc{Source: pane.source, Path: pane.cwd})
	*pane = m.newPane(source, *pane)
	m.overlay = overlayNone
	m.focus = side
	m.status = "switched " + sideName(side) + " pane to " + choice
	return m, m.loadCmd(side, pane.source, pane.cwd)
}

// ============================================================================
// Context menu overlay
// ============================================================================

type contextMenuItem struct {
	key       string
	label     string
	action    string
	enabled   bool
	separator bool
}

func (m filesModel) openContextMenu(side, idx, x, y int) filesModel {
	pane := m.paneRefConst(side)
	onRow := idx >= 0 && idx < len(pane.entries) && pane.entries[idx].name != ".."
	isDir := onRow && pane.entries[idx].isDir
	canEdit := onRow && !isDir
	isArchive := onRow && !isDir && isArchiveName(pane.entries[idx].name)
	items := []contextMenuItem{
		{key: "↵", label: "Open", action: "open", enabled: onRow || (idx >= 0 && idx < len(pane.entries))},
		{key: "e", label: "Edit", action: "edit", enabled: canEdit},
		{key: "c", label: "Copy to other pane", action: "copy", enabled: onRow},
		{key: "m", label: "Move to other pane", action: "move", enabled: onRow},
		{key: "D", label: "Duplicate", action: "duplicate", enabled: onRow},
		{key: "r", label: "Rename", action: "rename", enabled: onRow},
		{key: "d", label: "Delete", action: "delete", enabled: onRow},
		{key: "z", label: "Compress…", action: "compress", enabled: onRow},
		{key: "x", label: "Extract", action: "extract", enabled: isArchive},
		{key: "p", label: "Permissions…", action: "chmod", enabled: onRow},
		{key: "#", label: "Checksum (SHA-256)", action: "checksum", enabled: onRow && !isDir},
		{key: "n", label: "New folder", action: "newfolder", enabled: true},
		{key: "N", label: "New file", action: "newfile", enabled: true},
		{key: "i", label: "Properties", action: "props", enabled: onRow},
		{key: "/", label: "Filter…", action: "filter", enabled: true},
		{key: "o", label: "Sort: " + m.paneRefConst(side).sortBy.label(), action: "sort", enabled: true},
		{key: "v", label: viewLabel(m.paneRefConst(side).view), action: "view", enabled: true},
		{key: "b", label: "Bookmark this folder", action: "bookmark", enabled: true},
		{key: "g", label: "Refresh", action: "refresh", enabled: true},
	}
	m.overlay = overlayContextMenu
	m.menuItems = items
	m.menuIndex = 0
	m.menuTitle = ""
	m.menuX, m.menuY = x, y
	m.menuSide, m.menuRow = side, idx
	return m
}

// openActionsMenu shows toolbar buttons that did not fit (the "≡ More" menu),
// plus the navigation actions that have no button.
func (m filesModel) openActionsMenu(side, x, y int, overflow []toolButton, title string) filesModel {
	var items []contextMenuItem
	for _, b := range overflow {
		items = append(items, contextMenuItem{key: b.key, label: b.label, action: b.action, enabled: true})
	}
	items = append(items,
		contextMenuItem{key: ":", label: "Go to path…", action: "goto", enabled: true},
		contextMenuItem{key: "f", label: "Jump to item…", action: "jump", enabled: true},
		contextMenuItem{key: "'", label: "Places…", action: "places", enabled: true},
		contextMenuItem{key: "t", label: "Transfers", action: "transfers", enabled: len(m.transfers) > 0},
		contextMenuItem{key: "?", label: "Help", action: "help", enabled: true},
	)
	m.overlay = overlayContextMenu
	m.menuItems = items
	m.menuIndex = 0
	m.menuTitle = title
	if x < 0 {
		x = 0
	}
	m.menuX, m.menuY = x, y
	m.menuSide, m.menuRow = side, -1
	return m
}

func (m filesModel) runContextAction(action string) (tea.Model, tea.Cmd) {
	side := m.menuSide
	m.overlay = overlayNone
	m.menuTitle = ""
	switch action {
	case "open":
		return m.activate(side)
	case "edit":
		return m.openEditor(side)
	case "copy":
		return m.copyToOtherPane(side)
	case "move":
		return m.moveToOtherPane(side)
	case "duplicate":
		return m.duplicateFocused(side)
	case "compress":
		return m.openCompress(side), nil
	case "extract":
		return m.extractFocused(side)
	case "chmod":
		return m.openChmodPrompt(side), nil
	case "checksum":
		return m.checksumFocused(side)
	case "rename":
		return m.openRenamePrompt(side), nil
	case "delete":
		return m.openDeleteConfirm(side), nil
	case "newfolder":
		return m.openNewFolderPrompt(side), nil
	case "newfile":
		return m.openNewFilePrompt(side), nil
	case "filter":
		return m.openFilter(side), nil
	case "sort":
		return m.cycleSort(side), nil
	case "props":
		return m.openProperties(side)
	case "view":
		m.cycleView(side)
		return m, nil
	case "refresh":
		return m, m.reload(side)
	case "bookmark":
		return m.toggleBookmark(side), nil
	}
	m.focus = side
	return m.applyAction(action)
}

// ============================================================================
// New folder / Rename prompt overlay
// ============================================================================

func (m filesModel) openNewFolderPrompt(side int) filesModel {
	m.overlay = overlayPrompt
	m.prompt = promptNewFolder
	m.promptSide = side
	m.promptLabel = "New folder in " + m.paneRefConst(side).label()
	m.promptValue = ""
	return m
}

func (m filesModel) openNewFilePrompt(side int) filesModel {
	m.overlay = overlayPrompt
	m.prompt = promptNewFile
	m.promptSide = side
	m.promptLabel = "New file in " + m.paneRefConst(side).label()
	m.promptValue = ""
	return m
}

func (m filesModel) openRenamePrompt(side int) filesModel {
	it := m.focusedItem(side)
	if it.name == "" || it.name == ".." {
		m.status = "select an item to rename"
		return m
	}
	m.overlay = overlayPrompt
	m.prompt = promptRename
	m.promptSide = side
	m.promptItem = it
	m.promptLabel = "Rename '" + it.name + "'"
	m.promptValue = it.name
	return m
}

// submitPrompt validates the typed name and runs the operation off the UI
// thread (remote mkdir/rename/upload are network round trips).
func (m filesModel) submitPrompt() (tea.Model, tea.Cmd) {
	side := m.promptSide
	name := strings.TrimSpace(m.promptValue)
	m.overlay = overlayNone
	if name == "" {
		m.status = "cancelled"
		return m, nil
	}
	pane := m.paneRefConst(side)
	if m.prompt != promptChmod {
		if err := core.ValidateTargetPathComponent(pane.pathStyle, name); err != nil {
			m.status = "invalid name: " + err.Error()
			return m, nil
		}
	}
	app := m.app
	source, remote := pane.source, pane.remote
	switch m.prompt {
	case promptNewFolder:
		if m.nameExists(side, name) {
			m.setStatus(levelWarn, name+" already exists here")
			return m, nil
		}
		target := joinPath(pane.cwd, name, pane.pathStyle)
		m.status = "creating folder " + name + "…"
		pane := m.paneRef(side)
		pane.focusName = name
		return m, func() tea.Msg {
			var err error
			if remote {
				err = app.RemoteMkdir(source, target)
			} else {
				err = os.Mkdir(target, 0o750)
			}
			return fileOpDoneMsg{side: side, verb: "created folder", what: name, err: err}
		}
	case promptNewFile:
		if m.nameExists(side, name) {
			m.setStatus(levelWarn, name+" already exists here")
			return m, nil
		}
		target := joinPath(pane.cwd, name, pane.pathStyle)
		m.status = "creating file " + name + "…"
		m.paneRef(side).focusName = name
		return m, func() tea.Msg {
			return fileOpDoneMsg{side: side, verb: "created file", what: name, err: createEmptyFile(app, source, remote, target)}
		}
	case promptRename:
		if name == m.promptItem.name {
			return m, nil
		}
		if m.nameExists(side, name) {
			m.setStatus(levelWarn, name+" already exists here — pick another name")
			return m, nil
		}
		from := joinPath(pane.cwd, m.promptItem.name, pane.pathStyle)
		to := joinPath(pane.cwd, name, pane.pathStyle)
		m.status = "renaming to " + name + "…"
		m.paneRef(side).focusName = name
		return m, func() tea.Msg {
			var err error
			if remote {
				err = app.RemoteRename(source, from, to)
			} else {
				err = os.Rename(from, to)
			}
			return fileOpDoneMsg{side: side, verb: "renamed to", what: name, err: err}
		}
	case promptChmod:
		return m.runChmod(side, m.promptItem.name, name)
	}
	return m, nil
}

// nameExists reports whether the pane's current listing already has name
// (case-insensitively on Windows targets).
func (m filesModel) nameExists(side int, name string) bool {
	pane := m.paneRefConst(side)
	for _, it := range pane.allItems {
		if it.name == name || (pane.pathStyle.IsWindows() && strings.EqualFold(it.name, name)) {
			return true
		}
	}
	return false
}

func createEmptyFile(app *core.App, source string, remote bool, target string) error {
	if !remote {
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- operator-chosen path in the pane's folder
		if err != nil {
			return err
		}
		return f.Close()
	}
	// Stage an empty controller temp and upload it to the exact path.
	tmp, err := os.CreateTemp("", "fleet-new-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpName)
	_, err = app.UploadFile(source, tmpName, target, core.FileTransferOptions{}, nil)
	return err
}

// ============================================================================
// Delete confirm overlay
// ============================================================================

func (m filesModel) openDeleteConfirm(side int) filesModel {
	items := m.selectionItems(side)
	if len(items) == 0 {
		m.status = "select an item to delete"
		return m
	}
	m.overlay = overlayConfirm
	m.confirm = confirmDelete
	m.deleteSide = side
	m.deleteItems = items
	var what string
	if len(items) == 1 {
		what = "'" + items[0].name + "'"
	} else {
		what = fmt.Sprintf("%d items", len(items))
	}
	m.confirmText = "Delete " + what + " from " + m.paneRefConst(side).label() + "?"
	return m
}

// opFailure is one item that failed in a multi-item operation.
type opFailure struct {
	name string
	err  error
}

// batchOpDoneMsg reports a multi-item operation (delete, same-pane move) with
// per-item failures.
type batchOpDoneMsg struct {
	sides    []int
	verb     string // "deleted", "moved"
	suffix   string // e.g. " into logs/"
	total    int
	failures []opFailure
}

func (m filesModel) runDelete() (tea.Model, tea.Cmd) {
	side := m.deleteSide
	pane := m.paneRefConst(side)
	m.overlay = overlayNone
	items := append([]fileItem(nil), m.deleteItems...)
	m.deleteItems = nil
	app := m.app
	source, remote, cwd, style := pane.source, pane.remote, pane.cwd, pane.pathStyle
	m.status = fmt.Sprintf("deleting %d item(s)…", len(items))
	m.clearSelection(side)
	return m, func() tea.Msg {
		var fails []opFailure
		for _, it := range items {
			target := joinPath(cwd, it.name, style)
			var err error
			if remote {
				err = app.RemoteDelete(source, target, it.isDir)
			} else {
				err = os.RemoveAll(target)
			}
			if err != nil {
				fails = append(fails, opFailure{name: it.name, err: err})
			}
		}
		return batchOpDoneMsg{sides: []int{side}, verb: "deleted", total: len(items), failures: fails}
	}
}

func (m filesModel) onBatchOpDone(msg batchOpDoneMsg) (tea.Model, tea.Cmd) {
	ok := msg.total - len(msg.failures)
	switch {
	case len(msg.failures) == 0:
		m.setStatus(levelOK, fmt.Sprintf("%s %d item(s)%s", msg.verb, msg.total, msg.suffix))
	case ok == 0 && len(msg.failures) == 1:
		f := msg.failures[0]
		m.setStatus(levelError, fmt.Sprintf("%s failed: %s — %v", strings.TrimSuffix(msg.verb, "d"), f.name, f.err))
	default:
		var parts []string
		for i, f := range msg.failures {
			if i == 3 {
				parts = append(parts, fmt.Sprintf("+%d more", len(msg.failures)-3))
				break
			}
			parts = append(parts, fmt.Sprintf("%s (%v)", f.name, f.err))
		}
		m.setStatus(levelError, fmt.Sprintf("%s %d of %d; %d failed: %s", msg.verb, ok, msg.total,
			len(msg.failures), strings.Join(parts, ", ")))
	}
	cmds := make([]tea.Cmd, 0, len(msg.sides))
	for _, s := range msg.sides {
		cmds = append(cmds, m.reload(s))
	}
	return m, tea.Batch(cmds...)
}

// ============================================================================
// Properties overlay
// ============================================================================

// propsStatMsg refreshes a remote item's properties with an authoritative stat.
type propsStatMsg struct {
	key  string
	size int64
	mode uint32
	err  error
}

func (m filesModel) openProperties(side int) (tea.Model, tea.Cmd) {
	pane := m.paneRefConst(side)
	it := m.focusedItem(side)
	if it.name == "" || it.name == ".." {
		m.status = "select an item to inspect"
		return m, nil
	}
	full := joinPath(pane.cwd, it.name, pane.pathStyle)
	kind := "File"
	if it.isDir {
		kind = "Directory"
	}
	if it.symlink {
		kind = "Symlink"
		if it.linkDir {
			kind = "Symlink → directory"
		}
	}
	lines := []string{
		"Name:     " + it.name,
		"Kind:     " + kind,
		"Where:    " + pane.label(),
		"Path:     " + full,
		"Size:     " + humanSize(it.size),
		"Mode:     " + os.FileMode(it.mode).String(),
	}
	if !it.modTime.IsZero() {
		lines = append(lines, "Modified: "+it.modTime.Format("2006-01-02 15:04:05"))
	}
	if !pane.remote {
		if fi, err := os.Lstat(full); err == nil {
			if owner := fileOwner(fi); owner != "" {
				lines = append(lines, "Owner:    "+owner)
			}
		}
		if it.symlink {
			if target, err := os.Readlink(full); err == nil {
				lines = append(lines, "Target:   "+target)
			}
		}
	}
	m.overlay = overlayProperties
	m.propsText = strings.Join(lines, "\n")
	m.propsKey = pane.source + "\x00" + full
	// For remote files prefer a fresh stat so size/mode are authoritative —
	// fetched asynchronously so the dialog opens instantly.
	if pane.remote && !it.isDir && m.app != nil {
		app, source, key := m.app, pane.source, m.propsKey
		return m, func() tea.Msg {
			st, err := app.StatRemoteFile(source, full)
			return propsStatMsg{key: key, size: st.Entry.Size, mode: st.Entry.Mode, err: err}
		}
	}
	return m, nil
}

func (m filesModel) onPropsStat(msg propsStatMsg) (tea.Model, tea.Cmd) {
	if m.overlay != overlayProperties || msg.key != m.propsKey || msg.err != nil {
		return m, nil
	}
	lines := strings.Split(m.propsText, "\n")
	for i, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "Size:"):
			lines[i] = "Size:     " + humanSize(msg.size)
		case strings.HasPrefix(ln, "Mode:"):
			lines[i] = "Mode:     " + os.FileMode(msg.mode).String()
		}
	}
	m.propsText = strings.Join(lines, "\n")
	return m, nil
}

// ============================================================================
// Filter / search overlay (per-pane name filter)
// ============================================================================

func (m filesModel) openFilter(side int) filesModel {
	m.overlay = overlayFilter
	m.filterSide = side
	m.promptValue = m.paneRefConst(side).filter
	return m
}

func (m filesModel) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	side := m.filterSide
	switch msg.String() {
	case "esc":
		// Esc clears the filter and closes.
		pane := m.paneRef(side)
		pane.filter = ""
		m.reapplyPane(side)
		m.clampScroll(side)
		m.overlay = overlayNone
		m.status = "filter cleared"
		return m, nil
	case "enter":
		m.overlay = overlayNone
		pane := m.paneRefConst(side)
		if pane.filter == "" {
			m.status = "filter cleared"
		} else {
			m.status = fmt.Sprintf("filter: %q (%d shown)", pane.filter, countReal(pane.entries))
		}
		return m, nil
	case "backspace":
		pane := m.paneRef(side)
		if r := []rune(pane.filter); len(r) > 0 {
			pane.filter = string(r[:len(r)-1])
		}
		m.applyFilterLive(side)
		return m, nil
	case "ctrl+u":
		m.paneRef(side).filter = ""
		m.applyFilterLive(side)
		return m, nil
	default:
		switch {
		case msg.Type == tea.KeyRunes && len(msg.Runes) > 0:
			m.paneRef(side).filter += string(msg.Runes)
		case msg.Type == tea.KeySpace || msg.String() == " ":
			m.paneRef(side).filter += " "
		default:
			return m, nil
		}
		m.applyFilterLive(side)
		return m, nil
	}
}

// applyFilterLive re-narrows a pane as the filter text changes, keeping the
// selection valid and the scroll in range.
func (m *filesModel) applyFilterLive(side int) {
	pane := m.paneRef(side)
	pane.index = 0
	pane.scroll = 0
	pane.selected = map[int]bool{}
	m.reapplyPane(side)
	m.clampScroll(side)
}

// ============================================================================
// Sort
// ============================================================================

// cycleSort advances the focused pane's sort key (Name → Size → Modified →
// Name), flipping to descending on the wrap so a quick repeated press walks
// every (key, direction) combination.
func (m filesModel) cycleSort(side int) filesModel {
	pane := m.paneRef(side)
	switch pane.sortBy {
	case sortName:
		pane.sortBy = sortSize
	case sortSize:
		pane.sortBy = sortModified
	default:
		pane.sortBy = sortName
		pane.sortDesc = !pane.sortDesc // flip direction each full cycle
	}
	m.reapplyPane(side)
	m.clampScroll(side)
	m.status = fmt.Sprintf("%s pane: sort by %s %s",
		pane.label(), pane.sortBy.label(), sortArrow(pane.sortDesc))
	return m
}

// setSortByColumn sorts by a clicked column; clicking the active column again
// flips the direction.
func (m filesModel) setSortByColumn(side int, col string) filesModel {
	pane := m.paneRef(side)
	key := sortName
	switch col {
	case "size":
		key = sortSize
	case "modified":
		key = sortModified
	}
	if pane.sortBy == key {
		pane.sortDesc = !pane.sortDesc
	} else {
		pane.sortBy = key
		pane.sortDesc = key != sortName // newest / largest first feels natural
	}
	m.reapplyPane(side)
	m.clampScroll(side)
	m.status = fmt.Sprintf("%s pane: sort by %s %s", pane.label(), pane.sortBy.label(), sortArrow(pane.sortDesc))
	return m
}

func sortArrow(desc bool) string {
	if desc {
		return "↓"
	}
	return "↑"
}

// ============================================================================
// Copy / Move to the OTHER pane (keyboard + toolbar entry points)
// ============================================================================

func (m filesModel) copyToOtherPane(side int) (tea.Model, tea.Cmd) {
	items := m.selectionItems(side)
	if len(items) == 0 {
		m.status = "select item(s) to copy"
		return m, nil
	}
	return m.planTransfer(side, m.other(side), items, dtCopy, -1, false)
}

func (m filesModel) moveToOtherPane(side int) (tea.Model, tea.Cmd) {
	items := m.selectionItems(side)
	if len(items) == 0 {
		m.status = "select item(s) to move"
		return m, nil
	}
	return m.planTransfer(side, m.other(side), items, dtMove, -1, false)
}

// explicitTransfer is the `u` (upload) shortcut: push the selection to the other
// pane as a copy. Kept for muscle memory; equivalent to copyToOtherPane.
func (m filesModel) explicitTransfer(side int) (tea.Model, tea.Cmd) {
	return m.copyToOtherPane(side)
}

// ============================================================================
// Drag-drop Copy/Move menu overlay
// ============================================================================

func (m filesModel) openCopyMoveMenu(drag *dragState, x, y int) filesModel {
	m.overlay = overlayCopyMove
	m.cmDrag = drag
	m.cmIndex = 0
	m.cmX, m.cmY = x, y
	return m
}

func (m filesModel) chooseCopyMove(copy bool) (tea.Model, tea.Cmd) {
	drag := m.cmDrag
	m.overlay = overlayNone
	m.cmDrag = nil
	if drag == nil {
		return m, nil
	}
	kind := dtMove
	if copy {
		kind = dtCopy
	}
	// Begin a brief snap animation toward the drop point before transferring.
	m.drag = &dragState{
		fromSide: drag.fromSide, items: drag.items, primary: drag.primary,
		active: true, snapping: true, snapUntil: time.Now().Add(snapDuration),
		snapX: m.cmX, snapY: m.cmY,
	}
	model, cmd := m.planTransfer(drag.fromSide, m.other(drag.fromSide), drag.items, kind, m.cmTarget, true)
	return model, tea.Batch(cmd, snapCmd())
}

func snapCmd() tea.Cmd {
	return tea.Tick(snapDuration, func(time.Time) tea.Msg { return snapTickMsg{} })
}

// ============================================================================
// Same-pane drag onto a folder = immediate Move (rename)
// ============================================================================

func (m filesModel) sameDirMove(drag *dragState, dstDir fileItem) (tea.Model, tea.Cmd) {
	pane := m.paneRefConst(drag.fromSide)
	dstBase := joinPath(pane.cwd, dstDir.name, pane.pathStyle)
	app := m.app
	source, remote, cwd, style := pane.source, pane.remote, pane.cwd, pane.pathStyle
	side := drag.fromSide
	items := append([]fileItem(nil), drag.items...)
	m.status = fmt.Sprintf("moving %d item(s) into %s/…", len(items), dstDir.name)
	return m, func() tea.Msg {
		var fails []opFailure
		n := 0
		for _, it := range items {
			if sameItem(it, dstDir) {
				continue
			}
			n++
			from := joinPath(cwd, it.name, style)
			to := joinPath(dstBase, it.name, style)
			var err error
			if remote {
				err = app.RemoteRename(source, from, to)
			} else {
				err = os.Rename(from, to)
			}
			if err != nil {
				fails = append(fails, opFailure{name: it.name, err: err})
			}
		}
		return batchOpDoneMsg{sides: []int{side}, verb: "moved", suffix: " into " + dstDir.name + "/", total: n, failures: fails}
	}
}

// ============================================================================
// Overlay input dispatch
// ============================================================================

func (m filesModel) handleOverlayKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch m.overlay {
	case overlaySourcePicker:
		switch key {
		case "esc", "q":
			m.overlay = overlayNone
			return m, nil
		case "up", "k":
			if m.pickerIndex > 0 {
				m.pickerIndex--
			}
			return m, nil
		case "down", "j":
			if m.pickerIndex < len(m.pickerItems)-1 {
				m.pickerIndex++
			}
			return m, nil
		case "enter", "l", " ":
			return m.chooseSource(m.pickerIndex)
		}
		return m, nil

	case overlayContextMenu:
		switch key {
		case "esc", "q":
			m.overlay = overlayNone
			return m, nil
		case "up", "k":
			m.menuIndex = m.prevEnabled(m.menuIndex)
			return m, nil
		case "down", "j":
			m.menuIndex = m.nextEnabled(m.menuIndex)
			return m, nil
		case "enter", "l":
			if m.menuIndex >= 0 && m.menuIndex < len(m.menuItems) && m.menuItems[m.menuIndex].enabled {
				return m.runContextAction(m.menuItems[m.menuIndex].action)
			}
			return m, nil
		}
		// Shortcut keys inside the menu.
		for _, it := range m.menuItems {
			if it.enabled && it.key == key {
				return m.runContextAction(it.action)
			}
		}
		return m, nil

	case overlayCopyMove:
		switch key {
		case "esc", "q":
			m.overlay = overlayNone
			m.cmDrag = nil
			return m, nil
		case "left", "right", "h", "l", "tab":
			m.cmIndex = (m.cmIndex + 1) % 2
			return m, nil
		case "c":
			return m.chooseCopyMove(true)
		case "m":
			return m.chooseCopyMove(false)
		case "enter", " ":
			return m.chooseCopyMove(m.cmIndex == 0)
		}
		return m, nil

	case overlayConfirm:
		if m.confirm == confirmTransfer {
			return m.handlePlanKey(msg)
		}
		switch key {
		case "esc", "q", "n":
			m.overlay = overlayNone
			m.plan = nil
			m.deleteItems = nil
			m.status = "cancelled"
			return m, nil
		case "enter", "y":
			return m.runDelete()
		}
		return m, nil

	case overlayPrompt:
		switch key {
		case "esc":
			m.overlay = overlayNone
			m.status = "cancelled"
			return m, nil
		case "enter":
			return m.submitPrompt()
		case "backspace":
			if len(m.promptValue) > 0 {
				r := []rune(m.promptValue)
				m.promptValue = string(r[:len(r)-1])
			}
			return m, nil
		case "ctrl+u":
			m.promptValue = ""
			return m, nil
		default:
			switch {
			case msg.Type == tea.KeyRunes && len(msg.Runes) > 0:
				m.promptValue += string(msg.Runes)
			case msg.Type == tea.KeySpace || key == " ":
				m.promptValue += " "
			}
			return m, nil
		}

	case overlayProperties:
		switch key {
		case "esc", "q", "enter", "i":
			m.overlay = overlayNone
			return m, nil
		}
		return m, nil

	case overlayEditor:
		return m.handleEditorKey(msg)

	case overlayFilter:
		return m.handleFilterKey(msg)

	case overlayCompress:
		return m.handleCompressKey(msg)

	case overlayHelp:
		return m.handleHelpKey(msg)

	case overlayGoto:
		return m.handleGotoKey(msg)

	case overlayJump:
		return m.handleJumpKey(msg)

	case overlayPlaces:
		return m.handlePlacesKey(msg)
	}
	return m, nil
}

// handleCompressKey drives the archive overlay: ←/→ (or Tab) cycle the format,
// typing edits the archive name, Enter compresses, Esc cancels.
func (m filesModel) handleCompressKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.overlay = overlayNone
		m.status = "cancelled"
		return m, nil
	case "enter":
		return m.submitCompress()
	case "left", "shift+tab":
		m.cycleCompressFormat(-1)
		return m, nil
	case "right", "tab":
		m.cycleCompressFormat(1)
		return m, nil
	case "backspace":
		if r := []rune(m.compressName); len(r) > 0 {
			m.compressName = string(r[:len(r)-1])
			m.compressEditing = true
		}
		return m, nil
	case "ctrl+u":
		m.compressName = ""
		m.compressEditing = true
		return m, nil
	default:
		switch {
		case msg.Type == tea.KeyRunes && len(msg.Runes) > 0:
			m.compressName += string(msg.Runes)
			m.compressEditing = true
		case msg.Type == tea.KeySpace || msg.String() == " ":
			m.compressName += " "
			m.compressEditing = true
		}
		return m, nil
	}
}

func (m filesModel) handleOverlayMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress {
		return m, nil
	}
	if msg.Button != tea.MouseButtonLeft {
		// Right-click or other: dismiss menus.
		if m.overlay == overlayContextMenu || m.overlay == overlayCopyMove {
			m.overlay = overlayNone
			m.cmDrag = nil
		}
		return m, nil
	}
	switch m.overlay {
	case overlaySourcePicker:
		for i := range m.pickerItems {
			if zone.Get(fmt.Sprintf("%s%d", fmPickPrefix, i)).InBounds(msg) {
				return m.chooseSource(i)
			}
		}
		m.overlay = overlayNone
		return m, nil
	case overlayContextMenu:
		for i, it := range m.menuItems {
			if zone.Get(fmt.Sprintf("%s%d", fmMenuPrefix, i)).InBounds(msg) {
				if it.enabled {
					return m.runContextAction(it.action)
				}
				return m, nil
			}
		}
		m.overlay = overlayNone
		return m, nil
	case overlayCopyMove:
		if zone.Get(fmCMPrefix + "copy").InBounds(msg) {
			return m.chooseCopyMove(true)
		}
		if zone.Get(fmCMPrefix + "move").InBounds(msg) {
			return m.chooseCopyMove(false)
		}
		if zone.Get(fmCMPrefix + "cancel").InBounds(msg) {
			m.overlay = overlayNone
			m.cmDrag = nil
			return m, nil
		}
		m.overlay = overlayNone
		m.cmDrag = nil
		return m, nil
	case overlayCompress:
		if zone.Get(fmCompressPrefix + "format").InBounds(msg) {
			m.cycleCompressFormat(1)
			return m, nil
		}
		if zone.Get(fmCompressPrefix + "ok").InBounds(msg) {
			return m.submitCompress()
		}
		if zone.Get(fmCompressPrefix + "cancel").InBounds(msg) {
			m.overlay = overlayNone
			m.status = "cancelled"
			return m, nil
		}
		return m, nil
	case overlayPlaces:
		for i := range m.placesItems {
			if zone.Get(fmt.Sprintf("%s%d", fmPlacePrefix, i)).InBounds(msg) {
				return m.choosePlace(i)
			}
		}
		m.overlay = overlayNone
		return m, nil
	case overlayConfirm:
		if m.confirm == confirmTransfer {
			return m.handlePlanMouse(msg)
		}
		m.overlay = overlayNone
		m.deleteItems = nil
		m.status = "cancelled"
		return m, nil
	case overlayProperties, overlayHelp:
		m.overlay = overlayNone
		return m, nil
	}
	return m, nil
}

func (m filesModel) nextEnabled(from int) int {
	for i := from + 1; i < len(m.menuItems); i++ {
		if m.menuItems[i].enabled {
			return i
		}
	}
	return from
}

func (m filesModel) prevEnabled(from int) int {
	for i := from - 1; i >= 0; i-- {
		if m.menuItems[i].enabled {
			return i
		}
	}
	return from
}

// ---- small helpers ----

func sideName(side int) string {
	if side == 1 {
		return "right"
	}
	return "left"
}

// transferGlyph picks an arrow/label glyph reflecting the route + verb.
func transferGlyph(srcRemote, dstRemote bool, kind dirTransferKind) string {
	switch {
	case !srcRemote && dstRemote:
		return "↑" // upload
	case srcRemote && !dstRemote:
		return "↓" // download
	default:
		if kind == dtMove {
			return "↦" // move
		}
		return "⇒" // copy
	}
}
