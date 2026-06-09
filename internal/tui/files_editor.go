// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"bytes"
	"fmt"
	"os"
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
	active   bool
	side     int    // the pane the file belongs to (for refresh + source)
	source   string // "" = local, else server name
	path     string // absolute path of the file being edited
	name     string // base name (drives the lexer + the header)
	mode     editorMode
	area     textarea.Model
	content  string // last-saved content (for dirty detection)
	dirty    bool
	saving   bool
	status   string // inline message inside the editor footer (errors, hints)
	viewScrl int    // scroll offset (top line) for the read-only highlighted view
}

// editorLoadedMsg carries the result of loading a file into the editor.
type editorLoadedMsg struct {
	side    int
	source  string
	path    string
	name    string
	content string
	err     error
}

// editorSavedMsg carries the result of an asynchronous save.
type editorSavedMsg struct {
	err error
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
	full := joinPath(pane.cwd, it.name, pane.remote)
	m.overlay = overlayEditor
	m.editor = editorState{
		active: true, side: side, source: pane.source,
		path: full, name: it.name, mode: editorView,
		status: "loading…",
	}
	m.status = "opening " + it.name + "…"
	return m, m.editorLoadCmd(side, pane.source, full, it.name)
}

// editorLoadCmd reads the file content (local or remote) into memory, enforcing
// the size cap and rejecting binary content (detected via NUL bytes).
func (m filesModel) editorLoadCmd(side int, source, full, name string) tea.Cmd {
	app := m.app
	return func() tea.Msg {
		data, err := loadFileForEdit(app, source, full)
		return editorLoadedMsg{side: side, source: source, path: full, name: name, content: data, err: err}
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
	if !m.editor.active || m.editor.path != msg.path {
		return m, nil
	}
	if msg.err != nil {
		m.overlay = overlayNone
		m.editor = editorState{}
		m.status = "open failed: " + msg.err.Error()
		return m, nil
	}

	ta := textarea.New()
	ta.SetValue(msg.content)
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
	m.editor.content = msg.content
	m.editor.dirty = false
	m.editor.mode = editorView
	m.editor.viewScrl = 0
	m.editor.status = ""
	m.status = "editing " + msg.name
	return m, nil
}

// styleEditorTextarea tints the textarea to match the file-manager palette.
func styleEditorTextarea(ta *textarea.Model) {
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.FocusedStyle.Text = lipgloss.NewStyle().Foreground(fmText)
	ta.FocusedStyle.LineNumber = lipgloss.NewStyle().Foreground(fmDimC)
	ta.FocusedStyle.CursorLineNumber = lipgloss.NewStyle().Foreground(fmAccent2)
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle().Background(lipgloss.Color("#0f1a24"))
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
	ed := &m.editor
	switch msg.String() {
	case "esc":
		if ed.dirty && ed.mode == editorEdit {
			// First Esc out of a dirty edit returns to the viewer (a soft cancel);
			// a second Esc from the viewer closes. Keeps an accidental keystroke
			// from discarding edits silently.
			ed.area.Blur()
			ed.mode = editorView
			ed.status = "unsaved changes — Ctrl+S to save, Esc again to discard"
			return m, nil
		}
		return m.closeEditor()
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
	ed.dirty = ed.area.Value() != ed.content
	return m, cmd
}

// clampEditorScroll keeps the read-only viewer scroll within the content.
func (m *filesModel) clampEditorScroll() {
	ed := &m.editor
	total := strings.Count(ed.area.Value(), "\n") + 1
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
	ed := &m.editor
	if ed.mode == editorView {
		ed.mode = editorEdit
		ed.status = ""
		w, h := m.editorAreaSize()
		ed.area.SetWidth(w)
		ed.area.SetHeight(h)
		return m, ed.area.Focus()
	}
	ed.mode = editorView
	ed.area.Blur()
	m.clampEditorScroll()
	return m, nil
}

// closeEditor tears down the editor overlay without saving.
func (m filesModel) closeEditor() (tea.Model, tea.Cmd) {
	m.overlay = overlayNone
	m.editor = editorState{}
	m.status = "closed editor"
	return m, nil
}

// saveEditor writes the current content back to its source (local file or
// remote upload) off the UI thread, then refreshes the owning pane.
func (m filesModel) saveEditor() (tea.Model, tea.Cmd) {
	ed := &m.editor
	if ed.saving {
		return m, nil
	}
	content := ed.area.Value()
	ed.saving = true
	ed.status = "saving…"
	app := m.app
	source := ed.source
	path := ed.path
	return m, func() tea.Msg {
		err := saveFileFromEdit(app, source, path, []byte(content))
		return editorSavedMsg{err: err}
	}
}

// saveFileFromEdit persists edited content. Local writes go straight to the file
// (0o600); remote writes stage the content in a controller temp file and upload
// it to the exact remote path, then remove the temp.
func saveFileFromEdit(app *core.App, source, full string, content []byte) error {
	if source == "" {
		return os.WriteFile(full, content, 0o600)
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

// onEditorSaved updates editor state after a save completes and refreshes the
// owning pane so the new size/mtime show immediately.
func (m filesModel) onEditorSaved(msg editorSavedMsg) (tea.Model, tea.Cmd) {
	ed := &m.editor
	ed.saving = false
	if !ed.active {
		return m, nil
	}
	if msg.err != nil {
		ed.status = "save failed: " + msg.err.Error()
		m.status = "save failed: " + msg.err.Error()
		return m, nil
	}
	ed.content = ed.area.Value()
	ed.dirty = false
	ed.status = "saved ✓"
	m.status = "saved " + ed.name
	return m, m.reload(ed.side)
}
