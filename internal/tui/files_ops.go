// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
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

	// Wheel scrolls whichever pane is under the cursor.
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if m.overlay == overlayNone {
			if side, _, ok := m.hitRow(msg); ok {
				m.movePane(side, -1)
			}
		}
		return m, nil
	case tea.MouseButtonWheelDown:
		if m.overlay == overlayNone {
			if side, _, ok := m.hitRow(msg); ok {
				m.movePane(side, 1)
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
	// Update hover target.
	if side, idx, ok := m.hitRow(msg); ok && idx >= 0 {
		m.hoverSide, m.hoverIndex = side, idx
	} else {
		m.hoverSide, m.hoverIndex = -1, -1
	}
	// Promote a press into an active drag once the mouse is held and moves.
	if m.drag != nil && msg.Button == tea.MouseButtonLeft {
		m.drag.active = true
	}
	return m, nil
}

func (m filesModel) handleMousePress(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// Right-click opens the context menu on the row/pane under the cursor.
	if msg.Button == tea.MouseButtonRight {
		side, idx, ok := m.hitRow(msg)
		if !ok {
			return m, nil
		}
		m.focus = side
		if idx >= 0 && idx < len(m.paneRefConst(side).entries) {
			m.paneRef(side).index = idx
		}
		return m.openContextMenu(side, idx, msg.X, msg.Y), nil
	}

	if msg.Button != tea.MouseButtonLeft {
		return m, nil
	}

	// Toolbar buttons.
	if act, ok := hitToolbar(msg); ok {
		return m.applyAction(act)
	}

	// Header click opens the source picker for that pane.
	for side := range 2 {
		if zone.Get(headerZoneID(side)).InBounds(msg) {
			m.focus = side
			return m.openSourcePicker(side), nil
		}
	}

	side, idx, ok := m.hitRow(msg)
	if !ok {
		return m, nil
	}
	m.focus = side
	if idx >= 0 {
		pane := m.paneRef(side)
		pane.index = idx
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
	if msg.Button != tea.MouseButtonLeft {
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

// hitRow resolves a mouse event to (side, rowIndex, inSomePane). rowIndex is -1
// when inside a pane but not over a row.
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

func hitToolbar(msg tea.MouseMsg) (string, bool) {
	for _, name := range toolbarActions {
		if zone.Get(fmActPrefix + name).InBounds(msg) {
			return name, true
		}
	}
	return "", false
}

// toolbarActions are the clickable toolbar buttons (also keyboard shortcuts).
var toolbarActions = []string{
	"source", "newfolder", "rename", "delete", "copy", "move", "props", "view", "hidden", "refresh", "quit",
}

func (m filesModel) applyAction(name string) (tea.Model, tea.Cmd) {
	switch name {
	case "source":
		return m.openSourcePicker(m.focus), nil
	case "newfolder":
		return m.openNewFolderPrompt(m.focus), nil
	case "rename":
		return m.openRenamePrompt(m.focus), nil
	case "delete":
		return m.openDeleteConfirm(m.focus), nil
	case "copy":
		return m.copyToOtherPane(m.focus)
	case "move":
		return m.moveToOtherPane(m.focus)
	case "props":
		return m.openProperties(m.focus)
	case "view":
		m.cycleView(m.focus)
		return m, nil
	case "hidden":
		m.showHidden = !m.showHidden
		m.left.loading, m.right.loading = true, true
		m.status = "hidden files " + onOff(m.showHidden)
		return m, m.refreshBoth()
	case "refresh":
		m.left.loading, m.right.loading = true, true
		return m, m.refreshBoth()
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
	*pane = newPaneSource(m.app, source)
	m.overlay = overlayNone
	m.focus = side
	m.status = "switched " + sideName(side) + " pane to " + choice
	return m, m.loadCmd(side, pane.source, pane.cwd)
}

// ============================================================================
// Context menu overlay
// ============================================================================

type contextMenuItem struct {
	key     string
	label   string
	action  string
	enabled bool
}

func (m filesModel) openContextMenu(side, idx, x, y int) filesModel {
	pane := m.paneRefConst(side)
	onRow := idx >= 0 && idx < len(pane.entries) && pane.entries[idx].name != ".."
	isDir := onRow && pane.entries[idx].isDir
	items := []contextMenuItem{
		{key: "↵", label: "Open", action: "open", enabled: onRow || (idx >= 0 && idx < len(pane.entries))},
		{key: "c", label: "Copy to other pane", action: "copy", enabled: onRow},
		{key: "m", label: "Move to other pane", action: "move", enabled: onRow},
		{key: "r", label: "Rename", action: "rename", enabled: onRow},
		{key: "d", label: "Delete", action: "delete", enabled: onRow},
		{key: "n", label: "New folder", action: "newfolder", enabled: true},
		{key: "i", label: "Properties", action: "props", enabled: onRow},
		{key: "v", label: viewLabel(m.paneRefConst(side).view), action: "view", enabled: true},
		{key: "g", label: "Refresh", action: "refresh", enabled: true},
	}
	_ = isDir
	m.overlay = overlayContextMenu
	m.menuItems = items
	m.menuIndex = 0
	m.menuX, m.menuY = x, y
	m.menuSide, m.menuRow = side, idx
	return m
}

func (m filesModel) runContextAction(action string) (tea.Model, tea.Cmd) {
	side := m.menuSide
	m.overlay = overlayNone
	switch action {
	case "open":
		return m.activate(side)
	case "copy":
		return m.copyToOtherPane(side)
	case "move":
		return m.moveToOtherPane(side)
	case "rename":
		return m.openRenamePrompt(side), nil
	case "delete":
		return m.openDeleteConfirm(side), nil
	case "newfolder":
		return m.openNewFolderPrompt(side), nil
	case "props":
		return m.openProperties(side)
	case "view":
		m.cycleView(side)
		return m, nil
	case "refresh":
		return m, m.reload(side)
	}
	return m, nil
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

func (m filesModel) submitPrompt() (tea.Model, tea.Cmd) {
	side := m.promptSide
	name := strings.TrimSpace(m.promptValue)
	m.overlay = overlayNone
	if name == "" {
		m.status = "cancelled"
		return m, nil
	}
	pane := m.paneRefConst(side)
	switch m.prompt {
	case promptNewFolder:
		if strings.ContainsAny(name, "/\\") {
			m.status = "invalid folder name"
			return m, nil
		}
		target := joinPath(pane.cwd, name, pane.remote)
		if pane.remote {
			if err := m.app.RemoteMkdir(pane.source, target); err != nil {
				m.status = "mkdir failed: " + err.Error()
				return m, nil
			}
		} else {
			if err := os.Mkdir(target, 0o750); err != nil {
				m.status = "mkdir failed: " + err.Error()
				return m, nil
			}
		}
		m.status = "created folder " + name
		return m, m.reload(side)
	case promptRename:
		if strings.ContainsAny(name, "/\\") {
			m.status = "invalid name"
			return m, nil
		}
		from := joinPath(pane.cwd, m.promptItem.name, pane.remote)
		to := joinPath(pane.cwd, name, pane.remote)
		if pane.remote {
			if err := m.app.RemoteRename(pane.source, from, to); err != nil {
				m.status = "rename failed: " + err.Error()
				return m, nil
			}
		} else {
			if err := os.Rename(from, to); err != nil {
				m.status = "rename failed: " + err.Error()
				return m, nil
			}
		}
		m.status = "renamed to " + name
		return m, m.reload(side)
	}
	return m, nil
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

func (m filesModel) runDelete() (tea.Model, tea.Cmd) {
	side := m.deleteSide
	pane := m.paneRefConst(side)
	m.overlay = overlayNone
	var firstErr error
	for _, it := range m.deleteItems {
		target := joinPath(pane.cwd, it.name, pane.remote)
		var err error
		if pane.remote {
			err = m.app.RemoteDelete(pane.source, target, it.isDir)
		} else {
			err = os.RemoveAll(target)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		m.status = "delete failed: " + firstErr.Error()
	} else {
		m.status = fmt.Sprintf("deleted %d item(s)", len(m.deleteItems))
	}
	m.deleteItems = nil
	return m, m.reload(side)
}

// ============================================================================
// Properties overlay
// ============================================================================

func (m filesModel) openProperties(side int) (tea.Model, tea.Cmd) {
	pane := m.paneRefConst(side)
	it := m.focusedItem(side)
	if it.name == "" || it.name == ".." {
		m.status = "select an item to inspect"
		return m, nil
	}
	full := joinPath(pane.cwd, it.name, pane.remote)
	kind := "File"
	if it.isDir {
		kind = "Directory"
	}
	if it.symlink {
		kind = "Symlink"
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
	// For remote files prefer a fresh stat so size/mode are authoritative.
	if pane.remote && !it.isDir {
		if st, err := m.app.StatRemoteFile(pane.source, full); err == nil {
			lines[4] = "Size:     " + humanSize(st.Entry.Size)
			lines[5] = "Mode:     " + os.FileMode(st.Entry.Mode).String()
		}
	}
	m.overlay = overlayProperties
	m.propsText = strings.Join(lines, "\n")
	return m, nil
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
	return m.startBatch(side, m.other(side), items, dtCopy, -1)
}

func (m filesModel) moveToOtherPane(side int) (tea.Model, tea.Cmd) {
	items := m.selectionItems(side)
	if len(items) == 0 {
		m.status = "select item(s) to move"
		return m, nil
	}
	return m.startBatch(side, m.other(side), items, dtMove, -1)
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
	model, cmd := m.startBatch(drag.fromSide, m.other(drag.fromSide), drag.items, kind, m.cmTarget)
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
	dstBase := joinPath(pane.cwd, dstDir.name, pane.remote)
	var firstErr error
	for _, it := range drag.items {
		if sameItem(it, dstDir) {
			continue
		}
		from := joinPath(pane.cwd, it.name, pane.remote)
		to := joinPath(dstBase, it.name, pane.remote)
		var err error
		if pane.remote {
			err = m.app.RemoteRename(pane.source, from, to)
		} else {
			err = os.Rename(from, to)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		m.status = "move failed: " + firstErr.Error()
	} else {
		m.status = fmt.Sprintf("moved %d item(s) into %s/", len(drag.items), dstDir.name)
	}
	return m, m.reload(drag.fromSide)
}

// ============================================================================
// Batch + routing: the heart of the transfer engine
// ============================================================================

// startBatch routes a set of items from one pane to another. Directory items
// pop a size-confirmation modal (one at a time); file items transfer immediately
// in parallel. targetIdx selects a destination folder row in the dest pane, or
// -1 for the dest pane's cwd.
func (m filesModel) startBatch(fromSide, toSide int, items []fileItem, kind dirTransferKind, targetIdx int) (tea.Model, tea.Cmd) {
	if fromSide == toSide || len(items) == 0 {
		return m, nil
	}
	destDir := m.destDir(toSide, targetIdx)

	var cmds []tea.Cmd
	var dirItem *fileItem
	for i := range items {
		it := items[i]
		if it.isDir {
			// Confirm the FIRST directory; queue remaining handling after.
			dirItem = &items[i]
			break
		}
		cmds = append(cmds, m.transferOne(fromSide, toSide, it, destDir, kind))
	}

	if dirItem != nil {
		// Open the dir confirm modal; remember dest for the confirmed run. With a
		// mixed selection we confirm one directory at a time; any file items above
		// have already started transferring in parallel.
		mm := m.openDirConfirm(fromSide, toSide, *dirItem, destDir, kind)
		cmds = append(cmds, mm.dirScanCmd)
		return mm, tea.Batch(cmds...)
	}

	if len(cmds) == 0 {
		return m, nil
	}
	m.status = fmt.Sprintf("started %d transfer(s)", len(cmds))
	return m, tea.Batch(cmds...)
}

// destDir computes the absolute destination directory in the dest pane.
func (m filesModel) destDir(toSide, targetIdx int) string {
	pane := m.paneRefConst(toSide)
	if targetIdx >= 0 && targetIdx < len(pane.entries) {
		dst := pane.entries[targetIdx]
		if dst.isDir && dst.name != ".." {
			return joinPath(pane.cwd, dst.name, pane.remote)
		}
	}
	return pane.cwd
}

// transferOne builds the tea.Cmd that performs a single file copy/move from the
// source pane to a destination directory, routed by the two panes' source types.
func (m *filesModel) transferOne(fromSide, toSide int, it fileItem, destDir string, kind dirTransferKind) tea.Cmd {
	src := m.paneRefConst(fromSide)
	dst := m.paneRefConst(toSide)
	srcPath := joinPath(src.cwd, it.name, src.remote)

	// Vet remote-derived names before composing a LOCAL destination path.
	var dstPath string
	if !dst.remote {
		safe, err := core.SafeLocalJoin(destDir, it.name)
		if err != nil {
			m.status = "refused unsafe name: " + it.name
			return nil
		}
		dstPath = safe
	} else {
		dstPath = joinPath(destDir, it.name, true)
	}

	id := m.nextID
	m.nextID++
	c := &transferChans{
		progress: make(chan core.ProgressUpdate, 1),
		done:     make(chan transferOutcome, 1),
	}
	m.chans[id] = c
	progress := func(u core.ProgressUpdate) {
		select {
		case c.progress <- u:
		default:
		}
	}

	app := m.app
	label := fmt.Sprintf("%s %s", transferGlyph(src.remote, dst.remote, kind), it.name)
	m.transfers = append(m.transfers, &transferRow{id: id, label: label, total: it.size})

	go func() {
		err := runFileOp(app, src.source, srcPath, dst.source, dstPath, kind, progress)
		c.done <- transferOutcome{err: err}
	}()
	return pollTransferCmd(id, c)
}

// runFileOp dispatches a single-file operation by source/dest type and kind.
//
//	local  -> remote : Upload  (+ delete source for move)
//	remote -> local  : Download(+ delete source for move)
//	remote -> remote : Copy / Move(rename or relay)
//	local  -> local  : os copy (via relay-less rename/copy) — handled below
func runFileOp(app *core.App, srcServer, srcPath, dstServer, dstPath string, kind dirTransferKind, progress core.ProgressFunc) error {
	opts := core.FileTransferOptions{}
	switch {
	case srcServer == "" && dstServer != "":
		// local -> remote
		if _, err := app.UploadFile(dstServer, srcPath, dstPath, opts, progress); err != nil {
			return err
		}
		if kind == dtMove {
			return os.Remove(srcPath)
		}
		return nil
	case srcServer != "" && dstServer == "":
		// remote -> local
		if _, err := app.DownloadFile(srcServer, srcPath, dstPath, opts, progress); err != nil {
			return err
		}
		if kind == dtMove {
			return app.RemoteDelete(srcServer, srcPath, false)
		}
		return nil
	case srcServer != "" && dstServer != "":
		// remote -> remote
		if kind == dtMove {
			return app.MoveFile(srcServer, srcPath, dstServer, dstPath, opts, progress)
		}
		_, err := app.CopyFile(srcServer, srcPath, dstServer, dstPath, opts, progress)
		return err
	default:
		// local -> local
		return localFileCopyMove(srcPath, dstPath, kind, progress)
	}
}

// localFileCopyMove copies (or moves) a single local file. Move tries rename
// first (cheap, cross-pane on the same FS) then falls back to copy+remove.
func localFileCopyMove(srcPath, dstPath string, kind dirTransferKind, progress core.ProgressFunc) error {
	if kind == dtMove {
		if err := os.Rename(srcPath, dstPath); err == nil {
			if progress != nil {
				if fi, e := os.Stat(dstPath); e == nil {
					progress(core.ProgressUpdate{BytesDone: fi.Size(), TotalBytes: fi.Size(), Done: true})
				}
			}
			return nil
		}
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	if err := copyStream(in, out, progress); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if kind == dtMove {
		return os.Remove(srcPath)
	}
	return nil
}

func copyStream(in *os.File, out *os.File, progress core.ProgressFunc) error {
	var total int64
	if fi, err := in.Stat(); err == nil {
		total = fi.Size()
	}
	buf := make([]byte, 1<<20)
	var done int64
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
			done += int64(n)
			if progress != nil {
				progress(core.ProgressUpdate{BytesDone: done, TotalBytes: total})
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if progress != nil {
		progress(core.ProgressUpdate{BytesDone: done, TotalBytes: total, Done: true})
	}
	return nil
}

// ============================================================================
// Directory transfer confirmation
// ============================================================================

func (m filesModel) openDirConfirm(fromSide, toSide int, it fileItem, destDir string, kind dirTransferKind) filesModel {
	m.overlay = overlayConfirm
	m.confirm = confirmDirTransfer
	src := m.paneRefConst(fromSide)
	m.pendingDir = &pendingDirTransfer{kind: kind, fromSide: fromSide, item: it}
	// Stash destination + sides for the confirmed run.
	m.pendingDirDest = destDir
	m.pendingDirTo = toSide
	m.confirmText = m.dirConfirmText()
	// Kick off the async estimate.
	srcPath := joinPath(src.cwd, it.name, src.remote)
	return m.withDirScan(src.source, srcPath)
}

func (m filesModel) dirConfirmText() string {
	if m.pendingDir == nil {
		return ""
	}
	pd := m.pendingDir
	verb := "Copy"
	if pd.kind == dtMove {
		verb = "Move"
	}
	dest := m.paneRefConst(m.pendingDirTo).label()
	size := "(scanning…)"
	if pd.scanErr != nil {
		size = "(scan failed)"
	} else if pd.scanned {
		size = fmt.Sprintf("%d items, ~%s", pd.files, humanSize(pd.bytes))
	}
	return fmt.Sprintf("%s directory '%s' (%s) to %s?", verb, pd.item.name, size, dest)
}

func (m filesModel) withDirScan(source, path string) filesModel {
	m.dirScanCmd = m.dirScanCmdFor(source, path)
	return m
}

func (m filesModel) dirScanCmdFor(source, scanPath string) tea.Cmd {
	app := m.app
	return func() tea.Msg {
		if source == "" {
			f, b, err := core.EstimateLocalTree(scanPath)
			return dirScanMsg{files: f, bytes: b, err: err}
		}
		f, b, err := app.EstimateRemoteTree(source, scanPath)
		return dirScanMsg{files: f, bytes: b, err: err}
	}
}

func (m filesModel) runDirTransfer() (tea.Model, tea.Cmd) {
	pd := m.pendingDir
	m.overlay = overlayNone
	if pd == nil {
		return m, nil
	}
	fromSide := pd.fromSide
	toSide := m.pendingDirTo
	destDir := m.pendingDirDest
	src := m.paneRefConst(fromSide)
	dst := m.paneRefConst(toSide)
	srcPath := joinPath(src.cwd, pd.item.name, src.remote)

	var dstPath string
	if !dst.remote {
		safe, err := core.SafeLocalJoin(destDir, pd.item.name)
		if err != nil {
			m.status = "refused unsafe name: " + pd.item.name
			m.pendingDir = nil
			return m, nil
		}
		dstPath = safe
	} else {
		dstPath = joinPath(destDir, pd.item.name, true)
	}

	id := m.nextID
	m.nextID++
	c := &transferChans{
		progress: make(chan core.ProgressUpdate, 1),
		done:     make(chan transferOutcome, 1),
	}
	m.chans[id] = c
	progress := func(u core.ProgressUpdate) {
		select {
		case c.progress <- u:
		default:
		}
	}
	app := m.app
	srcSource, dstSource := src.source, dst.source
	kind := pd.kind
	label := fmt.Sprintf("%s %s/", transferGlyph(src.remote, dst.remote, kind), pd.item.name)
	m.transfers = append(m.transfers, &transferRow{id: id, label: label, total: pd.bytes})
	m.status = "started directory " + label

	go func() {
		err := runDirOp(app, srcSource, srcPath, dstSource, dstPath, kind, progress)
		c.done <- transferOutcome{err: err}
	}()
	m.pendingDir = nil
	return m, pollTransferCmd(id, c)
}

// runDirOp dispatches a recursive directory operation by source/dest type.
func runDirOp(app *core.App, srcServer, srcPath, dstServer, dstPath string, kind dirTransferKind, progress core.ProgressFunc) error {
	opts := core.FileTransferOptions{}
	switch {
	case srcServer == "" && dstServer != "":
		if _, err := app.UploadDir(dstServer, srcPath, dstPath, opts, progress); err != nil {
			return err
		}
		if kind == dtMove {
			return os.RemoveAll(srcPath)
		}
		return nil
	case srcServer != "" && dstServer == "":
		if _, err := app.DownloadDir(srcServer, srcPath, dstPath, opts, progress); err != nil {
			return err
		}
		if kind == dtMove {
			return app.RemoteDelete(srcServer, srcPath, true)
		}
		return nil
	case srcServer != "" && dstServer != "":
		if kind == dtMove {
			_, err := app.MoveDir(srcServer, srcPath, dstServer, dstPath, opts, progress)
			return err
		}
		_, err := app.CopyDir(srcServer, srcPath, dstServer, dstPath, opts, progress)
		return err
	default:
		return localDirCopyMove(srcPath, dstPath, kind, progress)
	}
}

// localDirCopyMove recursively copies (or moves) a local directory tree.
func localDirCopyMove(srcPath, dstPath string, kind dirTransferKind, progress core.ProgressFunc) error {
	if kind == dtMove {
		if err := os.Rename(srcPath, dstPath); err == nil {
			if progress != nil {
				progress(core.ProgressUpdate{Done: true})
			}
			return nil
		}
	}
	err := filepath.WalkDir(srcPath, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(srcPath, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dstPath, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		in, oerr := os.Open(p)
		if oerr != nil {
			return oerr
		}
		defer in.Close()
		out, cerr := os.Create(target)
		if cerr != nil {
			return cerr
		}
		if cperr := copyStream(in, out, nil); cperr != nil {
			_ = out.Close()
			return cperr
		}
		return out.Close()
	})
	if err != nil {
		return err
	}
	if progress != nil {
		progress(core.ProgressUpdate{Done: true})
	}
	if kind == dtMove {
		return os.RemoveAll(srcPath)
	}
	return nil
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
		switch key {
		case "esc", "q", "n":
			m.overlay = overlayNone
			m.pendingDir = nil
			m.deleteItems = nil
			m.status = "cancelled"
			return m, nil
		case "enter", "y":
			if m.confirm == confirmDelete {
				return m.runDelete()
			}
			return m.runDirTransfer()
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
	}
	return m, nil
}

func (m filesModel) handleOverlayMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress && msg.Action != tea.MouseActionRelease {
		return m, nil
	}
	if msg.Action == tea.MouseActionRelease {
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
	case overlayConfirm, overlayProperties, overlayPrompt:
		// Click outside dismisses (confirm/props); prompt keeps focus.
		if m.overlay != overlayPrompt {
			m.overlay = overlayNone
		}
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
