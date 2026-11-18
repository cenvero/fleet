// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cenvero/fleet/internal/core"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// maxEditBytes caps the size of a file the editor will load. Larger files (or
// binaries) are refused so the TUI never tries to syntax-highlight a multi-MiB
// blob or render NUL-laden binary content.
const maxEditBytes = 2 << 20 // 2 MiB

// editorMode distinguishes the read-only highlighted viewer from the plain
// editable textarea. The viewer is the default landing state (open → see
// highlighted content); pressing the toggle drops into the textarea to edit.
type editorMode int

const (
	editorView editorMode = iota // syntax-highlighted, read-only
	editorEdit                   // plain textarea, editable
)

// editorState holds everything the full-screen editor overlay needs. It lives on
// filesModel and is reset each time the editor opens.
type editorState struct {
	active  bool
	gen     uint64 // this session's number (filesModel.editorGen)
	side    int    // the pane the file belongs to (for refresh + source)
	source  string // "" = local, else server name
	path    string // absolute path of the file being edited
	name    string // base name (drives the lexer + the header)
	mode    editorMode
	area    textarea.Model
	codec   editorCodec // maps the file text onto area's text and back
	content string      // last-saved file text (for dirty detection)
	dirty   bool
	saving  bool
	loaded  bool // the file's content has arrived
	// readOnly says why the file cannot be edited here ("" = editable): its
	// content has no exact form in the textarea (see editorCodec).
	readOnly string
	// discardArmed is set by an Esc that warned about unsaved edits; the
	// next key, if it is Esc, discards them.
	discardArmed bool
	// inPlace is set after a save failed because the file cannot be replaced
	// (a bind-mounted file, a folder the agent may not write…): the next
	// save writes into the file itself.
	inPlace  bool
	status   string // inline message inside the editor footer (errors, hints)
	viewScrl int    // scroll offset (top line) for the read-only highlighted view

	// viewLines caches the highlighted lines of viewSrc so scrolling the viewer
	// does not re-tokenise the whole file on every frame.
	viewLines []string
	viewSrc   string
}

// discardWarning is the footer message of the first Esc over unsaved edits.
const discardWarning = "unsaved changes — Ctrl+S to save, Esc again to discard"

// editorLoadedMsg carries the result of loading a file into the editor.
type editorLoadedMsg struct {
	gen     uint64 // the editor session that asked for it
	side    int
	source  string
	path    string
	name    string
	content string
	lines   []string // highlighted content, computed off the UI thread
	err     error
}

// editorSavedMsg carries the result of an asynchronous save and the content
// that was saved (the base the next save is checked against). gen names the
// editor session that saved; side and name let a result that outlived its
// session still be reported.
type editorSavedMsg struct {
	gen     uint64
	side    int
	name    string
	err     error
	content string
}

// openEditor begins loading the focused file into the editor overlay. It refuses
// directories and "..". The heavy lifting (read local / cat remote) happens off
// the UI thread via editorLoadCmd.
func (m filesModel) openEditor(side int) (tea.Model, tea.Cmd) {
	pane := m.paneRefConst(side)
	it := m.focusedItem(side)
	if it.name == "" || it.name == ".." {
		m.status = "select a file to edit"
		return m, nil
	}
	if it.isDir {
		m.status = "cannot edit a directory"
		return m, nil
	}
	full := joinPath(pane.cwd, it.name, pane.pathStyle)
	m.overlay = overlayEditor
	m.editorGen++
	m.editor = &editorState{
		active: true, gen: m.editorGen, side: side, source: pane.source,
		path: full, name: it.name, mode: editorView,
		status: "loading…",
	}
	m.status = "opening " + it.name + "…"
	return m, m.editorLoadCmd(m.editorGen, side, pane.source, full, it.name)
}

// editorLoadCmd reads the file content (local or remote) into memory, enforcing
// the size cap and rejecting binary content (detected via NUL bytes).
func (m filesModel) editorLoadCmd(gen uint64, side int, source, full, name string) tea.Cmd {
	app := m.app
	return func() tea.Msg {
		data, err := loadFileForEdit(app, source, full)
		var lines []string
		if err == nil {
			lines = highlightLines(name, data)
		}
		return editorLoadedMsg{gen: gen, side: side, source: source, path: full, name: name, content: data, lines: lines, err: err}
	}
}

// loadFileForEdit fetches the content for editing, applying the size cap and a
// binary-content check. Local files are read with os.ReadFile; remote files are
// streamed via CatRemoteFile into a bounded buffer.
func loadFileForEdit(app *core.App, source, full string) (string, error) {
	var raw []byte
	if source == "" {
		fi, err := os.Stat(full)
		if err != nil {
			return "", err
		}
		if fi.IsDir() {
			return "", fmt.Errorf("not a regular file")
		}
		if fi.Size() > maxEditBytes {
			return "", fmt.Errorf("file is too large to edit (%s; limit %s)",
				humanSize(fi.Size()), humanSize(maxEditBytes))
		}
		raw, err = os.ReadFile(full) // #nosec G304 -- operator-selected path
		if err != nil {
			return "", err
		}
	} else {
		var buf limitedBuffer
		buf.limit = maxEditBytes
		if _, err := app.CatRemoteFile(source, full, &buf); err != nil {
			if buf.tripped {
				return "", fmt.Errorf("file is too large to edit (limit %s)", humanSize(maxEditBytes))
			}
			return "", err
		}
		raw = buf.Bytes()
	}
	if isBinary(raw) {
		return "", fmt.Errorf("refusing to edit binary file")
	}
	return string(raw), nil
}

// limitedBuffer is an io.Writer that aborts once more than `limit` bytes have
// been written, so streaming a remote file never balloons memory past the cap.
type limitedBuffer struct {
	bytes.Buffer
	limit   int64
	tripped bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if int64(b.Buffer.Len()+len(p)) > b.limit {
		b.tripped = true
		return 0, fmt.Errorf("size limit exceeded")
	}
	return b.Buffer.Write(p)
}

// isBinary reports whether data looks like a binary file: any NUL byte in the
// first 8 KiB is a strong signal it is not text we should edit.
func isBinary(data []byte) bool {
	n := len(data)
	if n > 8192 {
		n = 8192
	}
	return bytes.IndexByte(data[:n], 0) >= 0
}

// onEditorLoaded installs the loaded content into a fresh textarea and switches
// the editor into the read-only highlighted view.
func (m filesModel) onEditorLoaded(msg editorLoadedMsg) (tea.Model, tea.Cmd) {
	// Ignore a stale load if the editor was closed or retargeted meanwhile.
	if m.editor == nil || !m.editor.active || m.editor.gen != msg.gen || m.editor.path != msg.path {
		return m, nil
	}
	if msg.err != nil {
		m.overlay = overlayNone
		m.editor = nil
		m.status = "open failed: " + msg.err.Error()
		return m, nil
	}

	ta, codec, why := newEditorArea(msg.content)
	ta.ShowLineNumbers = true
	ta.CharLimit = 0
	ta.MaxHeight = 0 // unbounded; we size it explicitly each frame
	ta.MaxWidth = 0
	ta.Prompt = ""
	styleEditorTextarea(&ta)
	w, h := m.editorAreaSize()
	ta.SetWidth(w)
	ta.SetHeight(h)

	m.editor.area = ta
	m.editor.codec = codec
	m.editor.readOnly = why
	m.editor.loaded = true
	m.editor.content = msg.content
	m.editor.viewLines = msg.lines
	m.editor.viewSrc = msg.content
	if m.editor.viewLines == nil {
		m.editor.viewLines = highlightLines(msg.name, msg.content)
	}
	m.editor.dirty = false
	m.editor.mode = editorView
	m.editor.viewScrl = 0
	m.editor.status = ""
	m.status = "editing " + msg.name
	if why != "" {
		m.editor.status = readOnlyStatus(why, msg.source)
		m.status = "viewing " + msg.name + " (read-only: " + why + ")"
	}
	return m, nil
}

// text returns the editor's current content as file text.
func (ed *editorState) text() string { return ed.codec.decode(ed.area.Value()) }

// styleEditorTextarea tints the textarea to match the file-manager palette.
func styleEditorTextarea(ta *textarea.Model) {
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.FocusedStyle.Text = lipgloss.NewStyle().Foreground(fmText)
	ta.FocusedStyle.LineNumber = lipgloss.NewStyle().Foreground(fmDimC)
	ta.FocusedStyle.CursorLineNumber = lipgloss.NewStyle().Foreground(fmAccent2)
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle().Background(fmColor("#0f1a24"))
	ta.BlurredStyle.Text = lipgloss.NewStyle().Foreground(fmMutedC)
	ta.BlurredStyle.LineNumber = lipgloss.NewStyle().Foreground(fmDimC)
	ta.Cursor.Style = lipgloss.NewStyle().Foreground(fmAccent2)
}

// editorAreaSize returns the (width, height) available to the editor body inside
// its bordered box, matching the dimensions used by the view.
func (m filesModel) editorAreaSize() (int, int) {
	w := m.width - 8
	if w < 24 {
		w = 24
	}
	h := m.height - 8
	if h < 6 {
		h = 6
	}
	return w, h
}

// editorBody returns how many text lines the read-only viewer/edit area body has.
func (m filesModel) editorBodyHeight() int {
	_, h := m.editorAreaSize()
	return h
}

// handleEditorKey routes keys while the editor overlay is open.
func (m filesModel) handleEditorKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ed := m.editor
	key := msg.String()
	if key != "esc" && ed.discardArmed {
		// Only two Esc presses in a row discard; anything between disarms.
		ed.discardArmed = false
		if ed.status == discardWarning {
			ed.status = ""
		}
	}
	switch key {
	case "esc":
		if !ed.dirty || ed.discardArmed {
			return m.closeEditor()
		}
		// Unsaved edits: the first Esc only warns, from either mode (leaving
		// edit mode for the viewer), and arms the discard. A second Esc in a
		// row closes without saving.
		if ed.mode == editorEdit {
			ed.area.Blur()
			ed.mode = editorView
			if v := ed.text(); v != ed.viewSrc {
				ed.viewLines = highlightLines(ed.name, v)
				ed.viewSrc = v
			}
			m.clampEditorScroll()
		}
		ed.discardArmed = true
		ed.status = discardWarning
		return m, nil
	case "ctrl+s":
		return m.saveEditor()
	case "ctrl+e", "tab":
		return m.toggleEditorMode()
	}

	if ed.mode == editorView {
		// Read-only viewer: arrow/page keys scroll the highlighted content.
		switch msg.String() {
		case "up", "k":
			ed.viewScrl--
		case "down", "j":
			ed.viewScrl++
		case "pgup":
			ed.viewScrl -= m.editorBodyHeight()
		case "pgdown":
			ed.viewScrl += m.editorBodyHeight()
		case "home", "g":
			ed.viewScrl = 0
		case "end", "G":
			ed.viewScrl = 1 << 30
		}
		m.clampEditorScroll()
		return m, nil
	}

	// Edit mode: forward to the textarea.
	var cmd tea.Cmd
	ed.area, cmd = ed.area.Update(msg)
	ed.dirty = ed.text() != ed.content
	return m, cmd
}

// clampEditorScroll keeps the read-only viewer scroll within the content.
func (m *filesModel) clampEditorScroll() {
	ed := m.editor
	if ed == nil {
		return
	}
	total := len(ed.viewLines)
	if total == 0 {
		total = strings.Count(ed.viewSrc, "\n") + 1
	}
	maxTop := total - m.editorBodyHeight()
	if maxTop < 0 {
		maxTop = 0
	}
	if ed.viewScrl > maxTop {
		ed.viewScrl = maxTop
	}
	if ed.viewScrl < 0 {
		ed.viewScrl = 0
	}
}

// toggleEditorMode flips between the highlighted viewer and the editable
// textarea, focusing/blurring the textarea accordingly.
func (m filesModel) toggleEditorMode() (tea.Model, tea.Cmd) {
	ed := m.editor
	if ed.mode == editorView {
		if !ed.loaded {
			return m, nil
		}
		if ed.readOnly != "" {
			ed.status = readOnlyStatus(ed.readOnly, ed.source)
			return m, nil
		}
		ed.mode = editorEdit
		ed.status = ""
		w, h := m.editorAreaSize()
		ed.area.SetWidth(w)
		ed.area.SetHeight(h)
		return m, ed.area.Focus()
	}
	ed.mode = editorView
	ed.area.Blur()
	if v := ed.text(); v != ed.viewSrc {
		ed.viewLines = highlightLines(ed.name, v)
		ed.viewSrc = v
	}
	m.clampEditorScroll()
	return m, nil
}

// closeEditor tears down the editor overlay without saving.
func (m filesModel) closeEditor() (tea.Model, tea.Cmd) {
	m.overlay = overlayNone
	m.editor = nil
	m.status = "closed editor"
	return m, nil
}

// saveEditor writes the current content back to its source (local file or
// remote upload) off the UI thread, then refreshes the owning pane.
func (m filesModel) saveEditor() (tea.Model, tea.Cmd) {
	ed := m.editor
	if ed.saving || !ed.loaded {
		return m, nil
	}
	if ed.readOnly != "" {
		ed.status = readOnlyStatus(ed.readOnly, ed.source)
		return m, nil
	}
	content := ed.text()
	ed.saving = true
	ed.status = "saving…"
	app := m.app
	source := ed.source
	path := ed.path
	base := ed.content
	inPlace := ed.inPlace
	gen, side, name := ed.gen, ed.side, ed.name
	return m, func() tea.Msg {
		err := saveFileFromEdit(app, source, path, []byte(content), []byte(base), inPlace)
		return editorSavedMsg{gen: gen, side: side, name: name, err: err, content: content}
	}
}

// saveFileFromEdit persists edited content. Local writes go through
// saveLocalEdit. Remote writes use the agent's safe in-place edit: the file
// keeps its owner, group, mode and extended attributes, is replaced
// atomically, and is only replaced if it still has the content the editor
// opened (base) — a change made on the server meanwhile is reported instead
// of overwritten. Agents too old for that get the checksummed atomic upload
// instead.
//
// inPlace writes the remote file in place instead (see core.EditRequest).
func saveFileFromEdit(app *core.App, source, full string, content, base []byte, inPlace bool) error {
	if source == "" {
		return saveLocalEdit(full, content, base)
	}
	_, err := app.EditRemoteFile(source, core.EditRequest{Path: full, Replace: true, Content: content, BaseSHA256: sha256Hex(base), InPlace: inPlace})
	if err == nil {
		return nil
	}
	if core.EditErrorCode(err) == "edit_conflict" {
		return fmt.Errorf("the file changed on %s since you opened it; your text is still here — copy what you need, then reopen the file", source)
	}
	if !errors.Is(err, core.ErrEditUnsupported) {
		return err
	}
	tmp, err := os.CreateTemp("", "fleet-edit-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if _, err := app.UploadFile(source, tmpName, full, core.FileTransferOptions{}, nil); err != nil {
		return err
	}
	return nil
}

// saveLocalEdit replaces the local file at full with content, atomically, and
// only while it still holds base (what the editor opened).
//
// Like the agent's remote edit, symlinks in the path are resolved first: an
// edit through a link changes the file it points to and leaves the link in
// place. The resolved entry is then inspected without following links, its
// bytes are hashed and compared with base, and content is written to a
// sidecar in the same folder that is renamed over it with the file's owner,
// group and mode. A file that is missing, no longer regular, or has
// different bytes is left alone and the save is refused.
//
// Replacing a file is not writing it, so the save is also refused where an
// ordinary write would have been, or where the replacement would differ in
// more than content: a file the user may not write, one with other hard
// links (they would keep the old text), or one whose owner cannot be kept.
// Extended attributes and ACLs are not carried over. Saving unchanged text
// writes nothing.
func saveLocalEdit(full string, content, base []byte) error {
	const keep = "; your text is still here — copy what you need, then reopen the file"
	target, err := filepath.EvalSymlinks(full)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("the file was removed since you opened it" + keep)
	}
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer root.Close()
	name := filepath.Base(target)
	before, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("the file was removed since you opened it" + keep)
	}
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return errors.New("the file is no longer a regular file" + keep)
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	// Reading one byte past base is enough to tell a longer file apart.
	current, readErr := io.ReadAll(io.LimitReader(f, int64(len(base))+1))
	opened, statErr := f.Stat()
	_ = f.Close()
	if readErr != nil {
		return readErr
	}
	if statErr != nil {
		return statErr
	}
	if !os.SameFile(before, opened) || sha256Hex(current) != sha256Hex(base) {
		return errors.New("the file changed on disk since you opened it" + keep)
	}
	if bytes.Equal(content, base) {
		return nil // nothing to change: leave the file (and its inode) alone
	}
	if n := localLinkCount(before); n > 1 {
		return fmt.Errorf("the file has %d hard links and replacing it would split them apart, so it was left unchanged — edit it in $EDITOR", n)
	}
	if err := checkLocalWritable(root, name); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return errors.New("you may not write this file; nothing was saved")
		}
		return err
	}

	mode := before.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	out, err := core.CreateAtomicLocalFile(target, mode.Perm())
	if err != nil {
		return err
	}
	defer out.Abort() // a no-op once committed
	if _, err := out.File().Write(content); err != nil {
		return err
	}
	if err := keepLocalOwner(out.File(), before); err != nil {
		return fmt.Errorf("cannot give the saved file its owner and group (%v), so it was left unchanged — edit it in $EDITOR", err)
	}
	if err := out.File().Chmod(mode); err != nil {
		return err
	}
	return out.Commit()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// onEditorSaved updates editor state after a save completes and refreshes the
// owning pane so the new size/mtime show immediately.
func (m filesModel) onEditorSaved(msg editorSavedMsg) (tea.Model, tea.Cmd) {
	ed := m.editor
	if ed == nil || !ed.active || ed.gen != msg.gen {
		// The editor session that saved was closed while the save was in
		// flight (another file, or this one again, may be open now). Report
		// the outcome without touching the current session.
		if msg.err != nil {
			m.setStatus(levelError, "save of "+msg.name+" failed: "+msg.err.Error())
			return m, nil
		}
		m.setStatus(levelOK, "saved "+msg.name)
		return m, m.reload(msg.side)
	}
	ed.saving = false
	if msg.err != nil && core.EditErrorCode(msg.err) == "cannot_replace" && !ed.inPlace {
		ed.inPlace = true
		ed.status = "can't replace this file — ^s again writes it in place"
		m.status = "not saved: " + msg.err.Error()
		return m, nil
	}
	if msg.err != nil {
		ed.status = "save failed: " + msg.err.Error()
		m.status = "save failed: " + msg.err.Error()
		return m, nil
	}
	// What was saved is the new base; text typed while the save ran is
	// still unsaved.
	ed.content = msg.content
	ed.dirty = ed.text() != msg.content
	ed.status = "saved ✓"
	m.status = "saved " + ed.name
	return m, m.reload(ed.side)
}
