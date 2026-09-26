// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/core"
	tea "github.com/charmbracelet/bubbletea"
	zone "github.com/lrstanley/bubblezone"
)

// ============================================================================
// Locations, history, recent folders and bookmarks
// ============================================================================

// fmLoc is a folder on a source ("" = Local).
type fmLoc struct {
	Source string `json:"source"`
	Path   string `json:"path"`
}

func (l fmLoc) label() string {
	if l.Source == "" {
		return "Local"
	}
	return l.Source
}

// navHistory is a browser-style back/forward stack for one pane.
type navHistory struct {
	back, fwd []fmLoc
}

const (
	maxHistory = 100
	maxRecent  = 20
)

func (m filesModel) locOf(side int) fmLoc {
	p := m.paneRefConst(side)
	return fmLoc{Source: p.source, Path: p.cwd}
}

// navigate moves a pane to path on its current source, remembering where it
// came from (for back/forward) and which entry to focus when the listing
// arrives (so going up lands on the folder you were in).
func (m *filesModel) navigate(side int, path, focusName string, record bool) tea.Cmd {
	pane := m.paneRef(side)
	path = pane.pathStyle.Clean(path)
	if record && path != pane.cwd {
		m.pushHistory(side, fmLoc{Source: pane.source, Path: pane.cwd})
	}
	pane.cwd = path
	pane.index, pane.scroll = 0, 0
	pane.loading = true
	pane.selected = map[int]bool{}
	pane.anchorSet, pane.visual = false, false
	pane.focusName = focusName
	pane.touch()
	return m.loadCmd(side, pane.source, path)
}

func (m *filesModel) pushHistory(side int, loc fmLoc) {
	h := &m.hist[side]
	if n := len(h.back); n > 0 && h.back[n-1] == loc {
		return
	}
	h.back = append(h.back, loc)
	if len(h.back) > maxHistory {
		h.back = h.back[len(h.back)-maxHistory:]
	}
	h.fwd = nil
}

// goLoc moves a pane to a location, switching its source when needed.
func (m *filesModel) goLoc(side int, loc fmLoc, record bool) tea.Cmd {
	pane := m.paneRef(side)
	if loc.Source == pane.source {
		return m.navigate(side, loc.Path, "", record)
	}
	if record {
		m.pushHistory(side, fmLoc{Source: pane.source, Path: pane.cwd})
	}
	next := m.newPane(loc.Source, *pane)
	if loc.Path != "" && withinRoot(next, loc.Path) {
		next.cwd = next.pathStyle.Clean(loc.Path)
	}
	*pane = next
	return m.loadCmd(side, pane.source, pane.cwd)
}

// newPane builds a pane for source, carrying over the old pane's sort/view
// preferences and restoring the last folder visited on that source during
// this session.
func (m *filesModel) newPane(source string, old paneState) paneState {
	var p paneState
	if m.app != nil {
		p = newPaneSource(m.app, source)
	} else {
		style := core.NativePathStyle()
		if source != "" {
			style = core.TargetPathPOSIX
		}
		p = paneState{source: source, remote: source != "", pathStyle: style,
			root: style.DefaultRoot(), cwd: style.DefaultRoot(), loading: true, selected: map[int]bool{}}
	}
	p.home = p.cwd
	if last, ok := m.lastPath[source]; ok && withinRoot(p, last) {
		p.cwd = last
	}
	p.sortBy, p.sortDesc, p.view = old.sortBy, old.sortDesc, old.view
	return p
}

// withinRoot reports whether path is at or below the pane's browse root.
func withinRoot(p paneState, path string) bool {
	if !p.pathStyle.IsAbs(path) {
		return false
	}
	root := paneBrowseRoot(&p)
	_, err := p.pathStyle.Relative(root, p.pathStyle.Clean(path))
	return err == nil
}

func (m filesModel) historyBack(side int) (tea.Model, tea.Cmd) {
	h := &m.hist[side]
	if len(h.back) == 0 {
		m.setStatus(levelInfo, "no earlier folder in this pane's history")
		return m, nil
	}
	loc := h.back[len(h.back)-1]
	h.back = h.back[:len(h.back)-1]
	h.fwd = append(h.fwd, m.locOf(side))
	return m, m.goLoc(side, loc, false)
}

func (m filesModel) historyForward(side int) (tea.Model, tea.Cmd) {
	h := &m.hist[side]
	if len(h.fwd) == 0 {
		m.setStatus(levelInfo, "no later folder in this pane's history")
		return m, nil
	}
	loc := h.fwd[len(h.fwd)-1]
	h.fwd = h.fwd[:len(h.fwd)-1]
	h.back = append(h.back, m.locOf(side))
	return m, m.goLoc(side, loc, false)
}

// rememberVisit records a successfully listed folder in the session's recent
// list and the per-source last path.
func (m *filesModel) rememberVisit(side int) {
	pane := m.paneRefConst(side)
	if m.lastPath == nil {
		m.lastPath = map[string]string{}
	}
	m.lastPath[pane.source] = pane.cwd
	loc := fmLoc{Source: pane.source, Path: pane.cwd}
	out := []fmLoc{loc}
	for _, r := range m.recent {
		if r != loc {
			out = append(out, r)
		}
	}
	if len(out) > maxRecent {
		out = out[:maxRecent]
	}
	m.recent = out
}

// goHome jumps to the local home directory, or the server's initial folder
// (its configured remote dir or advertised file root).
func (m filesModel) goHome(side int) (tea.Model, tea.Cmd) {
	pane := m.paneRef(side)
	home := pane.home
	if !pane.remote {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	if home == "" || !withinRoot(*pane, home) {
		home = paneBrowseRoot(pane)
	}
	if home == pane.cwd && pane.err == nil {
		m.setStatus(levelInfo, "already at "+home)
		return m, nil
	}
	return m, m.navigate(side, home, "", true)
}

// ---- bookmarks ----

func (m filesModel) toggleBookmark(side int) filesModel {
	loc := m.locOf(side)
	for i, b := range m.bookmarks {
		if b == loc {
			m.bookmarks = append(append([]fmLoc{}, m.bookmarks[:i]...), m.bookmarks[i+1:]...)
			m.saveBookmarks()
			m.setStatus(levelInfo, "removed bookmark "+loc.label()+":"+loc.Path)
			return m
		}
	}
	m.bookmarks = append(m.bookmarks, loc)
	m.saveBookmarks()
	m.setStatus(levelOK, "bookmarked "+loc.label()+":"+loc.Path+" — ' to open places")
	return m
}

// bookmarksFile is where bookmarks persist between sessions.
func bookmarksFile(configDir string) string {
	if configDir == "" {
		return ""
	}
	return filepath.Join(configDir, "tui", "files-bookmarks.json")
}

func loadBookmarks(configDir string) []fmLoc {
	path := bookmarksFile(configDir)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path) // #nosec G304 -- fixed file under the operator's config dir
	if err != nil {
		return nil
	}
	var doc struct {
		Bookmarks []fmLoc `json:"bookmarks"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	out := doc.Bookmarks[:0]
	for _, b := range doc.Bookmarks {
		if b.Path != "" && len(out) < 200 {
			out = append(out, b)
		}
	}
	return out
}

// saveBookmarks persists bookmarks (best effort; 0700 dir, 0600 file).
func (m *filesModel) saveBookmarks() {
	if m.app == nil {
		return
	}
	path := bookmarksFile(m.app.ConfigDir)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		m.setStatus(levelWarn, "could not save bookmarks: "+err.Error())
		return
	}
	data, _ := json.MarshalIndent(struct {
		Bookmarks []fmLoc `json:"bookmarks"`
	}{m.bookmarks}, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		m.setStatus(levelWarn, "could not save bookmarks: "+err.Error())
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		m.setStatus(levelWarn, "could not save bookmarks: "+err.Error())
	}
}

// ---- places overlay (bookmarks + recent) ----

type fmPlace struct {
	loc      fmLoc
	bookmark bool
	section  string // header text for the first item of a section
}

func (m filesModel) placesList() []fmPlace {
	var out []fmPlace
	for i, b := range m.bookmarks {
		pl := fmPlace{loc: b, bookmark: true}
		if i == 0 {
			pl.section = "Bookmarks"
		}
		out = append(out, pl)
	}
	first := true
	for _, r := range m.recent {
		dup := false
		for _, b := range m.bookmarks {
			if b == r {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		pl := fmPlace{loc: r}
		if first {
			pl.section = "Recent"
			first = false
		}
		out = append(out, pl)
	}
	return out
}

func (m filesModel) openPlaces() filesModel {
	m.overlay = overlayPlaces
	m.placesItems = m.placesList()
	m.placesIndex = 0
	return m
}

func (m filesModel) handlePlacesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "'":
		m.overlay = overlayNone
	case "up", "k":
		if m.placesIndex > 0 {
			m.placesIndex--
		}
	case "down", "j":
		if m.placesIndex < len(m.placesItems)-1 {
			m.placesIndex++
		}
	case "enter", "l", "right":
		return m.choosePlace(m.placesIndex)
	case "d", "delete", "x":
		if m.placesIndex < len(m.placesItems) && m.placesItems[m.placesIndex].bookmark {
			loc := m.placesItems[m.placesIndex].loc
			for i, b := range m.bookmarks {
				if b == loc {
					m.bookmarks = append(append([]fmLoc{}, m.bookmarks[:i]...), m.bookmarks[i+1:]...)
					break
				}
			}
			m.saveBookmarks()
			m.placesItems = m.placesList()
			if m.placesIndex >= len(m.placesItems) {
				m.placesIndex = len(m.placesItems) - 1
			}
			if m.placesIndex < 0 {
				m.placesIndex = 0
			}
		}
	}
	return m, nil
}

func (m filesModel) choosePlace(i int) (tea.Model, tea.Cmd) {
	m.overlay = overlayNone
	if i < 0 || i >= len(m.placesItems) {
		return m, nil
	}
	loc := m.placesItems[i].loc
	if loc.Source != "" && !m.serverKnown(loc.Source) {
		m.setStatus(levelError, "unknown server "+loc.Source)
		return m, nil
	}
	return m, m.goLoc(m.focus, loc, true)
}

func (m filesModel) serverKnown(name string) bool {
	if m.app == nil {
		return true
	}
	for _, s := range m.servers {
		if s.Name == name {
			return true
		}
	}
	return false
}

func (m filesModel) renderPlaces() string {
	dw := m.dialogWidth(64)
	var b strings.Builder
	b.WriteString(fmTitleSty.Render("Places"))
	b.WriteString(fmDimSty.Render("  → " + sideName(m.focus) + " pane"))
	b.WriteString("\n")
	if len(m.placesItems) == 0 {
		b.WriteString("\n")
		b.WriteString(fmStatusSty.Render("No bookmarks or recent folders yet."))
		b.WriteString("\n")
		b.WriteString(fmDimSty.Render("Press b in a folder to bookmark it."))
		b.WriteString("\n\n")
		b.WriteString(fmStatusSty.Render("esc close"))
		return fmOverlayBox.Render(b.String())
	}
	maxRows := m.height - 12
	if maxRows < 3 {
		maxRows = 3
	}
	start := 0
	if m.placesIndex >= maxRows {
		start = m.placesIndex - maxRows + 1
	}
	for i := start; i < len(m.placesItems) && i < start+maxRows; i++ {
		pl := m.placesItems[i]
		if pl.section != "" {
			b.WriteString("\n")
			b.WriteString(fmDimSty.Render(pl.section))
			b.WriteString("\n")
		}
		star := "  "
		if pl.bookmark {
			star = "★ "
		}
		src := sourceGlyph(pl.loc.Source != "") + " " + fmPadRight(fmSanitize(pl.loc.label()), 10)
		path := fmFitPathLeft(fmSanitize(pl.loc.Path), dw-fmWidth(src)-6)
		line := fmPadRight(" "+star+src+"  "+path, dw)
		if i == m.placesIndex {
			b.WriteString(zone.Mark(fmt.Sprintf("%s%d", fmPlacePrefix, i), fmSelRow.Render(line)))
		} else {
			b.WriteString(zone.Mark(fmt.Sprintf("%s%d", fmPlacePrefix, i), fmTextSty.Render(line)))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(fmStatusSty.Render("↑↓ choose · ↵ open · d remove bookmark · esc close"))
	return fmOverlayBox.Render(b.String())
}

// ============================================================================
// Go to path
// ============================================================================

func (m filesModel) openGoto(side int) filesModel {
	pane := m.paneRefConst(side)
	m.overlay = overlayGoto
	m.gotoSide = side
	val := pane.cwd
	if !strings.HasSuffix(val, pane.pathStyle.Separator()) {
		val += pane.pathStyle.Separator()
	}
	m.gotoValue = val
	m.gotoErr = ""
	m.gotoIndex = -1
	m.gotoSugg = m.gotoCompletions(side, val)
	return m
}

// resolveGotoPath turns what the user typed into an absolute path for the
// pane's target: "~" means home (local) or the server's initial folder, and
// relative paths are taken from the current folder.
func (m filesModel) resolveGotoPath(side int, raw string) string {
	pane := m.paneRefConst(side)
	style := pane.pathStyle
	v := strings.TrimSpace(raw)
	if v == "" {
		return pane.cwd
	}
	if v == "~" || strings.HasPrefix(v, "~/") || strings.HasPrefix(v, `~\`) {
		home := pane.home
		if !pane.remote {
			if h, err := os.UserHomeDir(); err == nil {
				home = h
			}
		}
		if home == "" {
			home = paneBrowseRoot(&pane)
		}
		rest := strings.TrimLeft(v[1:], `/\`)
		if rest == "" {
			return style.Clean(home)
		}
		return style.Join(home, rest)
	}
	if !style.IsAbs(v) {
		return style.Join(pane.cwd, v)
	}
	return style.Clean(v)
}

// gotoCompletions lists folders that complete the typed path. Local panes read
// the parent folder (bounded); remote panes complete from the current listing
// when the typed parent is the pane's folder, so typing never blocks on the
// network.
func (m filesModel) gotoCompletions(side int, raw string) []string {
	pane := m.paneRefConst(side)
	style := pane.pathStyle
	sep := style.Separator()
	v := raw
	if v == "" {
		return nil
	}
	dir, prefix := v, ""
	if !strings.HasSuffix(v, sep) && !(style.IsWindows() && strings.HasSuffix(v, "/")) {
		abs := m.resolveGotoPath(side, v)
		dir, prefix = style.Dir(abs), style.Base(abs)
	} else {
		dir = m.resolveGotoPath(side, v)
	}
	lowPrefix := strings.ToLower(prefix)
	var names []string
	if pane.remote {
		if style.Clean(dir) != style.Clean(pane.cwd) {
			return nil
		}
		for _, it := range pane.allItems {
			if (it.isDir || it.linkDir) && strings.HasPrefix(strings.ToLower(it.name), lowPrefix) {
				names = append(names, it.name)
			}
		}
	} else {
		f, err := os.Open(dir) // #nosec G304 -- operator-typed local path, listing only
		if err != nil {
			return nil
		}
		entries, _ := f.ReadDir(2000)
		_ = f.Close()
		for _, e := range entries {
			n := e.Name()
			if strings.HasPrefix(n, ".") && !strings.HasPrefix(prefix, ".") && !m.showHidden {
				continue
			}
			isDir := e.IsDir()
			if !isDir && e.Type()&os.ModeSymlink != 0 {
				if st, err := os.Stat(filepath.Join(dir, n)); err == nil && st.IsDir() {
					isDir = true
				}
			}
			if isDir && strings.HasPrefix(strings.ToLower(n), lowPrefix) {
				names = append(names, n)
			}
		}
	}
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	if len(names) > 50 {
		names = names[:50]
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, style.Join(dir, n))
	}
	return out
}

func (m filesModel) handleGotoKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	side := m.gotoSide
	sep := m.paneRefConst(side).pathStyle.Separator()
	switch msg.String() {
	case "esc":
		m.overlay = overlayNone
		return m, nil
	case "enter":
		if m.gotoIndex >= 0 && m.gotoIndex < len(m.gotoSugg) {
			m.gotoValue = m.gotoSugg[m.gotoIndex]
		}
		return m.submitGoto()
	case "tab":
		switch {
		case m.gotoIndex >= 0 && m.gotoIndex < len(m.gotoSugg):
			m.gotoValue = m.gotoSugg[m.gotoIndex] + sep
		case len(m.gotoSugg) == 1:
			m.gotoValue = m.gotoSugg[0] + sep
		case len(m.gotoSugg) > 1:
			if cp := commonPrefix(m.gotoSugg); len(cp) > len(m.resolveGotoPath(side, m.gotoValue)) {
				m.gotoValue = cp
			}
		}
		m.gotoIndex = -1
	case "down", "ctrl+n":
		if m.gotoIndex < len(m.gotoSugg)-1 {
			m.gotoIndex++
		}
		return m, nil
	case "up", "ctrl+p":
		if m.gotoIndex >= 0 {
			m.gotoIndex--
		}
		return m, nil
	case "backspace":
		if r := []rune(m.gotoValue); len(r) > 0 {
			m.gotoValue = string(r[:len(r)-1])
		}
		m.gotoIndex = -1
	case "ctrl+u":
		m.gotoValue = ""
		m.gotoIndex = -1
	case "ctrl+w":
		v := strings.TrimRight(m.gotoValue, `/\`)
		if i := strings.LastIndexAny(v, `/\`); i >= 0 {
			m.gotoValue = v[:i+1]
		} else {
			m.gotoValue = ""
		}
		m.gotoIndex = -1
	default:
		switch {
		case msg.Type == tea.KeyRunes && len(msg.Runes) > 0:
			m.gotoValue += string(msg.Runes)
		case msg.Type == tea.KeySpace:
			m.gotoValue += " "
		default:
			return m, nil
		}
		m.gotoIndex = -1
	}
	m.gotoErr = ""
	m.gotoSugg = m.gotoCompletions(side, m.gotoValue)
	return m, nil
}

func commonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, p) && p != "" {
			_, size := lastRune(p)
			p = p[:len(p)-size]
		}
	}
	return p
}

func lastRune(s string) (rune, int) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] < 0x80 || s[i] >= 0xc0 {
			return rune(s[i]), len(s) - i
		}
	}
	return 0, len(s)
}

func (m filesModel) submitGoto() (tea.Model, tea.Cmd) {
	side := m.gotoSide
	pane := m.paneRefConst(side)
	target := m.resolveGotoPath(side, m.gotoValue)
	if err := core.ValidateTargetPath(pane.pathStyle, target); err != nil {
		m.gotoErr = "invalid path: " + err.Error()
		return m, nil
	}
	if !withinRoot(pane, target) {
		m.gotoErr = "outside this pane's browse root (" + paneBrowseRoot(&pane) + ")"
		return m, nil
	}
	focus := ""
	if !pane.remote {
		st, err := os.Stat(target)
		switch {
		case errors.Is(err, os.ErrNotExist):
			m.gotoErr = "no such folder: " + target
			return m, nil
		case err == nil && !st.IsDir():
			// A file: open its folder with the file focused.
			focus = filepath.Base(target)
			target = filepath.Dir(target)
		}
	}
	m.overlay = overlayNone
	return m, m.navigate(side, target, focus, true)
}

func (m filesModel) renderGoto() string {
	dw := m.dialogWidth(70)
	pane := m.paneRefConst(m.gotoSide)
	var b strings.Builder
	b.WriteString(fmTitleSty.Render("Go to folder"))
	b.WriteString(fmDimSty.Render("  " + sourceGlyph(pane.remote) + " " + fmSanitize(pane.label())))
	b.WriteString("\n\n")
	b.WriteString(renderInput(m.gotoValue, dw))
	b.WriteString("\n")
	if m.gotoErr != "" {
		for _, ln := range fmWrap(fmSanitize(m.gotoErr), dw) {
			b.WriteString(fmErrSty.Render(ln))
			b.WriteString("\n")
		}
	}
	maxSugg := 8
	if m.height < 30 {
		maxSugg = 4
	}
	start := 0
	if m.gotoIndex >= maxSugg {
		start = m.gotoIndex - maxSugg + 1
	}
	for i := start; i < len(m.gotoSugg) && i < start+maxSugg; i++ {
		s := " " + fmLocalGlyph + " " + fmFitPathLeft(fmSanitize(m.gotoSugg[i]), dw-4)
		s = strings.Replace(s, fmLocalGlyph, "▣", 1)
		if i == m.gotoIndex {
			b.WriteString(fmSelRow.Render(fmPadRight(s, dw)))
		} else {
			b.WriteString(fmDimSty.Render(fmPadRight(s, dw)))
		}
		b.WriteString("\n")
	}
	if n := len(m.gotoSugg) - start - maxSugg; n > 0 {
		b.WriteString(fmDimSty.Render(fmt.Sprintf("   … %d more", n)))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(fmStatusSty.Render("tab complete · ↑↓ pick · ↵ go · ~ home · esc cancel"))
	return fmOverlayBox.Render(b.String())
}

// ============================================================================
// Quick jump (fuzzy find in the current listing)
// ============================================================================

const maxJumpResults = 12

func (m filesModel) openJump(side int) filesModel {
	m.overlay = overlayJump
	m.jumpSide = side
	m.jumpQuery = ""
	m.jumpIndex = 0
	m.jumpResults = m.jumpMatches(side, "")
	return m
}

// jumpMatches ranks the listing against the query (fuzzy subsequence match,
// see fmFuzzyScore) and returns the best entry indices.
func (m filesModel) jumpMatches(side int, query string) []int {
	pane := m.paneRefConst(side)
	q := strings.ToLower(strings.TrimSpace(query))
	type scored struct{ idx, score int }
	best := make([]scored, 0, maxJumpResults+1)
	for i, it := range pane.entries {
		if it.name == ".." {
			continue
		}
		low := it.lowerName
		if low == "" {
			low = strings.ToLower(it.name)
		}
		s, ok := fmFuzzyScore(q, low)
		if !ok {
			continue
		}
		// Keep a small sorted top-N instead of sorting 50k matches.
		pos := len(best)
		for pos > 0 && best[pos-1].score < s {
			pos--
		}
		if pos >= maxJumpResults {
			continue
		}
		best = append(best, scored{})
		copy(best[pos+1:], best[pos:])
		best[pos] = scored{i, s}
		if len(best) > maxJumpResults {
			best = best[:maxJumpResults]
		}
	}
	out := make([]int, len(best))
	for i, s := range best {
		out[i] = s.idx
	}
	return out
}

func (m filesModel) handleJumpKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	side := m.jumpSide
	switch msg.String() {
	case "esc":
		m.overlay = overlayNone
		return m, nil
	case "enter", "tab":
		if m.jumpIndex >= 0 && m.jumpIndex < len(m.jumpResults) {
			pane := m.paneRef(side)
			pane.index = m.jumpResults[m.jumpIndex]
			m.clampScroll(side)
			m.focus = side
			m.overlay = overlayNone
			if msg.String() == "enter" && pane.entries[pane.index].isDir {
				// Enter on a folder result opens it — the common reason to jump.
				return m.activate(side)
			}
			return m, nil
		}
		m.overlay = overlayNone
		return m, nil
	case "down", "ctrl+n":
		if m.jumpIndex < len(m.jumpResults)-1 {
			m.jumpIndex++
		}
		return m, nil
	case "up", "ctrl+p":
		if m.jumpIndex > 0 {
			m.jumpIndex--
		}
		return m, nil
	case "backspace":
		if r := []rune(m.jumpQuery); len(r) > 0 {
			m.jumpQuery = string(r[:len(r)-1])
		}
	case "ctrl+u":
		m.jumpQuery = ""
	default:
		switch {
		case msg.Type == tea.KeyRunes && len(msg.Runes) > 0:
			m.jumpQuery += string(msg.Runes)
		case msg.Type == tea.KeySpace:
			m.jumpQuery += " "
		default:
			return m, nil
		}
	}
	m.jumpResults = m.jumpMatches(side, m.jumpQuery)
	m.jumpIndex = 0
	return m, nil
}

func (m filesModel) renderJump() string {
	dw := m.dialogWidth(60)
	pane := m.paneRefConst(m.jumpSide)
	var b strings.Builder
	b.WriteString(fmTitleSty.Render("Jump to"))
	b.WriteString(fmDimSty.Render(fmt.Sprintf("  %s · %s items", fmSanitize(pane.label()), groupThousands(int64(realCountFast(pane.entries))))))
	b.WriteString("\n\n")
	b.WriteString(renderInput(m.jumpQuery, dw))
	b.WriteString("\n\n")
	maxRows := maxJumpResults
	if m.height < 30 {
		maxRows = 5
	}
	if len(m.jumpResults) == 0 {
		b.WriteString(fmWarnSty.Render("  no matches"))
		b.WriteString("\n")
	}
	for i, idx := range m.jumpResults {
		if i >= maxRows {
			break
		}
		it := pane.entries[idx]
		glyph, kind := iconKindFor(it)
		name := fmSanitize(it.name)
		if it.isDir || it.linkDir {
			name += "/"
		}
		line := " " + glyph + " " + fmPadRight(fmFitName(name, dw-4), dw-3)
		if i == m.jumpIndex {
			b.WriteString(fmSelRow.Render(line))
		} else {
			b.WriteString(fmIconStyles()[kind].Render(" "+glyph) + fmTextSty.Render(" "+fmPadRight(fmFitName(name, dw-4), dw-3)))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(fmStatusSty.Render("type to search · ↑↓ pick · ↵ open · tab select · esc cancel"))
	return fmOverlayBox.Render(b.String())
}

// ============================================================================
// Mirrored navigation
// ============================================================================

// mirrorEnter makes the other pane follow into a same-named sub-folder.
func (m *filesModel) mirrorEnter(side int, name string) tea.Cmd {
	if !m.mirror {
		return nil
	}
	o := m.other(side)
	other := m.paneRefConst(o)
	for _, it := range other.allItems {
		if it.name == name && (it.isDir || it.linkDir) {
			return m.navigate(o, joinPath(other.cwd, name, other.pathStyle), "", true)
		}
	}
	m.setStatus(levelWarn, fmt.Sprintf("mirror: %s has no folder %q", other.label(), name))
	return nil
}

// mirrorParent makes the other pane follow up one level.
func (m *filesModel) mirrorParent(side int) tea.Cmd {
	if !m.mirror {
		return nil
	}
	o := m.other(side)
	other := m.paneRef(o)
	root := paneBrowseRoot(other)
	parent := parentDir(other.cwd, other.pathStyle)
	if _, err := other.pathStyle.Relative(root, parent); err != nil || parent == other.cwd {
		return nil
	}
	return m.navigate(o, parent, other.pathStyle.Base(other.cwd), true)
}

// ============================================================================
// Selection helpers
// ============================================================================

func (m *filesModel) selectAll(side int) {
	pane := m.paneRef(side)
	pane.selected = make(map[int]bool, len(pane.entries))
	for i, it := range pane.entries {
		if it.name != ".." {
			pane.selected[i] = true
		}
	}
	pane.touch()
	m.setStatus(levelInfo, fmt.Sprintf("selected %d item(s)", len(pane.selected)))
}

func (m *filesModel) invertSelection(side int) {
	pane := m.paneRef(side)
	next := make(map[int]bool, len(pane.entries))
	for i, it := range pane.entries {
		if it.name != ".." && !pane.selected[i] {
			next[i] = true
		}
	}
	pane.selected = next
	pane.touch()
	m.setStatus(levelInfo, fmt.Sprintf("%d item(s) selected", len(next)))
}

func (m *filesModel) clearSelection(side int) bool {
	pane := m.paneRef(side)
	if len(pane.selected) == 0 && !pane.visual {
		return false
	}
	pane.selected = map[int]bool{}
	pane.anchorSet, pane.visual = false, false
	pane.touch()
	return true
}

// extendRange moves the cursor by delta and selects everything between the
// range anchor and the new cursor (on top of whatever was selected before the
// range started), like shift+arrows in a desktop file manager.
func (m *filesModel) extendRange(side, delta int) {
	pane := m.paneRef(side)
	if len(pane.entries) == 0 {
		return
	}
	if !pane.anchorSet {
		pane.anchorSet = true
		pane.anchor = pane.index
		pane.rangeBase = make(map[int]bool, len(pane.selected))
		for k, v := range pane.selected {
			pane.rangeBase[k] = v
		}
	}
	m.movePane(side, delta)
	m.applyRange(side)
}

func (m *filesModel) applyRange(side int) {
	pane := m.paneRef(side)
	lo, hi := pane.anchor, pane.index
	if lo > hi {
		lo, hi = hi, lo
	}
	sel := make(map[int]bool, len(pane.rangeBase)+hi-lo+1)
	for k, v := range pane.rangeBase {
		if v {
			sel[k] = true
		}
	}
	for i := lo; i <= hi && i < len(pane.entries); i++ {
		if i >= 0 && pane.entries[i].name != ".." {
			sel[i] = true
		}
	}
	pane.selected = sel
	pane.touch()
}

// toggleVisual turns range-select mode on/off: while on, plain movement
// extends the selection from where it was switched on.
func (m *filesModel) toggleVisual(side int) {
	pane := m.paneRef(side)
	if pane.visual {
		pane.visual, pane.anchorSet = false, false
		m.setStatus(levelInfo, fmt.Sprintf("range select off · %d selected", len(pane.selected)))
		return
	}
	pane.visual = true
	pane.anchorSet = true
	pane.anchor = pane.index
	pane.rangeBase = make(map[int]bool, len(pane.selected))
	for k, v := range pane.selected {
		pane.rangeBase[k] = v
	}
	m.applyRange(side)
	m.setStatus(levelInfo, "range select on — move to extend, V or esc to finish")
}

// selStats summarises a pane's selection.
type selStats struct {
	count, files, dirs int
	bytes              int64
}

func (s selStats) sizeText() string {
	if s.dirs > 0 && s.files == 0 {
		return plural(s.dirs, "folder", "folders")
	}
	t := humanSize(s.bytes)
	if s.dirs > 0 {
		t += " + " + plural(s.dirs, "folder", "folders")
	}
	return t
}

func (m filesModel) selectionStats(side int) selStats {
	pane := m.paneRefConst(side)
	var st selStats
	if len(pane.selected) == 0 {
		return st
	}
	if c := m.frames; c != nil && c.selSide == side && c.selRev == pane.rev && c.selValid && c.selN == len(pane.selected) {
		return c.sel
	}
	for i := range pane.selected {
		if i < 0 || i >= len(pane.entries) || pane.entries[i].name == ".." {
			continue
		}
		st.count++
		if pane.entries[i].isDir {
			st.dirs++
		} else {
			st.files++
			st.bytes += pane.entries[i].size
		}
	}
	if c := m.frames; c != nil {
		c.selSide, c.selRev, c.selN, c.sel, c.selValid = side, pane.rev, len(pane.selected), st, true
	}
	return st
}

// freeSpace reports free bytes for a local pane (measured at load time).
func (m filesModel) freeSpace(side int) (int64, bool) {
	pane := m.paneRefConst(side)
	if pane.remote || pane.free <= 0 {
		return 0, false
	}
	return pane.free, true
}

// fmRelTime renders a coarse "3h ago" style age.
func fmRelTime(ts time.Time) string {
	d := time.Since(ts)
	switch {
	case d < 0:
		return "in the future"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 730*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/24/30))
	}
	return fmt.Sprintf("%dy ago", int(d.Hours()/24/365))
}
