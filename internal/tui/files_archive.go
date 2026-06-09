// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cenvero/fleet/internal/core"
	tea "github.com/charmbracelet/bubbletea"
)

// ============================================================================
// Archive / permission / checksum / duplicate operations
//
// These reuse the existing overlay/prompt machinery and run their heavy I/O off
// the Bubble Tea update loop via tea.Cmd so the UI never blocks. All of them
// resolve a pane's source ("" for Local) and absolute path the same way the rest
// of the file manager does (joinPath honours remote vs local separators).
// ============================================================================

// archiveExts are the file suffixes recognised as extractable archives. They are
// matched case-insensitively against the full name so multi-part suffixes like
// ".tar.gz" work without special-casing.
var archiveExts = []string{
	".zip", ".tar", ".tar.gz", ".tgz", ".tar.bz2", ".tbz2",
	".tar.xz", ".txz", ".tar.zst", ".tar.lz", ".gz", ".bz2", ".xz",
}

// isArchiveName reports whether a file name looks like an archive we can extract.
func isArchiveName(name string) bool {
	low := strings.ToLower(name)
	for _, ext := range archiveExts {
		if strings.HasSuffix(low, ext) {
			return true
		}
	}
	return false
}

// ============================================================================
// Compress (archive a selection)
// ============================================================================

// openCompress opens the format/name overlay for the current selection (the
// multi-selection if any, else the focused row). The default archive name is
// derived from the selection and the default format.
func (m filesModel) openCompress(side int) filesModel {
	items := m.selectionItems(side)
	if len(items) == 0 {
		m.status = "select item(s) to compress"
		return m
	}
	names := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, it.name)
	}
	m.overlay = overlayCompress
	m.compressSide = side
	m.compressNames = names
	m.compressFormat = 0
	m.compressEditing = false
	m.compressName = defaultArchiveName(names, core.ArchiveFormats()[0])
	return m
}

// defaultArchiveName proposes an archive base name for the given selection and
// format, reusing core.SuggestArchiveName so local and server panes agree.
func defaultArchiveName(names []string, format string) string {
	return core.SuggestArchiveName(names, format)
}

// cycleCompressFormat advances the chosen format and, while the user hasn't
// hand-edited the name, re-derives the default name so its extension tracks the
// format (e.g. archive.zip → archive.tar.gz).
func (m *filesModel) cycleCompressFormat(delta int) {
	formats := core.ArchiveFormats()
	n := len(formats)
	m.compressFormat = ((m.compressFormat+delta)%n + n) % n
	if !m.compressEditing {
		m.compressName = defaultArchiveName(m.compressNames, formats[m.compressFormat])
	}
}

// submitCompress kicks off the archive build off the UI thread and refreshes the
// pane when it completes.
func (m filesModel) submitCompress() (tea.Model, tea.Cmd) {
	side := m.compressSide
	formats := core.ArchiveFormats()
	format := formats[m.compressFormat]
	name := strings.TrimSpace(m.compressName)
	names := append([]string(nil), m.compressNames...)
	m.overlay = overlayNone
	if name == "" {
		m.status = "cancelled"
		return m, nil
	}
	if strings.ContainsAny(name, "/\\") {
		m.status = "invalid archive name"
		return m, nil
	}
	pane := m.paneRefConst(side)
	app := m.app
	source := pane.source
	dir := pane.cwd
	m.status = fmt.Sprintf("compressing %d item(s) → %s…", len(names), name)
	return m, func() tea.Msg {
		err := app.CompressPaths(source, dir, names, name, format)
		return fileOpDoneMsg{side: side, verb: "compressed", what: name, err: err}
	}
}

// ============================================================================
// Extract (an archive file)
// ============================================================================

// extractFocused extracts the focused archive into its containing directory,
// off the UI thread. It refuses non-archives and directories.
func (m filesModel) extractFocused(side int) (tea.Model, tea.Cmd) {
	pane := m.paneRefConst(side)
	it := m.focusedItem(side)
	if it.name == "" || it.name == ".." {
		m.status = "select an archive to extract"
		return m, nil
	}
	if it.isDir {
		m.status = "cannot extract a directory"
		return m, nil
	}
	if !isArchiveName(it.name) {
		m.status = it.name + " is not a recognised archive"
		return m, nil
	}
	full := joinPath(pane.cwd, it.name, pane.remote)
	app := m.app
	source := pane.source
	m.status = "extracting " + it.name + "…"
	return m, func() tea.Msg {
		err := app.ExtractArchive(source, full)
		return fileOpDoneMsg{side: side, verb: "extracted", what: it.name, err: err}
	}
}

// ============================================================================
// Permissions (chmod) prompt
// ============================================================================

// openChmodPrompt opens a text prompt seeded with the focused item's current
// octal mode (e.g. "755"), reusing the shared prompt overlay.
func (m filesModel) openChmodPrompt(side int) filesModel {
	it := m.focusedItem(side)
	if it.name == "" || it.name == ".." {
		m.status = "select an item to change permissions"
		return m
	}
	m.overlay = overlayPrompt
	m.prompt = promptChmod
	m.promptSide = side
	m.promptItem = it
	m.promptLabel = "Permissions for '" + it.name + "' (octal, e.g. 755)"
	m.promptValue = octalModeString(it.mode)
	return m
}

// octalModeString renders the permission bits of a mode as a 3- or 4-digit octal
// string suitable for a chmod prompt default.
func octalModeString(mode uint32) string {
	perm := os.FileMode(mode).Perm()
	s := fmt.Sprintf("%o", uint32(perm))
	if len(s) < 3 {
		s = strings.Repeat("0", 3-len(s)) + s
	}
	return s
}

// runChmod applies an octal mode to the prompt's item via core.ChmodPath, then
// reloads the pane. It is called from submitPrompt for promptChmod.
func (m filesModel) runChmod(side int, name, mode string) (tea.Model, tea.Cmd) {
	if _, err := strconv.ParseUint(mode, 8, 32); err != nil {
		m.status = "invalid octal mode: " + mode
		return m, nil
	}
	pane := m.paneRefConst(side)
	full := joinPath(pane.cwd, name, pane.remote)
	if err := m.app.ChmodPath(pane.source, full, mode); err != nil {
		m.status = "chmod failed: " + err.Error()
		return m, nil
	}
	m.status = fmt.Sprintf("set %s on %s", mode, name)
	return m, m.reload(side)
}

// ============================================================================
// Checksum (SHA-256)
// ============================================================================

// checksumFocused computes the SHA-256 of the focused file off the UI thread.
// The result is shown in the properties overlay (copyable) and the status line.
func (m filesModel) checksumFocused(side int) (tea.Model, tea.Cmd) {
	pane := m.paneRefConst(side)
	it := m.focusedItem(side)
	if it.name == "" || it.name == ".." {
		m.status = "select a file to checksum"
		return m, nil
	}
	if it.isDir {
		m.status = "cannot checksum a directory"
		return m, nil
	}
	full := joinPath(pane.cwd, it.name, pane.remote)
	app := m.app
	source := pane.source
	m.status = "computing SHA-256 of " + it.name + "…"
	return m, func() tea.Msg {
		sum, err := app.ChecksumPath(source, full)
		return checksumDoneMsg{side: side, name: it.name, path: full, sum: sum, err: err}
	}
}

// onChecksumDone surfaces a completed checksum in the properties overlay (so the
// hash is on-screen to copy) and the status line.
func (m filesModel) onChecksumDone(msg checksumDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.status = "checksum failed: " + msg.err.Error()
		return m, nil
	}
	lines := []string{
		"Name:    " + msg.name,
		"Path:    " + msg.path,
		"SHA-256: " + msg.sum,
	}
	m.overlay = overlayProperties
	m.propsText = strings.Join(lines, "\n")
	m.status = "SHA-256 " + msg.name + ": " + msg.sum
	return m, nil
}

// ============================================================================
// Duplicate (copy a file to a "<name> copy.<ext>" sibling)
// ============================================================================

// duplicateFocused copies the focused file to a uniquely-named sibling in the
// same pane. For a server pane it relays through core.CopyFile (same src/dst
// server); for Local it does a local stream copy. Runs off the UI thread.
func (m filesModel) duplicateFocused(side int) (tea.Model, tea.Cmd) {
	pane := m.paneRefConst(side)
	it := m.focusedItem(side)
	if it.name == "" || it.name == ".." {
		m.status = "select a file to duplicate"
		return m, nil
	}
	if it.isDir {
		m.status = "cannot duplicate a directory"
		return m, nil
	}
	existing := make(map[string]bool, len(pane.entries))
	for _, e := range pane.entries {
		existing[e.name] = true
	}
	dupName := duplicateName(it.name, existing)
	src := joinPath(pane.cwd, it.name, pane.remote)
	dst := joinPath(pane.cwd, dupName, pane.remote)
	app := m.app
	source := pane.source
	m.status = "duplicating " + it.name + "…"
	return m, func() tea.Msg {
		var err error
		if source == "" {
			err = copyLocalFile(src, dst)
		} else {
			_, err = app.CopyFile(source, src, source, dst, core.FileTransferOptions{}, nil)
		}
		return fileOpDoneMsg{side: side, verb: "duplicated", what: dupName, err: err}
	}
}

// duplicateName builds a Finder-style "<base> copy.<ext>" sibling name, bumping a
// numeric suffix ("<base> copy 2.<ext>", …) until it doesn't collide with any
// existing name in the directory.
func duplicateName(name string, existing map[string]bool) string {
	ext := filepath.Ext(name)
	// Treat multi-part archive suffixes (.tar.gz) as a single extension so the
	// "copy" tag lands before the whole suffix.
	low := strings.ToLower(name)
	for _, multi := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst"} {
		if strings.HasSuffix(low, multi) {
			ext = name[len(name)-len(multi):]
			break
		}
	}
	base := name[:len(name)-len(ext)]
	candidate := base + " copy" + ext
	if !existing[candidate] {
		return candidate
	}
	for i := 2; ; i++ {
		candidate = fmt.Sprintf("%s copy %d%s", base, i, ext)
		if !existing[candidate] {
			return candidate
		}
	}
}

// copyLocalFile copies a single local file's contents (and permission bits) to a
// new path, used by Duplicate for the Local pane.
func copyLocalFile(src, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- operator-selected path
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// ============================================================================
// Shared completion handler
// ============================================================================

// onFileOpDone reports a finished compress/extract/duplicate op and refreshes
// the owning pane so the new file shows immediately.
func (m filesModel) onFileOpDone(msg fileOpDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.status = msg.verb + " failed: " + msg.err.Error()
		return m, nil
	}
	m.status = msg.verb + " " + msg.what
	return m, m.reload(msg.side)
}
