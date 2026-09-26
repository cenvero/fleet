// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/core"
	tea "github.com/charmbracelet/bubbletea"
)

// ============================================================================
// Transfer queue
//
// Every copy/move becomes a row in a queue. At most maxActiveTransfers run at
// once (each already fans out into parallel streams inside core), the rest
// wait and can be cancelled before they start. Work happens on goroutines; the
// UI only ever receives messages, so a 200 MB upload never blocks a keypress.
// Progress comes from core's ProgressFunc callbacks and is coalesced to a few
// UI updates per second per transfer.
// ============================================================================

const (
	maxActiveTransfers = 3
	progressInterval   = 120 * time.Millisecond
	refreshDebounce    = 300 * time.Millisecond
)

type transferRow struct {
	id        int
	label     string
	bytesDone int64
	total     int64
	rate      float64
	streams   int
	done      bool
	err       error

	queued    bool // waiting for a free slot
	cancelled bool // removed from the queue before it started
	name      string
	isDir     bool
	kind      dirTransferKind
	route     string // human destination ("web-01:/srv/incoming")
	job       *transferJob
	started   time.Time
	finished  time.Time
	lastBytes int64
	lastAt    time.Time
}

// transferJob is everything needed to (re)run a transfer.
type transferJob struct {
	srcSource, srcPath string
	dstSource, dstPath string
	isDir              bool
	kind               dirTransferKind
}

type transferChans struct {
	progress chan core.ProgressUpdate
	done     chan transferOutcome
}

type transferOutcome struct{ err error }

// xferState is the display state of a row.
type xferState int

const (
	xferRunning xferState = iota
	xferQueued
	xferFailed
	xferCancelled
	xferDone
)

func (t *transferRow) state() xferState {
	switch {
	case t.cancelled:
		return xferCancelled
	case t.done && t.err != nil:
		return xferFailed
	case t.done:
		return xferDone
	case t.queued:
		return xferQueued
	}
	return xferRunning
}

// refreshTickMsg reloads panes after transfers finish, debounced so a batch of
// 500 small files triggers one listing refresh instead of 1000.
type refreshTickMsg struct{ seq int }

// enqueueTransfer adds a job to the queue and returns its row.
func (m *filesModel) enqueueTransfer(job *transferJob, label, name, route string, total int64) *transferRow {
	id := m.nextID
	m.nextID++
	row := &transferRow{
		id: id, label: label, name: name, total: total, queued: true,
		isDir: job.isDir, kind: job.kind, route: route, job: job,
	}
	m.transfers = append(m.transfers, row)
	return row
}

// pumpTransfers starts queued jobs while there is a free slot.
func (m *filesModel) pumpTransfers() tea.Cmd {
	if m.chans == nil {
		m.chans = make(map[int]*transferChans)
	}
	running := 0
	for _, t := range m.transfers {
		if t.state() == xferRunning {
			running++
		}
	}
	var cmds []tea.Cmd
	for _, t := range m.transfers {
		if running >= maxActiveTransfers {
			break
		}
		if t.state() != xferQueued || t.job == nil {
			continue
		}
		cmds = append(cmds, m.startTransfer(t))
		running++
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// startTransfer launches one queued row on a goroutine and returns the command
// that relays its progress into the UI.
func (m *filesModel) startTransfer(t *transferRow) tea.Cmd {
	c := &transferChans{
		progress: make(chan core.ProgressUpdate, 1),
		done:     make(chan transferOutcome, 1),
	}
	m.chans[t.id] = c
	t.queued = false
	t.started = time.Now()
	t.lastAt = t.started
	progress := func(u core.ProgressUpdate) {
		select {
		case c.progress <- u:
		default:
			// Drop an intermediate sample rather than block a transfer worker;
			// the poller always reads the freshest one available.
			select {
			case <-c.progress:
			default:
			}
			select {
			case c.progress <- u:
			default:
			}
		}
	}
	app := m.app
	job := *t.job
	go func() {
		var err error
		if job.isDir {
			err = runDirOp(app, job.srcSource, job.srcPath, job.dstSource, job.dstPath, job.kind, progress)
		} else {
			err = runFileOp(app, job.srcSource, job.srcPath, job.dstSource, job.dstPath, job.kind, progress)
		}
		c.done <- transferOutcome{err: err}
	}()
	return pollTransferCmd(t.id, c)
}

// pollTransferCmd waits for the next progress sample or completion. Samples
// are coalesced for progressInterval so a fast transfer cannot flood the
// update loop, but completion is always delivered immediately.
func pollTransferCmd(id int, c *transferChans) tea.Cmd {
	return func() tea.Msg {
		select {
		case u := <-c.progress:
			timer := time.NewTimer(progressInterval)
			defer timer.Stop()
			for {
				select {
				case u2 := <-c.progress:
					u = u2
				case out := <-c.done:
					return transferDoneMsg{id: id, err: out.err, last: &u}
				case <-timer.C:
					return progressTickMsg{id: id, u: u}
				}
			}
		case out := <-c.done:
			return transferDoneMsg{id: id, err: out.err}
		}
	}
}

func (m *filesModel) applyProgress(id int, u core.ProgressUpdate) {
	for _, t := range m.transfers {
		if t.id != id {
			continue
		}
		now := time.Now()
		if u.TotalBytes > 0 {
			t.total = u.TotalBytes
		}
		if u.RatePerSec > 0 {
			t.rate = u.RatePerSec
		} else if dt := now.Sub(t.lastAt).Seconds(); dt > 0.05 && u.BytesDone >= t.lastBytes {
			inst := float64(u.BytesDone-t.lastBytes) / dt
			if t.rate == 0 {
				t.rate = inst
			} else {
				t.rate = 0.7*t.rate + 0.3*inst
			}
		}
		t.bytesDone = u.BytesDone
		t.streams = u.ActiveStreams
		t.lastBytes, t.lastAt = u.BytesDone, now
	}
}

func (m filesModel) handleTransferDone(msg transferDoneMsg) (tea.Model, tea.Cmd) {
	if msg.last != nil {
		m.applyProgress(msg.id, *msg.last)
	}
	var row *transferRow
	for _, t := range m.transfers {
		if t.id == msg.id {
			row = t
			t.done = true
			t.err = msg.err
			t.finished = time.Now()
			if msg.err == nil {
				if t.total <= 0 && t.bytesDone > 0 {
					t.total = t.bytesDone
				}
				t.bytesDone = t.total
			}
		}
	}
	delete(m.chans, msg.id)
	label := msg.label
	if row != nil && label == "" {
		label = row.label
	}
	active := 0
	for _, t := range m.transfers {
		if s := t.state(); s == xferRunning || s == xferQueued {
			active++
		}
	}
	switch {
	case msg.err != nil:
		m.setStatus(levelError, "failed: "+label+" — "+msg.err.Error())
	case active == 0:
		done, failed, bytes := m.transferTotals()
		if failed > 0 {
			m.setStatus(levelWarn, fmt.Sprintf("finished %d transfer(s), %d failed — t to review, r to retry", done, failed))
		} else if done > 1 {
			m.setStatus(levelOK, fmt.Sprintf("completed %d transfers · %s", done, humanSize(bytes)))
		} else {
			m.setStatus(levelOK, "completed: "+label)
		}
	default:
		m.setStatus(levelOK, "completed: "+label)
	}
	cmds := []tea.Cmd{m.pumpTransfers(), m.scheduleRefresh()}
	return m, tea.Batch(cmds...)
}

// transferTotals summarises finished rows since the last clear.
func (m filesModel) transferTotals() (done, failed int, bytes int64) {
	for _, t := range m.transfers {
		if t.id < m.batchFrom {
			continue
		}
		switch t.state() {
		case xferDone:
			done++
			bytes += t.total
		case xferFailed:
			failed++
		}
	}
	return
}

// transfersActive reports whether anything is running or queued.
func (m filesModel) transfersActive() bool {
	for _, t := range m.transfers {
		if s := t.state(); s == xferRunning || s == xferQueued {
			return true
		}
	}
	return false
}

func (m *filesModel) scheduleRefresh() tea.Cmd {
	m.refreshSeq++
	seq := m.refreshSeq
	return tea.Tick(refreshDebounce, func(time.Time) tea.Msg { return refreshTickMsg{seq: seq} })
}

func (m filesModel) refreshBoth() tea.Cmd {
	return tea.Batch(
		m.loadCmd(0, m.left.source, m.left.cwd),
		m.loadCmd(1, m.right.source, m.right.cwd),
	)
}

// ---- queue actions ----

// cancelTransfer removes a queued row from the queue. Running transfers cannot
// be interrupted (core's transfer API has no cancellation hook), so the user
// is told so instead of being shown a fake "cancelled".
func (m *filesModel) cancelTransfer(t *transferRow) {
	switch t.state() {
	case xferQueued:
		t.cancelled = true
		t.queued = false
		t.finished = time.Now()
		m.setStatus(levelWarn, "cancelled: "+t.label)
	case xferRunning:
		m.setStatus(levelWarn, "a running transfer can't be interrupted — it will finish; queued items can be cancelled")
	default:
		m.setStatus(levelInfo, "nothing to cancel: "+t.label+" already finished")
	}
}

// cancelQueued cancels every row that has not started yet.
func (m *filesModel) cancelQueued() int {
	n := 0
	for _, t := range m.transfers {
		if t.state() == xferQueued {
			t.cancelled, t.queued, t.finished = true, false, time.Now()
			n++
		}
	}
	return n
}

// retryTransfer re-queues a failed or cancelled row.
func (m *filesModel) retryTransfer(t *transferRow) tea.Cmd {
	s := t.state()
	if (s != xferFailed && s != xferCancelled) || t.job == nil {
		m.setStatus(levelInfo, "only failed or cancelled transfers can be retried")
		return nil
	}
	t.done, t.err, t.cancelled = false, nil, false
	t.queued = true
	t.bytesDone, t.rate, t.lastBytes = 0, 0, 0
	t.finished = time.Time{}
	m.setStatus(levelInfo, "retrying "+t.label)
	return m.pumpTransfers()
}

// retryFailed re-queues every failed row.
func (m *filesModel) retryFailed() tea.Cmd {
	n := 0
	for _, t := range m.transfers {
		if t.state() == xferFailed && t.job != nil {
			t.done, t.err = false, nil
			t.queued = true
			t.bytesDone, t.rate, t.lastBytes = 0, 0, 0
			n++
		}
	}
	if n == 0 {
		m.setStatus(levelInfo, "no failed transfers to retry")
		return nil
	}
	m.setStatus(levelInfo, fmt.Sprintf("retrying %d failed transfer(s)", n))
	return m.pumpTransfers()
}

// clearFinished drops finished (done/failed/cancelled) rows from the panel.
func (m *filesModel) clearFinished() int {
	kept := m.transfers[:0:0]
	n := 0
	for _, t := range m.transfers {
		if s := t.state(); s == xferRunning || s == xferQueued {
			kept = append(kept, t)
		} else {
			n++
		}
	}
	m.transfers = kept
	if m.xferIndex >= len(m.transfers) {
		m.xferIndex = len(m.transfers) - 1
	}
	if m.xferIndex < 0 {
		m.xferIndex = 0
	}
	if len(m.transfers) == 0 {
		m.xferFocus = false
	}
	return n
}

// ---- panel geometry ----

// transferOrder is the display order: running, queued, then failures and
// completions (most recent first).
func (m filesModel) transferOrder() []*transferRow {
	out := append([]*transferRow(nil), m.transfers...)
	rank := func(t *transferRow) int {
		switch t.state() {
		case xferRunning:
			return 0
		case xferQueued:
			return 1
		case xferFailed:
			return 2
		case xferCancelled:
			return 3
		}
		return 4
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i]), rank(out[j])
		if ri != rj {
			return ri < rj
		}
		if ri >= 2 {
			return out[i].id > out[j].id
		}
		return out[i].id < out[j].id
	})
	return out
}

func (m filesModel) transferMaxRows() int {
	switch {
	case m.height >= 40:
		return 6
	case m.height >= 30:
		return 4
	}
	return 2
}

// transferExpanded reports whether the panel shows per-item rows (vs. a
// one-line summary).
func (m filesModel) transferExpanded() bool {
	if m.xferFocus {
		return true
	}
	for _, t := range m.transfers {
		if s := t.state(); s == xferRunning || s == xferQueued || s == xferFailed {
			return true
		}
	}
	return false
}

func (m filesModel) transferPanelHeight() int {
	if len(m.transfers) == 0 {
		return 0
	}
	if !m.transferExpanded() {
		return 1
	}
	n := len(m.transfers)
	if max := m.transferMaxRows(); n > max {
		n = max
	}
	return 1 + n
}

// transferWindow returns the first displayed row index for the panel.
func (m filesModel) transferWindow(rows int) int {
	if !m.xferFocus || m.xferIndex < rows {
		return 0
	}
	return m.xferIndex - rows + 1
}

// transferRowAt maps a panel line to an index in transferOrder (-1 = header).
func (m filesModel) transferRowAt(line int) int {
	if line <= 0 || !m.transferExpanded() {
		return -1
	}
	rows := m.transferPanelHeight() - 1
	idx := m.transferWindow(rows) + line - 1
	if idx >= len(m.transfers) {
		return -1
	}
	return idx
}

// ---- panel rendering ----

func (m filesModel) renderTransferPanel(l fmLayout, p *fmPalette) []string {
	if l.xferH <= 0 {
		return nil
	}
	w := l.innerW
	order := m.transferOrder()
	running, queued, failed, done := 0, 0, 0, 0
	var doneBytes, totalBytes int64
	var rate float64
	for _, t := range order {
		switch t.state() {
		case xferRunning:
			running++
			rate += t.rate
			doneBytes += t.bytesDone
			totalBytes += t.total
		case xferQueued:
			queued++
			totalBytes += t.total
		case xferFailed:
			failed++
		case xferDone:
			done++
		}
	}
	out := make([]string, 0, l.xferH)

	// Header / summary line.
	var head strings.Builder
	x := 0
	put := func(s string, pt fmPaint) {
		head.WriteString(pt.s(s))
		x += fmWidth(s)
	}
	title := " ⇅ Transfers "
	tp := p.accentB
	if m.xferFocus {
		tp = p.sel
	}
	put(title, tp)
	var counts []string
	if running > 0 {
		counts = append(counts, fmt.Sprintf("%d running", running))
	}
	if queued > 0 {
		counts = append(counts, fmt.Sprintf("%d queued", queued))
	}
	if failed > 0 {
		counts = append(counts, fmt.Sprintf("%d failed", failed))
	}
	if done > 0 {
		counts = append(counts, fmt.Sprintf("%d done", done))
	}
	put(" "+strings.Join(counts, " · ")+"  ", p.muted)
	if running+queued > 0 && totalBytes > 0 {
		pct := int(doneBytes * 100 / totalBytes)
		barW := 16
		if w < 100 {
			barW = 8
		}
		if x+barW+30 < w {
			put(progressBarText(pct, barW), p.accent)
			put(fmt.Sprintf(" %3d%%", pct), p.muted)
			if rate > 0 {
				put("  "+humanSize(int64(rate))+"/s", p.dim)
				if left := totalBytes - doneBytes; left > 0 {
					put("  ETA "+formatDuration(float64(left)/rate), p.dim)
				}
			}
		}
	}
	hint := "t manage · C clear"
	if m.xferFocus {
		hint = "↑↓ select · x cancel · r retry · R retry all · C clear · esc back"
	} else if failed > 0 {
		hint = "t review · R retry failed · C clear"
	}
	if hw := fmWidth(hint) + 2; x+hw <= w {
		head.WriteString(p.pad(w - x - hw))
		head.WriteString(p.dim.s(hint + "  "))
		x = w
	}
	line := head.String()
	if x > w {
		line = truncateANSI(line, w)
		x = w
	}
	if x < w {
		line += p.pad(w - x)
	}
	out = append(out, p.pad(l.padX)+line+p.pad(l.w-l.padX-w))

	rows := l.xferH - 1
	start := m.transferWindow(rows)
	for i := start; i < len(order) && len(out) < l.xferH; i++ {
		t := order[i]
		selected := m.xferFocus && i == m.xferIndex
		out = append(out, p.pad(l.padX)+m.renderTransferRow(t, w, selected, p)+p.pad(l.w-l.padX-w))
	}
	for len(out) < l.xferH {
		out = append(out, p.pad(l.w))
	}
	return out
}

// progressBarText is a plain (unstyled) eighth-block progress bar.
func progressBarText(pct, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	eighths := pct * width * 8 / 100
	full := eighths / 8
	rem := eighths % 8
	bar := strings.Repeat("█", full)
	used := full
	if rem > 0 && full < width {
		bar += string([]rune("▏▎▍▌▋▊▉")[rem-1])
		used++
	}
	if empty := width - used; empty > 0 {
		bar += strings.Repeat("░", empty)
	}
	return bar
}

// smoothBar renders a sub-cell-precise styled progress bar using eighth blocks.
func smoothBar(pct, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	if width < 1 {
		width = 1
	}
	p := fmPal()
	bar := progressBarText(pct, width)
	filled := strings.TrimRight(bar, "░")
	rest := bar[len(filled):]
	return p.barFill.s(filled) + p.barTrack.s(rest) + p.muted.s(fmt.Sprintf(" %3d%%", pct))
}

func (m filesModel) renderTransferRow(t *transferRow, w int, selected bool, p *fmPalette) string {
	st := t.state()
	glyph := strings.TrimSpace(strings.SplitN(t.label, " ", 2)[0])
	name := t.name
	if name == "" {
		name = strings.TrimSpace(strings.TrimPrefix(t.label, glyph))
	}
	name = fmSanitize(name)
	if t.isDir && !strings.HasSuffix(name, "/") {
		name += "/"
	}
	nameW := w / 4
	if nameW < 14 {
		nameW = 14
	}
	if nameW > 40 {
		nameW = 40
	}
	lead := "   " + glyph + " " + fmPadRight(fmFitName(name, nameW), nameW) + "  "
	restW := w - fmWidth(lead)
	var rest string
	var rp fmPaint
	switch st {
	case xferRunning:
		barW := 20
		switch {
		case w < 90:
			barW = 8
		case w < 130:
			barW = 14
		}
		pct := 0
		if t.total > 0 {
			pct = int(t.bytesDone * 100 / t.total)
		}
		bytes := humanSize(t.bytesDone)
		if t.total > 0 {
			bytes += "/" + humanSize(t.total)
		}
		speed := "—"
		if t.rate > 0 {
			speed = humanSize(int64(t.rate)) + "/s"
		}
		eta := "ETA " + formatETA(t)
		if t.total > 0 && t.bytesDone >= t.total {
			eta = "finalizing…"
		}
		meta := fmt.Sprintf(" %3d%%  %s  %s  %s", pct, fmPadLeft(bytes, 15), fmPadLeft(speed, 9), eta)
		if t.streams > 1 && w >= 130 {
			meta += fmt.Sprintf("  %d streams", t.streams)
		}
		route := ""
		if w >= 150 && t.route != "" {
			route = "  → " + fmSanitize(t.route)
		}
		if selected {
			return p.sel.s(fmPadRight(lead+progressBarText(pct, barW)+meta+route, w))
		}
		bar := progressBarText(pct, barW)
		filled := strings.TrimRight(bar, "░")
		tail := fmPadRight(meta+route, restW-barW)
		return p.base.s(lead) + p.barFill.s(filled) + p.barTrack.s(bar[len(filled):]) + p.muted.s(tail)
	case xferQueued:
		rest, rp = "queued", p.dim
		if t.total > 0 {
			rest += " · " + humanSize(t.total)
		}
		if t.route != "" && w >= 100 {
			rest += "  → " + fmSanitize(t.route)
		}
	case xferFailed:
		rest, rp = "✗ "+fmSanitize(t.err.Error()), p.danger
		if hint := transferErrorHint(t.err); hint != "" {
			rest += " — " + hint
		}
	case xferCancelled:
		rest, rp = "⊘ cancelled", p.warn
	case xferDone:
		rest, rp = "✓ done · "+humanSize(t.total), p.ok
		if !t.started.IsZero() && !t.finished.IsZero() {
			d := t.finished.Sub(t.started).Seconds()
			rest += " in " + formatDuration(d)
			if d > 0.5 && t.total > 0 {
				rest += " · " + humanSize(int64(float64(t.total)/d)) + "/s"
			}
		}
	}
	if selected {
		return p.sel.s(fmPadRight(lead+rest, w))
	}
	return p.base.s(lead) + rp.s(fmPadRight(fmFit(rest, restW), restW))
}

// transferErrorHint suggests a fix for common transfer failures.
func transferErrorHint(err error) string {
	if err == nil {
		return ""
	}
	low := strings.ToLower(err.Error())
	switch {
	case strings.Contains(low, "allowed file roots") || strings.Contains(low, "selected file root"):
		return "destination is outside the agent's --file-root"
	case strings.Contains(low, "permission denied") || strings.Contains(low, "access is denied"):
		return "check ownership/permissions"
	case strings.Contains(low, "no space"):
		return "destination disk is full"
	case strings.Contains(low, "connection refused") || strings.Contains(low, "timeout") ||
		strings.Contains(low, "unreachable") || strings.Contains(low, "eof"):
		return "server unreachable — r to retry"
	case strings.Contains(low, "exists"):
		return "destination already exists"
	}
	return "r to retry"
}

// ---- panel keyboard ----

func (m filesModel) handleTransferKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	order := m.transferOrder()
	if len(order) == 0 {
		m.xferFocus = false
		return m, nil
	}
	if m.xferIndex >= len(order) {
		m.xferIndex = len(order) - 1
	}
	cur := order[m.xferIndex]
	switch msg.String() {
	case "esc", "t", "tab", "q":
		m.xferFocus = false
		m.status = ""
		return m, nil
	case "up", "k":
		if m.xferIndex > 0 {
			m.xferIndex--
		}
	case "down", "j":
		if m.xferIndex < len(order)-1 {
			m.xferIndex++
		}
	case "home":
		m.xferIndex = 0
	case "end":
		m.xferIndex = len(order) - 1
	case "x", "delete", "d":
		m.cancelTransfer(cur)
		return m, nil
	case "X":
		n := m.cancelQueued()
		m.setStatus(levelWarn, fmt.Sprintf("cancelled %d queued transfer(s)", n))
		return m, nil
	case "r", "enter":
		return m, m.retryTransfer(cur)
	case "R":
		return m, m.retryFailed()
	case "C":
		n := m.clearFinished()
		m.setStatus(levelInfo, fmt.Sprintf("cleared %d finished transfer(s)", n))
		return m, nil
	case "?":
		return m.openHelp(), nil
	case "ctrl+c":
		return m, tea.Quit
	default:
		return m, nil
	}
	// Moving the selection surfaces the full error of a failed row.
	if sel := order[m.xferIndex]; sel.state() == xferFailed {
		m.setStatus(levelError, "failed: "+sel.label+" — "+sel.err.Error())
	} else {
		m.status = ""
	}
	return m, nil
}
