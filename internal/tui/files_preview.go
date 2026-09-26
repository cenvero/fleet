// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cenvero/fleet/internal/core"
	tea "github.com/charmbracelet/bubbletea"
)

// ============================================================================
// Preview pane
//
// The preview follows the focused entry of the focused pane. Loads are
// debounced (holding ↓ through a big directory does not issue a read per row),
// run off the UI thread, and are strictly size-capped: local files are read
// through a LimitReader, small remote files through a bounded writer, and
// large remote text files through the agent's memory-bounded tail reader. A
// huge or binary file is never pulled into memory — binaries get a short hex
// dump of their first bytes.
// ============================================================================

const (
	previewMaxBytes     = 64 << 10  // bytes of a local file read for preview
	previewRemoteMax    = 256 << 10 // remote files up to this size are fetched whole
	previewMaxLines     = 400       // highlighted lines kept per preview
	previewTailLines    = 200       // lines fetched from the end of large remote text files
	previewDirEntries   = 200       // local folder entries listed
	previewDebounce     = 90 * time.Millisecond
	previewCacheEntries = 32
)

type previewState struct {
	on      bool
	seq     int
	key     string
	loading bool
	data    *previewData
	cache   *previewCache
	scroll  int
}

type previewData struct {
	key     string
	name    string
	glyph   string
	kind    fmIconKind
	meta    [][2]string
	lines   []string // styled content lines
	note    string   // e.g. "first 64 KiB of 200 MB"
	errText string
}

type previewCache struct {
	mu    sync.Mutex
	order []string
	items map[string]*previewData
}

func (c *previewCache) get(key string) *previewData {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.items[key]
}

func (c *previewCache) put(d *previewData) {
	if c == nil || d == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[string]*previewData{}
	}
	if _, ok := c.items[d.key]; !ok {
		c.order = append(c.order, d.key)
	}
	c.items[d.key] = d
	for len(c.order) > previewCacheEntries {
		delete(c.items, c.order[0])
		c.order = c.order[1:]
	}
}

type previewDebounceMsg struct{ seq int }

type previewLoadedMsg struct {
	key  string
	data *previewData
}

// previewKey identifies what the preview should show for the focused entry.
// Size and mtime are part of it so an edited file is re-read.
func (m filesModel) previewKey() string {
	pane := m.paneRefConst(m.focus)
	it := m.focusedItem(m.focus)
	if it.name == "" || it.name == ".." {
		return "dir\x00" + pane.source + "\x00" + pane.cwd
	}
	return pane.source + "\x00" + joinPath(pane.cwd, it.name, pane.pathStyle) + "\x00" +
		strconv.FormatInt(it.size, 10) + "\x00" + strconv.FormatInt(it.modTime.UnixNano(), 10)
}

// syncPreview is called after every update: when the focused entry changed it
// serves the preview from cache or schedules a debounced load.
func (m *filesModel) syncPreview() tea.Cmd {
	if !m.preview.on {
		return nil
	}
	if m.preview.cache == nil {
		m.preview.cache = &previewCache{}
	}
	key := m.previewKey()
	if key == m.preview.key {
		return nil
	}
	m.preview.key = key
	m.preview.seq++
	m.preview.scroll = 0
	if d := m.preview.cache.get(key); d != nil {
		m.preview.data, m.preview.loading = d, false
		return nil
	}
	m.preview.loading = true
	seq := m.preview.seq
	return tea.Tick(previewDebounce, func(time.Time) tea.Msg { return previewDebounceMsg{seq: seq} })
}

func (m filesModel) onPreviewDebounce(msg previewDebounceMsg) (tea.Model, tea.Cmd) {
	if !m.preview.on || msg.seq != m.preview.seq {
		return m, nil
	}
	pane := m.paneRefConst(m.focus)
	it := m.focusedItem(m.focus)
	key := m.preview.key
	app := m.app
	return m, func() tea.Msg {
		return previewLoadedMsg{key: key, data: buildPreview(app, pane, it, key)}
	}
}

func (m filesModel) onPreviewLoaded(msg previewLoadedMsg) (tea.Model, tea.Cmd) {
	m.preview.cache.put(msg.data)
	if msg.key == m.preview.key {
		m.preview.data = msg.data
		m.preview.loading = false
	}
	return m, nil
}

func (m filesModel) togglePreview() (tea.Model, tea.Cmd) {
	m.preview.on = !m.preview.on
	m.preview.key = ""
	if m.preview.on {
		m.setStatus(levelInfo, "preview on — P or F3 to hide")
	} else {
		m.setStatus(levelInfo, "preview off")
	}
	m.clampScroll(0)
	m.clampScroll(1)
	return m, m.syncPreview()
}

// buildPreview gathers metadata and (size-capped) content for one entry. It
// runs on a tea.Cmd goroutine.
func buildPreview(app *core.App, pane paneState, it fileItem, key string) *previewData {
	d := &previewData{key: key}
	if it.name == "" || it.name == ".." {
		d.name = pane.label()
		d.glyph, d.kind = sourceGlyph(pane.remote), iconKindDir
		d.meta = [][2]string{{"Location", pane.cwd}, {"Items", strconv.Itoa(realCountFast(pane.entries))}}
		return d
	}
	full := joinPath(pane.cwd, it.name, pane.pathStyle)
	d.name = it.name
	d.glyph, d.kind = iconKindFor(it)
	kind := "File"
	switch {
	case it.isDir:
		kind = "Folder"
	case it.symlink && it.linkDir:
		kind = "Symlink → folder"
	case it.symlink:
		kind = "Symlink"
	case it.mode&0o111 != 0:
		kind = "Executable"
	}
	d.meta = append(d.meta, [2]string{"Kind", kind})
	if !it.isDir {
		d.meta = append(d.meta, [2]string{"Size", fmt.Sprintf("%s (%s bytes)", humanSize(it.size), groupThousands(it.size))})
	}
	if !it.modTime.IsZero() {
		d.meta = append(d.meta, [2]string{"Modified", it.modTime.Format("2006-01-02 15:04:05") + "  (" + fmRelTime(it.modTime) + ")"})
	}
	if it.mode != 0 {
		d.meta = append(d.meta, [2]string{"Mode", os.FileMode(it.mode).String() + "  " + octalModeString(it.mode)})
	}
	if !pane.remote {
		if fi, err := os.Lstat(full); err == nil {
			if owner := fileOwner(fi); owner != "" {
				d.meta = append(d.meta, [2]string{"Owner", owner})
			}
		}
		if it.symlink {
			if target, err := os.Readlink(full); err == nil {
				t := target
				if _, err := os.Stat(full); err != nil {
					t += "  (broken)"
				}
				d.meta = append(d.meta, [2]string{"Target", t})
			}
		}
	}
	d.meta = append(d.meta, [2]string{"Where", pane.label() + ":" + full})

	switch {
	case it.isDir || it.linkDir:
		if pane.remote {
			d.note = "↵ open folder"
			return d
		}
		previewLocalDir(d, full)
	case pane.remote:
		previewRemoteFile(d, app, pane.source, full, it)
	default:
		previewLocalFile(d, full, it)
	}
	return d
}

func previewLocalDir(d *previewData, full string) {
	f, err := os.Open(full) // #nosec G304 -- operator-selected local path
	if err != nil {
		d.errText = err.Error()
		return
	}
	defer f.Close()
	entries, err := f.ReadDir(previewDirEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		d.errText = err.Error()
		return
	}
	more := len(entries) > previewDirEntries
	if more {
		entries = entries[:previewDirEntries]
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})
	p := fmPal()
	for _, e := range entries {
		it := fileItem{name: e.Name(), isDir: e.IsDir()}
		if info, err := e.Info(); err == nil {
			it.mode = uint32(info.Mode())
			it.symlink = info.Mode()&os.ModeSymlink != 0
		}
		glyph, kind := iconKindFor(it)
		name := fmSanitize(it.name)
		np := p.base
		if it.isDir {
			name += "/"
			np = p.dir
		}
		d.lines = append(d.lines, p.icons[kind].s(glyph)+p.base.s(" ")+np.s(name))
	}
	switch {
	case len(entries) == 0:
		d.note = "empty folder"
	case more:
		d.note = fmt.Sprintf("first %d entries", previewDirEntries)
	default:
		d.note = plural(len(entries), "entry", "entries")
	}
}

func previewLocalFile(d *previewData, full string, it fileItem) {
	f, err := os.Open(full) // #nosec G304 -- operator-selected local path
	if err != nil {
		d.errText = err.Error()
		return
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, previewMaxBytes))
	if err != nil {
		d.errText = err.Error()
		return
	}
	previewContent(d, it.name, buf, it.size > int64(len(buf)), it.size)
}

func previewRemoteFile(d *previewData, app *core.App, server, full string, it fileItem) {
	if app == nil {
		return
	}
	if it.size <= previewRemoteMax {
		var hb headBuffer
		hb.limit = previewRemoteMax
		if _, err := app.CatRemoteFile(server, full, &hb); err != nil && !hb.full {
			d.errText = err.Error()
			return
		}
		previewContent(d, it.name, hb.Bytes(), hb.full, it.size)
		return
	}
	if !looksTextual(it.name) {
		d.note = "large remote file — contents not fetched for preview (" + humanSize(it.size) + ")"
		return
	}
	res, err := app.TailRemoteFile(server, full, previewTailLines, "")
	if err != nil {
		d.errText = err.Error()
		return
	}
	var b strings.Builder
	for _, ln := range res.Lines {
		b.WriteString(ln.Text)
		b.WriteByte('\n')
	}
	data := []byte(b.String())
	if isBinary(data) {
		d.note = "binary file"
		return
	}
	d.lines = highlightPreview(it.name, string(data))
	d.note = fmt.Sprintf("last %d lines of %s", len(res.Lines), humanSize(it.size))
}

// previewContent turns a capped byte slice into highlighted lines or a hex dump.
func previewContent(d *previewData, name string, data []byte, truncated bool, size int64) {
	if isBinary(data) {
		d.lines = hexDumpLines(data, 256)
		d.note = "binary file · first 256 bytes"
		return
	}
	d.lines = highlightPreview(name, string(data))
	if truncated {
		d.note = fmt.Sprintf("first %s of %s", humanSize(int64(len(data))), humanSize(size))
	}
}

// highlightPreview highlights text, dropping a possibly cut last line and
// capping the number of lines kept.
func highlightPreview(name, text string) []string {
	if i := strings.LastIndexByte(text, '\n'); i >= 0 && len(text) >= previewMaxBytes {
		text = text[:i]
	}
	lines := strings.SplitAfterN(text, "\n", previewMaxLines+1)
	if len(lines) > previewMaxLines {
		lines = lines[:previewMaxLines]
	}
	return highlightLines(name, strings.TrimRight(strings.Join(lines, ""), "\n"))
}

// hexDumpLines renders up to n bytes as "offset  hex  |ascii|" lines (16 bytes
// per line; the renderer drops columns that do not fit).
func hexDumpLines(data []byte, n int) []string {
	if len(data) > n {
		data = data[:n]
	}
	p := fmPal()
	var out []string
	for off := 0; off < len(data); off += 16 {
		end := off + 16
		if end > len(data) {
			end = len(data)
		}
		chunk := data[off:end]
		var hex strings.Builder
		for i := range 16 {
			if i < len(chunk) {
				fmt.Fprintf(&hex, "%02x ", chunk[i])
			} else {
				hex.WriteString("   ")
			}
			if i == 7 {
				hex.WriteByte(' ')
			}
		}
		var asc strings.Builder
		for _, c := range chunk {
			if c >= 0x20 && c < 0x7f {
				asc.WriteByte(c)
			} else {
				asc.WriteByte('.')
			}
		}
		out = append(out, p.dim.s(fmt.Sprintf("%08x  ", off))+p.base.s(hex.String())+p.muted.s("|"+asc.String()+"|"))
	}
	return out
}

// headBuffer keeps the first `limit` bytes written to it and then refuses more,
// which makes a streaming reader (CatRemoteFile) stop early.
type headBuffer struct {
	bytes.Buffer
	limit int
	full  bool
}

var errPreviewFull = errors.New("preview limit reached")

func (b *headBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.Len()
	if room <= 0 {
		b.full = true
		return 0, errPreviewFull
	}
	if len(p) > room {
		b.Buffer.Write(p[:room])
		b.full = true
		return room, errPreviewFull
	}
	return b.Buffer.Write(p)
}

// looksTextual guesses from the name whether tailing a large file is useful.
func looksTextual(name string) bool {
	low := strings.ToLower(name)
	for _, ext := range []string{".log", ".txt", ".md", ".csv", ".tsv", ".json", ".jsonl", ".ndjson",
		".yaml", ".yml", ".xml", ".conf", ".cfg", ".ini", ".toml", ".out", ".err", ".sql", ".html"} {
		if strings.HasSuffix(low, ext) {
			return true
		}
	}
	// Rotated logs: syslog.1, access.log.2
	return strings.Contains(low, ".log.") || !strings.Contains(low, ".")
}

// ---- rendering ----

func (m filesModel) renderPreviewBox(l fmLayout, p *fmPalette) []string {
	w := l.previewW
	cw := w - 2
	if cw < 4 {
		cw = 4
	}
	border := p.paneRule
	out := make([]string, 0, l.boxH)

	title := " ◧ Preview "
	fill := w - 3 - fmWidth(title) - 1
	if fill < 0 {
		fill = 0
	}
	out = append(out, border.s("╭─")+p.muted.s(title)+border.s(strings.Repeat("─", fill)+"─╮"))

	d := m.preview.data
	body := make([]string, 0, l.boxH-2)
	line := func(s string) { body = append(body, s) }
	switch {
	case d == nil && m.preview.loading:
		line(p.pad(1) + p.muted.s(fmPadRight("Loading…", cw-1)))
	case d == nil:
		line(p.pad(1) + p.dim.s(fmPadRight("Nothing selected", cw-1)))
	default:
		name := fmFitName(fmSanitize(d.name), cw-3)
		line(p.pad(1) + p.icons[d.kind].s(d.glyph) + p.pad(1) + p.accentB.s(name) + p.pad(cw-3-fmWidth(name)))
		line(p.pad(cw))
		labelW := 9
		for _, kv := range d.meta {
			var vals []string
			switch kv[0] {
			case "Where", "Target", "Location":
				// Paths: keep the informative end on one line.
				vals = []string{fmFitPathLeft(fmSanitize(kv[1]), cw-labelW-2)}
			default:
				vals = fmWrap(fmSanitize(kv[1]), cw-labelW-2)
			}
			for i, v := range vals {
				lab := ""
				if i == 0 {
					lab = kv[0]
				}
				line(p.pad(1) + p.dim.s(fmPadRight(lab, labelW)) + p.base.s(" "+fmPadRight(v, cw-labelW-2)))
			}
		}
		if d.errText != "" {
			line(p.pad(cw))
			for _, v := range fmWrap("⚠ "+fmSanitize(d.errText), cw-2) {
				line(p.pad(1) + p.danger.s(fmPadRight(v, cw-1)))
			}
		}
		if len(d.lines) > 0 || d.note != "" {
			line(previewRule(d.note, cw, p))
		}
		if m.preview.loading {
			line(p.pad(1) + p.muted.s(fmPadRight("Loading…", cw-1)))
		}
		gutter := 0
		if cw >= 40 && len(d.lines) > 0 && !strings.HasPrefix(d.note, "binary") && d.kind != iconKindDir {
			gutter = len(strconv.Itoa(len(d.lines))) + 1
		}
		start := m.preview.scroll
		if start > len(d.lines) {
			start = len(d.lines)
		}
		for i := start; i < len(d.lines) && len(body) < l.boxH-2; i++ {
			content := d.lines[i]
			var b strings.Builder
			b.WriteString(p.pad(1))
			if gutter > 0 {
				b.WriteString(p.dim.s(fmPadLeft(strconv.Itoa(i+1), gutter-1) + " "))
			}
			avail := cw - 1 - gutter
			t := truncateANSI(content, avail)
			b.WriteString(t)
			if tw := ansiWidth(t); tw < avail {
				b.WriteString(p.pad(avail - tw))
			}
			line(b.String())
		}
	}
	for len(body) < l.boxH-2 {
		body = append(body, p.pad(cw))
	}
	body = body[:l.boxH-2]
	side := border.s("│")
	for _, b := range body {
		out = append(out, side+b+side)
	}
	out = append(out, border.s("╰"+strings.Repeat("─", w-2)+"╯"))
	return out
}

// previewRule is a full-width separator with an optional caption:
// "── first 64.0K of 200.0M ─────".
func previewRule(note string, cw int, p *fmPalette) string {
	if note == "" {
		return p.paneRule.s(strings.Repeat("─", cw))
	}
	caption := " " + fmFit(fmSanitize(note), cw-6) + " "
	rest := cw - 2 - fmWidth(caption)
	if rest < 0 {
		rest = 0
	}
	return p.paneRule.s("──") + p.dim.s(caption) + p.paneRule.s(strings.Repeat("─", rest))
}

// ansiWidth is the display width of a string that may contain ANSI styling.
func ansiWidth(s string) int {
	if !strings.ContainsRune(s, '\x1b') {
		return fmWidth(s)
	}
	return fmWidth(stripANSIString(s))
}

// stripANSIString removes CSI escape sequences.
func stripANSIString(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if r == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if isCSIFinal(r) {
				inEsc = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
