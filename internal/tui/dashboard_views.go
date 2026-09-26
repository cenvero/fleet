// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"sort"
	"strconv"
	"strings"
	"time"

	fleetalerts "github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/version"
	zone "github.com/lrstanley/bubblezone"
)

// Smallest terminal the dashboard draws into; below it a notice is shown.
const (
	dashMinW = 60
	dashMinH = 16
)

type drect struct{ x, y, w, h int }

// dzone is a clickable screen region registered with bubblezone.
type dashZone struct {
	id        string
	x0, x1, y int
}

// rctx carries per-frame rendering state.
type drctx struct {
	p     *dashPalette
	now   time.Time
	zones []dashZone
}

func (r *drctx) zone(id string, x0, x1, y int) {
	if x1 >= x0 {
		r.zones = append(r.zones, dashZone{id: id, x0: x0, x1: x1, y: y})
	}
}

// ---------------------------------------------------------------------------
// Geometry (shared by View and Update so scrolling matches what is drawn)
// ---------------------------------------------------------------------------

type dashGeom struct {
	aside        drect // Ops: controller posture panel (w == 0 when hidden)
	list, detail drect
	side         bool // detail is to the right of the list
	hidden       bool // no room for the detail pane: enter toggles a zoomed view
}

func (m *model) bodyRect() drect {
	return drect{x: 0, y: 2, w: m.width, h: max(m.height-3, 0)}
}

func (m *model) geom(tab dashboardTab) dashGeom {
	b := m.bodyRect()
	W, H := b.w, b.h
	var g dashGeom
	area := drect{0, 0, W, H}
	if tab == tabOps {
		switch {
		case W >= 110:
			g.aside = drect{0, 0, 46, H}
			area = drect{46, 0, W - 46, H}
		case H >= 28:
			g.aside = drect{0, 0, W, 9}
			area = drect{0, 9, W, H - 9}
		}
		if area.h >= 20 {
			dh := 8
			g.list = drect{area.x, area.y, area.w, area.h - dh}
			g.detail = drect{area.x, area.y + area.h - dh, area.w, dh}
		} else {
			g.list, g.detail, g.hidden = area, area, true
		}
		return g
	}
	if tab == tabLogs {
		switch {
		case W >= 120:
			lw := clamp(W*42/100, 44, 72)
			g.list, g.detail, g.side = drect{0, 0, lw, H}, drect{lw, 0, W - lw, H}, true
		case H >= 24:
			dh := H * 55 / 100
			g.list, g.detail = drect{0, 0, W, H - dh}, drect{0, H - dh, W, dh}
		default:
			g.list, g.detail, g.hidden = area, area, true
		}
		return g
	}
	sideMin := 140
	if tab == tabServers {
		sideMin = 150
	}
	switch {
	case W >= sideMin:
		dw := clamp(W*36/100, 50, 76)
		g.list, g.detail, g.side = drect{0, 0, W - dw, H}, drect{W - dw, 0, dw, H}, true
	case H >= 30:
		dh := clamp(H*42/100, 11, 18)
		g.list, g.detail = drect{0, 0, W, H - dh}, drect{0, H - dh, W, dh}
	default:
		g.list, g.detail, g.hidden = area, area, true
	}
	return g
}

// visibleRows is how many table rows the tab's list box shows.
func (m *model) visibleRows(tab dashboardTab) int {
	if tab == tabOverview || tab >= dashNumTabs {
		return 0
	}
	return max(m.geom(tab).list.h-3, 0)
}

func (m *model) viewerRows() int {
	g := m.geom(tabLogs)
	if g.hidden {
		return max(m.bodyRect().h-2, 1)
	}
	return max(g.detail.h-2, 1)
}

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

func (m model) View() string {
	rt := m.rt
	if rt != nil && rt.frameOK && rt.frameRev == m.rev && rt.frameW == m.width && rt.frameH == m.height {
		return rt.frame
	}
	frame := m.renderFrame()
	if rt != nil {
		rt.frame, rt.frameRev, rt.frameW, rt.frameH, rt.frameOK = frame, m.rev, m.width, m.height, true
	}
	return frame
}

func (m *model) renderFrame() string {
	dark := true
	if m.rt != nil {
		dark = m.rt.dark
	}
	r := &drctx{p: dashPaletteFor(dark), now: m.clock()}
	W, H := m.width, m.height
	if W <= 0 || H <= 0 {
		return ""
	}
	var lines []string
	switch {
	case W < dashMinW || H < dashMinH:
		lines = dashCenterMessage(r, W, H, []string{
			"Terminal too small",
			strconv.Itoa(W) + "×" + strconv.Itoa(H) + " — the dashboard needs at least " + strconv.Itoa(dashMinW) + "×" + strconv.Itoa(dashMinH),
			"resize the window, or press q to quit",
		})
	default:
		m.ensureDerived()
		lines = make([]string, 0, H)
		lines = append(lines, m.renderHeader(r, W))
		lines = append(lines, m.renderTabs(r, W))
		body := m.bodyRect()
		lines = append(lines, m.renderBody(r, body)...)
		lines = append(lines, m.renderFooter(r, W, H-1))
	}
	m.registerZones(r)
	return strings.Join(lines, "\n")
}

// registerZones hands this frame's clickable regions to bubblezone. Instead of
// wrapping zone markers around styled text in the real frame (bubblezone strips
// each marker with a full-string copy, which is what made the old frames cost
// ~100 MB at 500 servers), it scans a tiny synthetic frame of blank cells with
// the markers at the recorded coordinates.
func (m *model) registerZones(r *drctx) {
	if zone.DefaultManager == nil {
		return
	}
	sort.SliceStable(r.zones, func(i, j int) bool {
		if r.zones[i].y != r.zones[j].y {
			return r.zones[i].y < r.zones[j].y
		}
		return r.zones[i].x0 < r.zones[j].x0
	})
	var buf []byte
	if m.rt != nil {
		buf = m.rt.scanBuf[:0]
	}
	y, x := 0, 0
	for _, z := range r.zones {
		if z.y < y || (z.y == y && z.x0 < x) {
			continue // overlapping; first one wins
		}
		for y < z.y {
			buf = append(buf, '\n')
			y++
			x = 0
		}
		buf = append(buf, dashRunSpace.n(z.x0-x)...)
		buf = append(buf, zone.Mark(z.id, dashRunSpace.n(z.x1-z.x0+1))...)
		x = z.x1 + 1
	}
	synthetic := string(buf)
	if m.rt != nil {
		m.rt.scanBuf = buf
		if synthetic == m.rt.lastScan {
			return
		}
		m.rt.lastScan = synthetic
	}
	zone.Scan(synthetic)
}

func dashCenterMessage(r *drctx, W, H int, msg []string) []string {
	out := make([]string, 0, H)
	top := max((H-len(msg))/2, 0)
	l := newDLine(r.p, W)
	for i := 0; i < H; i++ {
		l.reset(W)
		if j := i - top; j >= 0 && j < len(msg) {
			txt := dashClean(msg[j])
			tw := min(dashWidth(txt), W)
			l.pad(sNone, (W-tw)/2)
			st := sMuted
			if j == 0 {
				st = sAccentBold
			}
			l.text(st, txt, tw)
		}
		out = append(out, l.String())
	}
	return out
}

// seg is one styled run of a composed line.
type dseg struct {
	s dstyle
	t string
}

func dsegsWidth(segs []dseg) int {
	w := 0
	for _, s := range segs {
		w += dashWidth(s.t)
	}
	return w
}

// ---------------------------------------------------------------------------
// Header, tabs, footer
// ---------------------------------------------------------------------------

func (m *model) renderHeader(r *drctx, W int) string {
	l := newDLine(r.p, W)
	st := m.snapshot.Status

	// Right: live/paused/refreshing + data age.
	var right []dseg
	switch {
	case m.inflight && !m.loadedAt.IsZero() && r.now.Sub(m.lastStart) >= time.Second:
		// Only a refresh that is taking a while is worth a flicker.
		right = append(right, dseg{sHdrMuted, "⟳ refreshing"}, dseg{sHdrMuted, " · "})
	case m.lastErr != nil:
		right = append(right, dseg{sHdrCrit, "✕ refresh failed: " + dashClean(dashFirstLine(m.lastErr.Error()))}, dseg{sHdrMuted, " · "})
	}
	toggleStart := len(right)
	if m.paused {
		right = append(right, dseg{sHdrWarn, "⏸ paused"})
	} else {
		right = append(right, dseg{sHdrOK, "● live"}, dseg{sHdrMuted, " " + dashFmtInterval(m.interval)})
	}
	toggleEnd := len(right)
	if !m.loadedAt.IsZero() {
		age := r.now.Sub(m.loadedAt)
		stale := !m.paused && age > dashMaxDur(3*m.interval, 15*time.Second)
		label := "updated " + dashAge(r.now, m.loadedAt) + " ago"
		if age < time.Second {
			label = "updated just now"
		}
		if stale {
			right = append(right, dseg{sHdrMuted, " · "}, dseg{sHdrWarn, "stale " + dashAge(r.now, m.loadedAt)})
		} else {
			right = append(right, dseg{sHdrMuted, " · " + label})
		}
	}
	right = append(right, dseg{sHdr, " "})
	rw := dsegsWidth(right)
	if rw > W/2 && len(right) > 0 && right[0].s == sHdrCrit {
		// Keep a long refresh error from eating the whole bar.
		cut, _ := dashCut(right[0].t, max(W/2-rw+dashWidth(right[0].t), 12))
		right[0].t = cut + "…"
		rw = dsegsWidth(right)
	}

	l.putW(sHdrBrand, " ◆ FLEET ", 9)
	budget := W - 9 - rw
	var left []dseg
	alias := st.Alias
	if alias == "" {
		alias = "fleet"
	}
	left = append(left, dseg{sHdr, " " + alias})
	extras := []string{
		strconv.Itoa(len(m.snapshot.Servers)) + " servers",
		dashVersion(st.Version),
		string(st.DatabaseBackend),
		st.Channel,
	}
	used := dsegsWidth(left)
	for _, e := range extras {
		if strings.TrimSpace(e) == "" {
			continue
		}
		t := " · " + e
		if used+dashWidth(t) > budget-1 {
			break
		}
		left = append(left, dseg{sHdrMuted, t})
		used += dashWidth(t)
	}
	for _, s := range left {
		l.put(s.s, dashClean(s.t))
	}
	gap := W - l.w - rw
	l.pad(sHdr, gap)
	x := l.w
	for i, s := range right {
		if i == toggleStart {
			x = l.w
		}
		l.put(s.s, s.t)
		if i == toggleEnd-1 {
			r.zone(dashRefreshToggleID(), x, l.w-1, 0)
		}
	}
	l.fill(sHdr)
	return string(l.b)
}

func dashMaxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func dashFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func dashVersion(v string) string {
	if n, ok := version.NormalizeSemVer(v); ok {
		return n
	}
	if v == "" {
		return ""
	}
	return v + " build"
}

var dashShortTabs = []string{"Home", "Srv", "Svc", "Logs", "Alrt", "Ops"}

func (m *model) tabCount(tab dashboardTab) (string, dstyle) {
	switch tab {
	case tabServers:
		return strconv.Itoa(len(m.snapshot.Servers)), sTabCount
	case tabServices:
		return strconv.Itoa(len(m.base.services)), sTabCount
	case tabLogs:
		return strconv.Itoa(len(m.snapshot.CachedLogs)), sTabCount
	case tabAlerts:
		s := m.base.stats
		switch {
		case s.OpenCritical > 0:
			return strconv.Itoa(s.Open), sTabCrit
		case s.Open > 0:
			return strconv.Itoa(s.Open), sWarn
		}
		return "0", sTabCount
	}
	return "", sTabCount
}

func (m *model) renderTabs(r *drctx, W int) string {
	type tabText struct {
		label, count string
		cs           dstyle
	}
	build := func(tier int) ([]tabText, int) {
		out := make([]tabText, len(dashboardTabs))
		w := 1
		for i, name := range dashboardTabs {
			label := strconv.Itoa(i+1) + " " + name
			if tier == 2 {
				label = strconv.Itoa(i+1) + " " + dashShortTabs[i]
			}
			t := tabText{label: label}
			if tier == 0 {
				t.count, t.cs = m.tabCount(dashboardTab(i))
			}
			out[i] = t
			w += 2 + dashWidth(t.label) + 1
			if t.count != "" {
				w += 1 + len(t.count)
			}
		}
		return out, w
	}
	tabs, tw := build(0)
	if tw > W {
		tabs, tw = build(1)
	}
	if tw > W {
		tabs, _ = build(2)
	}
	l := newDLine(r.p, W)
	l.pad(sNone, 1)
	for i, t := range tabs {
		start := l.w
		if dashboardTab(i) == m.activeTab {
			l.put(sTabActive, " "+t.label)
			if t.count != "" {
				l.put(sTabActive, " "+t.count)
			}
			l.put(sTabActive, " ")
		} else {
			l.put(sTab, " "+t.label)
			if t.count != "" {
				l.put(sNone, " ")
				l.put(t.cs, t.count)
			}
			l.put(sNone, " ")
		}
		r.zone(dashTabID(i), start, l.w-1, 1)
		l.pad(sNone, 1)
	}
	// Right: compact fleet health, when it fits.
	f := &m.base.fleet
	health := []dseg{
		{sOK, "●"}, {sMuted, strconv.Itoa(f.online) + " "},
		{sWarn, "◐"}, {sMuted, strconv.Itoa(f.degraded) + " "},
		{sCrit, "○"}, {sMuted, strconv.Itoa(f.offline) + " "},
	}
	if hw := dsegsWidth(health); l.room() >= hw+2 {
		l.pad(sNone, l.room()-hw)
		for _, s := range health {
			l.put(s.s, s.t)
		}
	}
	return l.String()
}

type dhint struct{ key, label string }

func (m *model) hints() []dhint {
	switch {
	case m.overlay == dashOverlayHelp:
		return []dhint{{"?", "toggle help"}}
	case m.zoom:
		return []dhint{{"esc", "back"}, {"j/k", "move"}, {"?", "help"}}
	}
	var out []dhint
	detail := func() {
		if m.geom(m.activeTab).hidden {
			out = append(out, dhint{"enter", "details"})
		}
	}
	switch m.activeTab {
	case tabServers:
		out = append(out, dhint{"j/k", "move"})
		detail()
		out = append(out, dhint{"/", "filter"}, dhint{"o", "sort"}, dhint{"O", "reverse"}, dhint{"s", "ssh"}, dhint{"f", "files"}, dhint{"c", "reconnect"}, dhint{"m", "metrics"}, dhint{"q", "quit"})
	case tabServices:
		out = append(out, dhint{"j/k", "move"})
		detail()
		out = append(out, dhint{"/", "filter"}, dhint{"L", "follow log"}, dhint{"R", "restart"}, dhint{"s", "ssh"}, dhint{"q", "quit"})
	case tabLogs:
		if m.viewerFocus {
			return []dhint{{"j/k", "scroll"}, {"/", "search"}, {"G", "end"}, {"L", "follow live"}, {"esc", "back"}}
		}
		out = append(out, dhint{"j/k", "move"}, dhint{"enter", "read"}, dhint{"/", "filter"}, dhint{"L", "follow live"}, dhint{"q", "quit"})
	case tabAlerts:
		out = append(out, dhint{"j/k", "move"})
		detail()
		out = append(out, dhint{"a", "ack"}, dhint{"z", "suppress"}, dhint{"u", "unsuppress"}, dhint{"v", "severity"}, dhint{"t", "state"}, dhint{"/", "filter"})
	case tabOps:
		out = append(out, dhint{"j/k", "move"})
		if g := m.geom(tabOps); g.hidden || g.aside.w == 0 {
			out = append(out, dhint{"enter", "controller"})
		}
		out = append(out, dhint{"/", "filter"}, dhint{"q", "quit"})
	default:
		out = append(out, dhint{"1-6", "tabs"}, dhint{"click", "drill down"}, dhint{"r", "refresh"}, dhint{"p", "pause"}, dhint{"+/-", "interval"}, dhint{"q", "quit"})
	}
	return out
}

func (m *model) renderFooter(r *drctx, W, y int) string {
	l := newDLine(r.p, W)
	if p := m.prompt; p != nil {
		l.put(sPrompt, " ? ")
		l.put(sPrompt, dashClean(p.question))
		if len(p.choices) > 0 {
			for i, c := range p.choices {
				l.put(sPrompt, "  ")
				start := l.w
				label := "[" + c.key + "] " + c.label
				if i == p.def {
					label += "*"
				}
				l.put(sPromptKey, label)
				r.zone(dashPromptID(i), start, l.w-1, y)
			}
			l.put(sPrompt, "   enter "+p.choices[p.def].label+" · esc cancel ")
		} else {
			l.put(sPrompt, "   ")
			l.put(sPromptKey, " y ")
			l.put(sPrompt, " confirm  ")
			l.put(sPromptKey, " n ")
			l.put(sPrompt, " cancel ")
		}
		l.fill(sPrompt)
		return string(l.b)
	}
	if m.filtering {
		target := m.filters[m.activeTab]
		label := " filter "
		if m.activeTab == tabLogs && m.viewerFocus {
			target, label = m.logSearch, " search "
		}
		l.put(sKey, " /")
		l.put(sMuted, label)
		l.put(sBold, dashClean(target))
		l.put(sAccent, "▏")
		info := "  " + m.filterInfo() + " · enter keep · esc clear"
		l.put(sMuted, info)
		return l.String()
	}
	l.pad(sNone, 1)
	if m.flash != "" {
		st := sOK
		if m.flashErr {
			st = sCrit
		}
		msg := dashClean(m.flash)
		maxW := W * 2 / 3
		if !m.flashErr && dashWidth(msg) > maxW {
			cut, _ := dashCut(msg, maxW-1)
			msg = cut + "…"
		}
		l.put(st, msg)
		l.pad(sNone, 3)
	} else if m.busy != "" {
		l.put(sMuted, "⟳ "+m.busy+"…")
		l.pad(sNone, 3)
	}
	// "? help" is pinned to the right edge; other hints fill what is left.
	pinned := dhint{"?", "help"}
	if m.overlay == dashOverlayHelp {
		pinned = dhint{"esc", "close"}
	}
	pinW := dashWidth(pinned.key) + 1 + dashWidth(pinned.label) + 1
	for _, h := range m.hints() {
		if h == pinned || h.key == "?" {
			continue
		}
		need := dashWidth(h.key) + 1 + dashWidth(h.label) + 2
		if l.room()-pinW < need {
			break
		}
		l.put(sKey, h.key)
		l.put(sMuted, " "+h.label+"  ")
	}
	if l.room() >= pinW {
		l.pad(sNone, l.room()-pinW)
		l.put(sKey, pinned.key)
		l.put(sMuted, " "+pinned.label+" ")
	}
	return l.String()
}

func (m *model) filterInfo() string {
	switch m.activeTab {
	case tabServers:
		return strconv.Itoa(len(m.views.servers)) + " of " + strconv.Itoa(len(m.base.rows)) + " servers"
	case tabServices:
		return strconv.Itoa(len(m.views.services)) + " of " + strconv.Itoa(len(m.base.services)) + " services"
	case tabLogs:
		if m.viewerFocus {
			if lp := m.selectedLog(); lp != nil {
				texts, _, _ := m.logLines(lp)
				return strconv.Itoa(len(texts)) + " matching lines"
			}
			return ""
		}
		return strconv.Itoa(len(m.views.logs)) + " of " + strconv.Itoa(len(m.snapshot.CachedLogs)) + " logs"
	case tabAlerts:
		return strconv.Itoa(len(m.views.alerts)) + " of " + strconv.Itoa(len(m.base.alerts)) + " alerts"
	case tabOps:
		return strconv.Itoa(len(m.views.audit)) + " of " + strconv.Itoa(len(m.snapshot.RecentAudit)) + " entries"
	}
	return ""
}

// ---------------------------------------------------------------------------
// Body dispatch
// ---------------------------------------------------------------------------

func (m *model) renderBody(r *drctx, body drect) []string {
	var lines []string
	switch {
	case m.loading && m.snapshot.GeneratedAt.IsZero():
		lines = dashCenterMessage(r, body.w, body.h, []string{"Loading fleet data…", "reading servers, alerts, logs and audit trail"})
	case m.err != nil && m.snapshot.GeneratedAt.IsZero():
		lines = dashCenterMessage(r, body.w, body.h, []string{"Dashboard unavailable", dashFirstLine(m.err.Error()), "press r to retry or q to quit"})
	case m.overlay == dashOverlayHelp:
		lines = m.renderHelp(r, body)
	default:
		switch m.activeTab {
		case tabServers:
			lines = m.renderServersTab(r, body)
		case tabServices:
			lines = m.renderServicesTab(r, body)
		case tabLogs:
			lines = m.renderLogsTab(r, body)
		case tabAlerts:
			lines = m.renderAlertsTab(r, body)
		case tabOps:
			lines = m.renderOpsTab(r, body)
		default:
			lines = m.renderOverview(r, body)
		}
	}
	// Guarantee the exact body height.
	for len(lines) < body.h {
		lines = append(lines, dashRunSpace.n(body.w))
	}
	return lines[:body.h]
}

// place stacks/aligns rendered boxes into the body using geometry rects.
func dplace(body drect, parts ...dplaced) []string {
	out := dashBlankLines(body.w, body.h)
	// Build row by row: parts never overlap and are laid out on a grid, so for
	// each row concatenate the parts covering it in x order.
	for y := 0; y < body.h; y++ {
		var row []dplaced
		for _, p := range parts {
			if y >= p.r.y && y < p.r.y+p.r.h && len(p.lines) > y-p.r.y {
				row = append(row, p)
			}
		}
		if len(row) == 0 {
			continue
		}
		sort.Slice(row, func(i, j int) bool { return row[i].r.x < row[j].r.x })
		var sb strings.Builder
		x := 0
		for _, p := range row {
			if p.r.x > x {
				sb.WriteString(dashRunSpace.n(p.r.x - x))
			}
			sb.WriteString(p.lines[y-p.r.y])
			x = p.r.x + p.r.w
		}
		if x < body.w {
			sb.WriteString(dashRunSpace.n(body.w - x))
		}
		out[y] = sb.String()
	}
	return out
}

type dplaced struct {
	r     drect
	lines []string
}

// ---------------------------------------------------------------------------
// Tables
// ---------------------------------------------------------------------------

type dcol struct {
	key   int
	title string
	w     int
	grow  int // extra cells this column may take; -1 = unlimited
	right bool
	drop  int // 0 = never dropped; higher = dropped first
	group int // columns sharing a non-zero group are dropped together
}

// fitCols drops low-priority columns until the table fits avail cells (1-cell
// gaps, plus a 1-cell cursor gutter the caller reserves), then hands spare
// width to growable columns in order.
// dashMaxW is the widest of n values (capped at limit), for content-aware
// column widths.
func dashMaxW(n, limit int, get func(i int) string) int {
	w := 0
	for i := 0; i < n && w < limit; i++ {
		if x := dashWidth(dashClean(get(i))); x > w {
			w = x
		}
	}
	return min(w, limit)
}

// growTo is the grow budget that lets a column of base width w reach content
// width cw.
func growTo(w, cw int) int { return max(cw-w, 0) }

func dashFitCols(cols []dcol, avail int) []dcol {
	cs := append([]dcol(nil), cols...)
	used := func() int {
		t := 0
		for _, c := range cs {
			t += c.w
		}
		return t + max(len(cs)-1, 0)
	}
	// Never-dropped columns claim their content width (w+grow) before any
	// optional column is kept, so names are not truncated to make room for
	// nice-to-have columns.
	desired := func() int {
		t := 0
		for _, c := range cs {
			t += c.w
			if c.drop == 0 && c.grow > 0 {
				t += c.grow
			}
		}
		return t + max(len(cs)-1, 0)
	}
	for desired() > avail {
		worst := -1
		for i, c := range cs {
			if c.drop > 0 && (worst < 0 || c.drop > cs[worst].drop) {
				worst = i
			}
		}
		if worst < 0 {
			break
		}
		if g := cs[worst].group; g != 0 {
			kept := cs[:0]
			for _, c := range cs {
				if c.group != g {
					kept = append(kept, c)
				}
			}
			cs = kept
			continue
		}
		cs = append(cs[:worst], cs[worst+1:]...)
	}
	extra := avail - used()
	for i := range cs {
		if extra <= 0 {
			break
		}
		if cs[i].grow > 0 {
			g := min(cs[i].grow, extra)
			cs[i].w += g
			extra -= g
		}
	}
	for i := range cs {
		if extra <= 0 {
			break
		}
		if cs[i].grow < 0 {
			cs[i].w += extra
			extra = 0
		}
	}
	if extra < 0 {
		// Still too wide (tiny terminal): shrink the widest column.
		wi := 0
		for i := range cs {
			if cs[i].w > cs[wi].w {
				wi = i
			}
		}
		cs[wi].w = max(cs[wi].w+extra, 1)
	}
	return cs
}

// tableHeader renders the column titles; sortKey (or -1) is marked with an
// arrow.
func dashTableHeader(r *drctx, iw int, cols []dcol, sortKey int, desc bool, zoneFor func(key int) string, x0, y int) string {
	l := newDLine(r.p, iw)
	l.pad(sNone, 1)
	for i, c := range cols {
		if i > 0 {
			l.pad(sNone, 1)
		}
		st := sColHead
		title := c.title
		if c.key == sortKey && sortKey >= 0 {
			st = sColHeadSort
			if desc {
				title += "▼"
			} else {
				title += "▲"
			}
		}
		start := l.w
		if c.right {
			l.textR(st, title, c.w)
		} else {
			l.text(st, title, c.w)
		}
		if zoneFor != nil {
			if id := zoneFor(c.key); id != "" {
				r.zone(id, x0+start, x0+l.w-1, y)
			}
		}
	}
	return l.String()
}

// emptyBody returns inner lines with a centred notice.
func dashEmptyBody(r *drctx, iw, ih int, msg string, st dstyle) []string {
	out := make([]string, 0, ih)
	l := newDLine(r.p, iw)
	for i := 0; i < ih; i++ {
		l.reset(iw)
		if i == min(1, ih-1) {
			l.pad(sNone, 1)
			l.text(st, msg, iw-1)
		}
		out = append(out, l.String())
	}
	return out
}

func dashRangeMeta(off, rows, n int) string {
	if n == 0 {
		return "0"
	}
	end := min(off+rows, n)
	if off == 0 && end == n {
		return strconv.Itoa(n)
	}
	return strconv.Itoa(off+1) + "–" + strconv.Itoa(end) + " of " + strconv.Itoa(n)
}

func dashJoinMeta(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

func dashFilterMeta(f string) string {
	if strings.TrimSpace(f) == "" {
		return ""
	}
	return "/" + f
}

// listBox renders a table in a box at rect b (body-relative), marking each
// visible row as a zone.
func (m *model) listBox(r *drctx, body drect, b drect, tab dashboardTab, title, meta string, cols []dcol, sortKey int, desc bool,
	colZone func(int) string, n int, cell func(row int, c dcol, l *dline, sel bool), empty string) []string {
	box := &dbox{p: r.p, w: b.w, h: b.h, title: title, meta: meta, focus: !(tab == tabLogs && m.viewerFocus)}
	iw, ih := box.inner()
	ax, ay := body.x+b.x+2, body.y+b.y+1 // absolute origin of inner content
	if n == 0 {
		box.lines = dashEmptyBody(r, iw, ih, empty, sMuted)
		return box.render()
	}
	box.lines = make([]string, 0, ih)
	if ih > 0 {
		box.lines = append(box.lines, dashTableHeader(r, iw, cols, sortKey, desc, colZone, ax, ay))
	}
	cur := *m.cursorPtr(tab)
	off := m.offsets[tab]
	rows := ih - 1
	l := newDLine(r.p, iw)
	for i := off; i < n && i < off+rows; i++ {
		l.reset(iw)
		sel := i == cur
		fill := sNone
		if sel {
			fill = sSel
			if tab == tabLogs && m.viewerFocus {
				fill = sSelBlur
			}
			l.putW(fill, "›", 1)
		} else {
			l.pad(sNone, 1)
		}
		for ci, c := range cols {
			if ci > 0 {
				l.pad(fill, 1)
			}
			start := l.w
			cell(i, c, l, sel)
			// Pad/crop each cell to its exact width.
			if w := l.w - start; w < c.w {
				l.pad(fill, c.w-w)
			}
		}
		l.fill(fill)
		box.lines = append(box.lines, string(l.b))
		r.zone(dashRowID(int(tab), i), ax-1, ax+iw, ay+1+(i-off))
	}
	return box.render()
}

// cellText writes a table cell; selected rows use the selection style.
func dashCell(l *dline, c dcol, st dstyle, text string, sel bool) {
	if sel {
		st = sSel
	}
	if c.right {
		l.textR(st, text, c.w)
	} else {
		l.text(st, text, c.w)
	}
}

// ---------------------------------------------------------------------------
// Overview
// ---------------------------------------------------------------------------

func (m *model) renderOverview(r *drctx, body drect) []string {
	W, H := body.w, body.h
	f := &m.base.fleet
	kpiH := min(6, H)
	rest := H - kpiH
	hotH, botH := 0, 0
	switch {
	case rest >= 10:
		hotH = clamp(rest*45/100, 5, 3+dashTopN/2)
		// Do not reserve rows the hot lists cannot fill.
		entries := max(len(f.topCPU), max(len(f.topMem), len(f.topDisk)))
		hotH = min(hotH, max(entries+2, 5))
		botH = rest - hotH
	case rest >= 4:
		hotH = rest
	}
	var parts []dplaced

	// KPI row.
	if W >= 96 {
		w1 := W / 3
		w2 := W / 3
		w3 := W - w1 - w2
		parts = append(parts,
			dplaced{drect{0, 0, w1, kpiH}, m.fleetBox(r, w1, kpiH)},
			dplaced{drect{w1, 0, w2, kpiH}, m.alertsKPIBox(r, w2, kpiH)},
			dplaced{drect{w1 + w2, 0, w3, kpiH}, m.resourcesBox(r, w3, kpiH)},
		)
		r.zone(dashOverviewBoxID(0), body.x, body.x+w1-1, body.y)
		r.zone(dashOverviewBoxID(1), body.x+w1, body.x+w1+w2-1, body.y)
	} else {
		w1 := W / 2
		parts = append(parts,
			dplaced{drect{0, 0, w1, kpiH}, m.fleetBox(r, w1, kpiH)},
			dplaced{drect{w1, 0, W - w1, kpiH}, m.alertsKPIBox(r, W-w1, kpiH)},
		)
		r.zone(dashOverviewBoxID(0), body.x, body.x+w1-1, body.y)
		r.zone(dashOverviewBoxID(1), body.x+w1, body.x+W-1, body.y)
	}

	// Hot spots.
	if hotH > 0 {
		y := kpiH
		if W >= 90 {
			w1, w2 := W/3, W/3
			w3 := W - w1 - w2
			parts = append(parts,
				dplaced{drect{0, y, w1, hotH}, m.hotBox(r, body, drect{0, y, w1, hotH}, 0, "Top CPU", f.topCPU, func(s *core.ServerRecord) float64 { return s.Metrics.CPUPercent }, dashCPUWarn, dashCPUCrit)},
				dplaced{drect{w1, y, w2, hotH}, m.hotBox(r, body, drect{w1, y, w2, hotH}, 1, "Top memory", f.topMem, func(s *core.ServerRecord) float64 { return s.Metrics.MemoryPercent }, dashMemWarn, dashMemCrit)},
				dplaced{drect{w1 + w2, y, w3, hotH}, m.hotBox(r, body, drect{w1 + w2, y, w3, hotH}, 2, "Top disk", f.topDisk, func(s *core.ServerRecord) float64 { return s.Metrics.DiskPercent }, dashDiskWarn, dashDiskCrit)},
			)
		} else {
			parts = append(parts, dplaced{drect{0, y, W, hotH}, m.hotCombinedBox(r, body, drect{0, y, W, hotH})})
		}
	}

	// Open alerts + recent activity.
	if botH > 0 {
		y := kpiH + hotH
		w1 := W * 55 / 100
		parts = append(parts,
			dplaced{drect{0, y, w1, botH}, m.openAlertsBox(r, body, drect{0, y, w1, botH})},
			dplaced{drect{w1, y, W - w1, botH}, m.activityBox(r, W-w1, botH)},
		)
	}
	return dplace(drect{0, 0, W, H}, parts...)
}

func (m *model) fleetBox(r *drctx, w, h int) []string {
	f := &m.base.fleet
	b := &dbox{p: r.p, w: w, h: h, title: "Fleet", meta: strconv.Itoa(f.total) + " servers"}
	iw, _ := b.inner()
	l := newDLine(r.p, iw)
	var lines []string

	full := []dseg{
		{sOK, "● "}, {sBold, strconv.Itoa(f.online)}, {sMuted, " online   "},
		{sWarn, "◐ "}, {sBold, strconv.Itoa(f.degraded)}, {sMuted, " degraded   "},
		{sCrit, "○ "}, {sBold, strconv.Itoa(f.offline)}, {sMuted, " offline"},
	}
	l.putFit(full, []dseg{
		{sOK, "●"}, {sBold, strconv.Itoa(f.online)}, {sMuted, " up  "},
		{sWarn, "◐"}, {sBold, strconv.Itoa(f.degraded)}, {sMuted, " deg  "},
		{sCrit, "○"}, {sBold, strconv.Itoa(f.offline)}, {sMuted, " down"},
	})
	lines = append(lines, l.String())

	// Stacked health bar.
	l.reset(iw)
	if f.total > 0 {
		cells := func(n int) int {
			if n == 0 {
				return 0
			}
			return max(n*iw/f.total, 1)
		}
		deg, off := cells(f.degraded), cells(f.offline)
		on := iw - deg - off
		l.putW(sOK, dashRunFull.n(on), on)
		l.putW(sWarn, dashRunMid.n(deg), deg)
		l.putW(sCrit, dashRunTrack.n(off), off)
	} else {
		l.put(sMuted, "no servers yet — add one with `fleet server add`")
	}
	lines = append(lines, l.String())

	l.reset(iw)
	svcLine := func(label string) []dseg {
		segs := []dseg{{sBold, strconv.Itoa(f.services)}, {sMuted, label}}
		if f.failedSvc > 0 {
			segs = append(segs, dseg{sMuted, " · "}, dseg{sCrit, strconv.Itoa(f.failedSvc) + " failed"})
		}
		if f.startingSvc > 0 {
			segs = append(segs, dseg{sMuted, " · "}, dseg{sWarn, strconv.Itoa(f.startingSvc) + " starting"})
		}
		return segs
	}
	l.putFit(svcLine(" services"), svcLine(" svc"))
	lines = append(lines, l.String())

	l.reset(iw)
	l.put(sMuted, "agents ")
	if f.newestAgent != "" {
		l.put(sBold, f.newestAgent)
	} else {
		l.put(sDim, "version unknown")
	}
	if f.agentOld > 0 {
		l.put(sMuted, " · ")
		l.put(sWarn, strconv.Itoa(f.agentOld)+" behind")
	}
	if f.agentUnknown > 0 && f.newestAgent != "" {
		l.put(sMuted, " · "+strconv.Itoa(f.agentUnknown)+" unknown")
	}
	lines = append(lines, l.String())
	b.lines = lines
	return b.render()
}

func (m *model) alertsKPIBox(r *drctx, w, h int) []string {
	s := m.base.stats
	b := &dbox{p: r.p, w: w, h: h, title: "Alerts", meta: strconv.Itoa(s.Total) + " total"}
	iw, _ := b.inner()
	l := newDLine(r.p, iw)
	var lines []string
	counts := func(sep string, glyphSep string, crit, warn, info string) []dseg {
		one := func(n int, st dstyle, glyph, label string) []dseg {
			if n == 0 {
				st = sDim
			}
			return []dseg{{st, glyph + glyphSep}, {sBold, strconv.Itoa(n)}, {sMuted, " " + label}}
		}
		out := one(s.OpenCritical, sCrit, "▲", crit)
		out = append(out, dseg{sNone, sep})
		out = append(out, one(s.OpenWarning, sWarn, "▲", warn)...)
		out = append(out, dseg{sNone, sep})
		return append(out, one(s.OpenInfo, sInfo, "●", info)...)
	}
	l.putFit(counts("   ", " ", "critical", "warning", "info"), counts("  ", "", "crit", "warn", "info"))
	lines = append(lines, l.String())

	l.reset(iw)
	states := func(acked, supp string) []dseg {
		return []dseg{{sBold, strconv.Itoa(s.Open)}, {sMuted, " open · "}, {sBold, strconv.Itoa(s.Acknowledged)},
			{sMuted, " " + acked + " · "}, {sBold, strconv.Itoa(s.Suppressed)}, {sMuted, " " + supp}}
	}
	l.putFit(states("acked", "suppressed"), states("ack", "supp"))
	lines = append(lines, l.String())

	l.reset(iw)
	if n := m.base.fleet.alertingServers; n > 0 {
		l.put(sWarn, strconv.Itoa(n))
		l.put(sMuted, " servers with open alerts")
	} else if s.Total == 0 {
		l.put(sOK, "✓ no alerts")
	} else {
		l.put(sOK, "✓ nothing open")
	}
	lines = append(lines, l.String())

	l.reset(iw)
	if idx := m.openAlertOrder(); len(idx) > 0 {
		a := &m.base.alerts[idx[0]]
		l.put(sMuted, "latest ")
		l.put(dashSevStyle(a.Severity), dashSevShort(a.Severity))
		l.put(sNone, " ")
		l.text(sNone, a.Message, max(l.room()-5, 1))
		l.textR(sDim, dashAge(r.now, dashAlertTime(*a)), l.room())
	}
	lines = append(lines, l.String())
	b.lines = lines
	return b.render()
}

func (m *model) resourcesBox(r *drctx, w, h int) []string {
	f := &m.base.fleet
	b := &dbox{p: r.p, w: w, h: h, title: "Resources", meta: "avg of " + strconv.Itoa(f.reporting) + " reporting"}
	iw, _ := b.inner()
	l := newDLine(r.p, iw)
	var lines []string
	row := func(label string, avg, p95, warn, crit float64) {
		l.reset(iw)
		l.put(sMuted, label)
		tail := 5
		showP95 := iw >= 34
		if showP95 {
			tail += 10
		}
		bw := max(iw-dashWidth(label)-tail, 3)
		if f.reporting == 0 {
			l.put(sDim, "no metrics yet")
			lines = append(lines, l.String())
			return
		}
		l.bar(avg, bw, dashPctStyle(avg, warn, crit))
		l.textR(dashPctStyle(avg, warn, crit), dashPct(avg), 5)
		if showP95 {
			l.put(sDim, "  p95 ")
			l.textR(dashPctStyle(p95, warn, crit), dashPct(p95), 4)
		}
		lines = append(lines, l.String())
	}
	row("CPU  ", f.avgCPU, f.p95CPU, dashCPUWarn, dashCPUCrit)
	row("MEM  ", f.avgMem, f.p95Mem, dashMemWarn, dashMemCrit)
	row("DISK ", f.avgDisk, f.p95Disk, dashDiskWarn, dashDiskCrit)
	l.reset(iw)
	hotSegs := func(prefix string) []dseg {
		one := func(n int, label string) []dseg {
			st := sDim
			if n > 0 {
				st = sWarn
			}
			return []dseg{{st, strconv.Itoa(n)}, {sMuted, " " + label}}
		}
		out := []dseg{{sMuted, prefix}}
		out = append(out, one(f.hotCPU, "cpu · ")...)
		out = append(out, one(f.hotMem, "mem · ")...)
		return append(out, one(f.hotDisk, "disk")...)
	}
	l.putFit(hotSegs("over threshold "), hotSegs("hot "))
	lines = append(lines, l.String())
	b.lines = lines
	return b.render()
}

func (m *model) hotBox(r *drctx, body, at drect, metric int, title string, list []int, value func(*core.ServerRecord) float64, warn, crit float64) []string {
	b := &dbox{p: r.p, w: at.w, h: at.h, title: title}
	iw, ih := b.inner()
	if len(list) == 0 {
		b.lines = dashEmptyBody(r, iw, ih, "no metrics reported yet", sDim)
		return b.render()
	}
	l := newDLine(r.p, iw)
	bw := clamp(iw/3, 4, 16)
	nameW := max(iw-bw-6, 4)
	for i := 0; i < len(list) && i < ih; i++ {
		s := &m.snapshot.Servers[m.base.rows[list[i]].idx]
		v := value(s)
		l.reset(iw)
		l.text(sNone, s.Name, nameW)
		l.pad(sNone, 1)
		l.bar(v, bw, dashPctStyle(v, warn, crit))
		l.textR(dashPctStyle(v, warn, crit), dashPct(v), 5)
		b.lines = append(b.lines, l.String())
		r.zone(dashHotID(metric, i), body.x+at.x+1, body.x+at.x+at.w-2, body.y+at.y+1+i)
	}
	return b.render()
}

func (m *model) hotCombinedBox(r *drctx, body, at drect) []string {
	f := &m.base.fleet
	b := &dbox{p: r.p, w: at.w, h: at.h, title: "Hot spots", meta: "highest of cpu/mem/disk"}
	iw, ih := b.inner()
	seen := map[int]bool{}
	var list []int
	for _, lst := range [][]int{f.topCPU, f.topMem, f.topDisk} {
		for _, i := range lst {
			if !seen[i] {
				seen[i] = true
				list = append(list, i)
			}
		}
	}
	peak := func(i int) float64 {
		s := &m.snapshot.Servers[m.base.rows[i].idx]
		return dashMax64(s.Metrics.CPUPercent, s.Metrics.MemoryPercent, s.Metrics.DiskPercent)
	}
	sort.SliceStable(list, func(a, c int) bool { return peak(list[a]) > peak(list[c]) })
	if len(list) == 0 {
		b.lines = dashEmptyBody(r, iw, ih, "no metrics reported yet", sDim)
		return b.render()
	}
	l := newDLine(r.p, iw)
	nameW := 6
	for i := 0; i < len(list) && i < ih; i++ {
		nameW = max(nameW, dashWidth(dashClean(m.snapshot.Servers[m.base.rows[list[i]].idx].Name))+2)
	}
	nameW = min(nameW, max(iw-30, 6))
	for i := 0; i < len(list) && i < ih; i++ {
		s := &m.snapshot.Servers[m.base.rows[list[i]].idx]
		l.reset(iw)
		l.text(sNone, s.Name, nameW)
		for _, mv := range []struct {
			label      string
			v          float64
			warn, crit float64
		}{{" cpu ", s.Metrics.CPUPercent, dashCPUWarn, dashCPUCrit}, {" mem ", s.Metrics.MemoryPercent, dashMemWarn, dashMemCrit}, {" disk ", s.Metrics.DiskPercent, dashDiskWarn, dashDiskCrit}} {
			l.put(sDim, mv.label)
			l.textR(dashPctStyle(mv.v, mv.warn, mv.crit), dashPct(mv.v), 4)
		}
		b.lines = append(b.lines, l.String())
		r.zone(dashHotID(0, i), body.x+at.x+1, body.x+at.x+at.w-2, body.y+at.y+1+i)
	}
	return b.render()
}

func dashMax64(vs ...float64) float64 {
	out := vs[0]
	for _, v := range vs[1:] {
		if v > out {
			out = v
		}
	}
	return out
}

// openAlertOrder returns indices of open alerts, most severe then newest first.
func (m *model) openAlertOrder() []int {
	var idx []int
	for i := range m.base.alerts {
		if core.AlertState(m.base.alerts[i], m.base.now) == "open" {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, c int) bool {
		x, y := &m.base.alerts[idx[a]], &m.base.alerts[idx[c]]
		if rx, ry := dashSevRank(x.Severity), dashSevRank(y.Severity); rx != ry {
			return rx > ry
		}
		return dashAlertTime(*x).After(dashAlertTime(*y))
	})
	return idx
}

func dashSevStyle(s fleetalerts.Severity) dstyle {
	switch s {
	case fleetalerts.SeverityCritical:
		return sCrit
	case fleetalerts.SeverityWarning:
		return sWarn
	default:
		return sInfo
	}
}

func dashSevShort(s fleetalerts.Severity) string {
	switch s {
	case fleetalerts.SeverityCritical:
		return "CRIT"
	case fleetalerts.SeverityWarning:
		return "WARN"
	case fleetalerts.SeverityInfo:
		return "INFO"
	}
	return strings.ToUpper(string(s))
}

func (m *model) openAlertsBox(r *drctx, body, at drect) []string {
	order := m.openAlertOrder()
	b := &dbox{p: r.p, w: at.w, h: at.h, title: "Open alerts", meta: strconv.Itoa(len(order))}
	if len(order) > 0 && dashSevRank(m.base.alerts[order[0]].Severity) == 3 {
		b.metaSty = sCrit
	}
	iw, ih := b.inner()
	if len(order) == 0 {
		msg := "✓ No open alerts — the fleet is quiet."
		b.lines = dashEmptyBody(r, iw, ih, msg, sOK)
		return b.render()
	}
	l := newDLine(r.p, iw)
	srvW := clamp(iw/5, 8, 18)
	for i := 0; i < len(order) && i < ih; i++ {
		a := &m.base.alerts[order[i]]
		l.reset(iw)
		l.text(dashSevStyle(a.Severity), dashSevShort(a.Severity), 4)
		l.pad(sNone, 1)
		l.text(sMuted, dashIfEmpty(a.Server), srvW)
		l.pad(sNone, 1)
		l.text(sNone, a.Message, max(l.room()-5, 1))
		l.textR(sDim, dashAge(r.now, dashAlertTime(*a)), l.room())
		b.lines = append(b.lines, l.String())
		r.zone(dashAlertRowID(i), body.x+at.x+1, body.x+at.x+at.w-2, body.y+at.y+1+i)
	}
	return b.render()
}

func (m *model) activityBox(r *drctx, w, h int) []string {
	audit := m.snapshot.RecentAudit
	b := &dbox{p: r.p, w: w, h: h, title: "Recent activity"}
	iw, ih := b.inner()
	if len(audit) == 0 {
		b.lines = dashEmptyBody(r, iw, ih, "No audit activity yet.", sMuted)
		return b.render()
	}
	l := newDLine(r.p, iw)
	actW := clamp(iw/3, 10, 22)
	opW := 0
	if iw >= 46 {
		opW = clamp(iw/6, 6, 14)
	}
	for i := 0; i < len(audit) && i < ih; i++ {
		e := &audit[i]
		l.reset(iw)
		l.text(sDim, e.Timestamp.Local().Format("15:04"), 5)
		l.pad(sNone, 1)
		l.text(sAccent, e.Action, actW)
		l.pad(sNone, 1)
		l.text(sNone, e.Target, max(l.room()-opW-1, 1))
		if opW > 0 {
			l.pad(sNone, 1)
			l.textR(sDim, e.Operator, l.room())
		}
		b.lines = append(b.lines, l.String())
	}
	return b.render()
}

// ---------------------------------------------------------------------------
// Servers
// ---------------------------------------------------------------------------

const (
	scName = iota
	scStatus
	scMode
	scCPUBar
	scCPU
	scMemBar
	scMem
	scDiskBar
	scDisk
	scLoad
	scAgent
	scSeen
	scTags
)

var serverColSort = map[int]serverSortCol{
	scName: ssName, scStatus: ssStatus, scMode: ssMode, scCPU: ssCPU, scCPUBar: ssCPU,
	scMem: ssMem, scMemBar: ssMem, scDisk: ssDisk, scDiskBar: ssDisk, scLoad: ssLoad,
	scAgent: ssAgent, scSeen: ssSeen,
}

func (m *model) serverCols(iw int) []dcol {
	nameW := 8
	for _, i := range m.views.servers {
		if n := dashWidth(m.snapshot.Servers[m.base.rows[i].idx].Name); n > nameW {
			nameW = n
		}
	}
	nameW = min(nameW, 28)
	cols := []dcol{
		{key: scName, title: "NAME", w: 8, grow: nameW - 8},
		{key: scStatus, title: "STATUS", w: 10},
		{key: scMode, title: "MODE", w: 7, drop: 5},
		{key: scCPU, title: "CPU", w: 5, right: true},
		{key: scCPUBar, w: 8, drop: 9, group: 1},
		{key: scMem, title: "MEM", w: 5, right: true},
		{key: scMemBar, w: 8, drop: 9, group: 1},
		{key: scDisk, title: "DISK", w: 5, right: true},
		{key: scDiskBar, w: 8, drop: 9, group: 1},
		{key: scLoad, title: "LOAD", w: 5, right: true, drop: 3},
		{key: scAgent, title: "AGENT", w: 7, drop: 4},
		{key: scSeen, title: "SEEN", w: 5, right: true, drop: 2},
		{key: scTags, title: "TAGS", w: 8, grow: -1, drop: 6},
	}
	cols = dashFitCols(cols, iw-1)
	return cols
}

func (m *model) renderServersTab(r *drctx, body drect) []string {
	g := m.geom(tabServers)
	if g.hidden && m.zoom {
		return m.serverDetailBox(r, body.w, body.h)
	}
	list := m.serverListBox(r, body, g.list)
	if g.hidden {
		return list
	}
	return dplace(drect{0, 0, body.w, body.h}, dplaced{g.list, list}, dplaced{g.detail, m.serverDetailBox(r, g.detail.w, g.detail.h)})
}

func (m *model) serverListBox(r *drctx, body, at drect) []string {
	iw := max(at.w-4, 1)
	cols := m.serverCols(iw)
	sortKey := -1
	for _, c := range cols {
		if sc, ok := serverColSort[c.key]; ok && sc == m.sortCol && c.title != "" {
			sortKey = c.key
			break
		}
	}
	n := len(m.views.servers)
	meta := dashJoinMeta(dashFilterMeta(m.filters[tabServers]), dashRangeMeta(m.offsets[tabServers], max(at.h-3, 0), n), "sort "+serverSortNames[m.sortCol])
	empty := "No servers yet — add one with `fleet server add`."
	if len(m.base.rows) > 0 {
		empty = "No servers match “" + m.filters[tabServers] + "” — esc clears the filter."
	}
	colZone := func(key int) string {
		if sc, ok := serverColSort[key]; ok {
			return dashColID(int(sc))
		}
		return ""
	}
	return m.listBox(r, body, at, tabServers, "Servers", meta, cols, sortKey, m.sortDesc, colZone, n, func(row int, c dcol, l *dline, sel bool) {
		sr := &m.base.rows[m.views.servers[row]]
		s := &m.snapshot.Servers[sr.idx]
		m.serverCell(r, l, c, s, sr, sel)
	}, empty)
}

func (m *model) serverCell(r *drctx, l *dline, c dcol, s *core.ServerRecord, sr *dashSrvRow, sel bool) {
	stale := !s.Observed.Reachable
	pct := func(v, warn, crit float64) {
		st := dashPctStyle(v, warn, crit)
		if stale {
			st = sDim
		}
		txt := dashPct(v)
		if !sr.hasMetrics {
			txt, st = "-", sDim
		}
		dashCell(l, c, st, txt, sel)
	}
	bar := func(v, warn, crit float64) {
		if !sr.hasMetrics {
			dashCell(l, c, sDim, "", sel)
			return
		}
		st := dashPctStyle(v, warn, crit)
		if stale {
			st = sDim
		}
		if sel {
			st = sSel
		}
		l.bar(v, c.w, st)
	}
	switch c.key {
	case scName:
		dashCell(l, c, sBold, s.Name, sel)
	case scStatus:
		st := sr.status.style()
		if sel {
			st = sSel
		}
		l.putW(st, sr.status.glyph(), 1)
		dashCell(l, dcol{w: c.w - 1}, st, " "+sr.status.label(), sel)
	case scMode:
		dashCell(l, c, sMuted, string(s.Mode), sel)
	case scCPUBar:
		bar(s.Metrics.CPUPercent, dashCPUWarn, dashCPUCrit)
	case scCPU:
		pct(s.Metrics.CPUPercent, dashCPUWarn, dashCPUCrit)
	case scMemBar:
		bar(s.Metrics.MemoryPercent, dashMemWarn, dashMemCrit)
	case scMem:
		pct(s.Metrics.MemoryPercent, dashMemWarn, dashMemCrit)
	case scDiskBar:
		bar(s.Metrics.DiskPercent, dashDiskWarn, dashDiskCrit)
	case scDisk:
		pct(s.Metrics.DiskPercent, dashDiskWarn, dashDiskCrit)
	case scLoad:
		txt := "-"
		if sr.hasMetrics {
			txt = strconv.FormatFloat(s.Metrics.Load1, 'f', 2, 64)
		}
		st := sNone
		if stale || !sr.hasMetrics {
			st = sDim
		}
		dashCell(l, c, st, txt, sel)
	case scAgent:
		st := sMuted
		if sr.agentOld {
			st = sWarn
		}
		dashCell(l, c, st, sr.agent, sel)
	case scSeen:
		st := sMuted
		if stale {
			st = sCrit
		}
		dashCell(l, c, st, dashAge(r.now, s.Observed.LastSeen), sel)
	case scTags:
		dashCell(l, c, sDim, sr.tags, sel)
	}
}

func (m *model) serverDetailBox(r *drctx, w, h int) []string {
	s, sr := m.selectedServer()
	b := &dbox{p: r.p, w: w, h: h, title: "Server"}
	iw, ih := b.inner()
	if s == nil {
		b.lines = dashEmptyBody(r, iw, ih, "Select a server to see its details.", sMuted)
		return b.render()
	}
	b.title = s.Name
	b.meta = sr.status.glyph() + " " + sr.status.label()
	b.metaSty = sr.status.style()
	var lines []string
	if iw >= 96 {
		// Wide (stacked-below) pane: identity + resources on the left,
		// services / alerts / facts on the right.
		lw := iw * 52 / 100
		rw := iw - lw - 3
		left := m.serverInfoLines(r, s, sr, lw)
		right := m.serverFactLines(r, s, sr, rw)
		gap := dashRunSpace.n(3)
		for i := 0; i < max(len(left), len(right)); i++ {
			a, c := dashRunSpace.n(lw), dashRunSpace.n(rw)
			if i < len(left) {
				a = left[i]
			}
			if i < len(right) {
				c = right[i]
			}
			lines = append(lines, a+gap+c)
		}
	} else {
		lines = m.serverInfoLines(r, s, sr, iw)
		lines = append(lines, dashRunSpace.n(iw))
		lines = append(lines, m.serverFactLines(r, s, sr, iw)...)
	}
	b.lines = dashFitDetail(r, lines, iw, ih, []dhint{{"s", "ssh"}, {"f", "files"}, {"c", "reconnect"}, {"m", "metrics"}})
	return b.render()
}

// serverInfoLines is the identity and resource-history part of the server
// detail pane, cw cells wide.
func (m *model) serverInfoLines(r *drctx, s *core.ServerRecord, sr *dashSrvRow, cw int) []string {
	l := newDLine(r.p, cw)
	var lines []string
	emit := func() { lines = append(lines, l.String()); l = newDLine(r.p, cw) }

	l.put(sMuted, string(s.Mode)+" · ")
	l.put(sNone, s.Address+":"+strconv.Itoa(s.Port))
	l.put(sMuted, " · "+dashIfEmpty(s.User))
	if s.Observed.NodeName != "" {
		l.put(sMuted, " · node ")
		l.put(sNone, dashClean(s.Observed.NodeName))
	}
	emit()
	l.put(sMuted, "agent ")
	ast := sNone
	if sr.agentOld {
		ast = sWarn
	}
	l.put(ast, sr.agent)
	if osarch := strings.Trim(s.Observed.OS+"/"+s.Observed.Arch, "/"); osarch != "" {
		l.put(sMuted, " · "+dashClean(osarch))
	}
	if s.Metrics.UptimeSeconds > 0 {
		l.put(sMuted, " · up "+dashUptime(s.Metrics.UptimeSeconds))
	}
	if s.Metrics.ProcessCount > 0 {
		l.put(sMuted, " · "+strconv.FormatUint(s.Metrics.ProcessCount, 10)+" procs")
	}
	emit()
	l.put(sMuted, "last seen ")
	seenSt := sNone
	if !s.Observed.Reachable {
		seenSt = sCrit
	}
	l.put(seenSt, dashAge(r.now, s.Observed.LastSeen)+" ago")
	if !s.Metrics.Timestamp.IsZero() {
		l.put(sMuted, " · metrics "+dashAge(r.now, s.Metrics.Timestamp)+" ago")
	}
	emit()
	if s.Observed.LastError != "" && !s.Observed.Reachable {
		l.put(sCrit, "✕ ")
		l.text(sCrit, s.Observed.LastError, l.room())
		emit()
	}
	emit()

	// Resource history: sparklines from metric_snapshots when available,
	// current-value gauges otherwise.
	var hist dashHist
	haveHist := false
	if m.rt != nil {
		if e, ok := m.rt.hist[s.Name]; ok && len(e.cpu) > 1 {
			hist, haveHist = e, true
		}
	}
	switch {
	case haveHist:
		l.put(sTitle, "Resources")
		l.put(sMuted, " · last "+dashSpan(hist.last.Sub(hist.first))+", "+strconv.Itoa(len(hist.cpu))+" samples")
	case !sr.hasMetrics:
		l.put(sTitle, "Resources")
		l.put(sMuted, " · no metrics collected yet")
	default:
		l.put(sTitle, "Resources")
		l.put(sMuted, " · current")
	}
	emit()
	if !sr.hasMetrics {
		return lines
	}
	memExtra, diskExtra := "", ""
	if s.Metrics.MemoryTotalBytes > 0 && cw >= 48 {
		memExtra = "  " + dashBytes(s.Metrics.MemoryUsedBytes) + "/" + dashBytes(s.Metrics.MemoryTotalBytes)
	}
	if s.Metrics.DiskTotalBytes > 0 && cw >= 48 {
		diskExtra = "  " + dashBytes(s.Metrics.DiskUsedBytes) + "/" + dashBytes(s.Metrics.DiskTotalBytes)
	}
	pad := max(dashWidth(memExtra), dashWidth(diskExtra))
	memExtra += dashRunSpace.n(pad - dashWidth(memExtra))
	diskExtra += dashRunSpace.n(pad - dashWidth(diskExtra))
	gw := max(cw-6-6-pad, 4)
	if haveHist {
		gw = min(gw, max(len(hist.cpu), 16))
	} else {
		gw = min(gw, 40)
	}
	metricRow := func(name string, v float64, series []float64, warn, crit float64, extra string) {
		l.put(sMuted, name)
		st := dashPctStyle(v, warn, crit)
		if !s.Observed.Reachable {
			st = sDim
		}
		if haveHist {
			l.spark(series, gw, st)
		} else {
			l.bar(v, gw, st)
		}
		l.textR(st, dashPct(v), 6)
		if extra != "" {
			l.put(sDim, extra)
		}
		emit()
	}
	metricRow("CPU   ", s.Metrics.CPUPercent, hist.cpu, dashCPUWarn, dashCPUCrit, "")
	metricRow("MEM   ", s.Metrics.MemoryPercent, hist.mem, dashMemWarn, dashMemCrit, memExtra)
	metricRow("DISK  ", s.Metrics.DiskPercent, hist.disk, dashDiskWarn, dashDiskCrit, diskExtra)
	l.put(sMuted, "LOAD  ")
	l.put(sNone, strconv.FormatFloat(s.Metrics.Load1, 'f', 2, 64)+"  "+strconv.FormatFloat(s.Metrics.Load5, 'f', 2, 64)+"  "+strconv.FormatFloat(s.Metrics.Load15, 'f', 2, 64))
	l.put(sDim, "  1m 5m 15m")
	emit()
	return lines
}

// serverFactLines is the services / open alerts / tags / keys part of the
// server detail pane, cw cells wide.
func (m *model) serverFactLines(r *drctx, s *core.ServerRecord, sr *dashSrvRow, cw int) []string {
	l := newDLine(r.p, cw)
	var lines []string
	emit := func() { lines = append(lines, l.String()); l = newDLine(r.p, cw) }

	l.put(sTitle, "Services")
	l.put(sMuted, " "+strconv.Itoa(len(s.Services)))
	if sr.failedSvc > 0 {
		l.put(sCrit, " · "+strconv.Itoa(sr.failedSvc)+" failed")
	}
	emit()
	if len(s.Services) == 0 {
		l.put(sDim, "  no tracked services")
		emit()
	}
	svcNameW := 8
	for _, svc := range s.Services {
		svcNameW = max(svcNameW, dashWidth(dashClean(svc.Name)))
	}
	svcNameW = min(svcNameW, max(cw/2, 8))
	for _, svc := range s.Services {
		st, glyph := dashServiceGlyph(svc)
		l.put(st, "  "+glyph+" ")
		l.text(sNone, svc.Name, svcNameW)
		l.put(sNone, " ")
		state := serviceState(svc)
		if svc.Critical {
			l.text(st, state, max(l.room()-9, 1))
			l.put(sDim, " critical")
		} else {
			l.text(st, state, l.room())
		}
		emit()
	}

	var mine []int
	for _, i := range m.openAlertOrder() {
		if m.base.alerts[i].Server == s.Name {
			mine = append(mine, i)
		}
	}
	emit()
	l.put(sTitle, "Open alerts")
	l.put(sMuted, " "+strconv.Itoa(len(mine)))
	emit()
	if len(mine) == 0 {
		l.put(sOK, "  ✓ none")
		emit()
	}
	for _, i := range mine {
		a := &m.base.alerts[i]
		l.put(dashSevStyle(a.Severity), "  "+dashSevShort(a.Severity)+" ")
		l.text(sNone, a.Message, max(l.room()-5, 1))
		l.textR(sDim, dashAge(r.now, dashAlertTime(*a)), l.room())
		emit()
	}
	emit()
	if sr.tags != "" {
		l.put(sMuted, "tags     ")
		l.text(sNone, sr.tags, l.room())
		emit()
	}
	if fp := s.Observed.HostKeyFingerprint; fp != "" {
		l.put(sMuted, "host key ")
		l.text(sDim, fp, l.room())
		emit()
	}
	if s.Firewall.Enabled || len(s.OpenPorts) > 0 {
		l.put(sMuted, "firewall ")
		fw := "disabled"
		if s.Firewall.Enabled {
			fw = "enabled (" + strconv.Itoa(len(s.Firewall.Rules)) + " rules)"
		}
		l.put(sNone, fw)
		if len(s.OpenPorts) > 0 {
			ports := make([]string, 0, len(s.OpenPorts))
			for _, p := range s.OpenPorts {
				ports = append(ports, strconv.Itoa(p))
			}
			l.put(sMuted, " · ports ")
			l.text(sNone, strings.Join(ports, ","), l.room())
		}
		emit()
	}
	return lines
}

// fitDetail trims detail lines to the box and pins an action hint line to its
// bottom when there is room.
func dashFitDetail(r *drctx, lines []string, iw, ih int, actions []dhint) []string {
	// Drop trailing blank lines.
	for len(lines) > 0 && strings.TrimSpace(dashStripSGR(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(actions) == 0 || ih < 6 {
		if len(lines) > ih {
			lines = lines[:ih]
		}
		return lines
	}
	body := ih - 2
	if len(lines) > body {
		lines = lines[:body]
	}
	for len(lines) < body {
		lines = append(lines, dashRunSpace.n(iw))
	}
	l := newDLine(r.p, iw)
	lines = append(lines, l.String())
	l.reset(iw)
	for _, h := range actions {
		if l.room() < dashWidth(h.key)+dashWidth(h.label)+3 {
			break
		}
		l.put(sKey, h.key)
		l.put(sMuted, " "+h.label+"  ")
	}
	lines = append(lines, l.String())
	return lines
}

// stripSGR removes SGR sequences (only used on our own rendered lines).
func dashStripSGR(s string) string {
	if !strings.Contains(s, "\x1b[") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func dashSpan(d time.Duration) string {
	switch {
	case d <= 0:
		return "moment"
	case d < time.Hour:
		return strconv.Itoa(int(d.Round(time.Minute)/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.FormatFloat(d.Hours(), 'f', 1, 64) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}

func dashServiceGlyph(svc core.ServiceRecord) (dstyle, string) {
	switch dashSvcRank(svc) {
	case 0:
		return sCrit, "✕"
	case 1:
		return sCrit, "○"
	case 2:
		return sWarn, "◐"
	}
	if strings.EqualFold(svc.ActiveState, "active") {
		return sOK, "●"
	}
	return sDim, "○"
}

// ---------------------------------------------------------------------------
// Services
// ---------------------------------------------------------------------------

const (
	vcServer = iota
	vcService
	vcState
	vcCrit
	vcAction
	vcLog
	vcDesc
)

func (m *model) renderServicesTab(r *drctx, body drect) []string {
	g := m.geom(tabServices)
	if g.hidden && m.zoom {
		return m.serviceDetailBox(r, body.w, body.h)
	}
	at := g.list
	iw := max(at.w-4, 1)
	svc := func(i int) *serviceRow { return &m.base.services[m.views.services[i]] }
	nv := len(m.views.services)
	cols := dashFitCols([]dcol{
		{key: vcServer, title: "SERVER", w: 8, grow: growTo(8, dashMaxW(nv, 24, func(i int) string { return svc(i).Server.Name }))},
		{key: vcService, title: "SERVICE", w: 10, grow: growTo(10, dashMaxW(nv, 32, func(i int) string { return svc(i).Service.Name }))},
		{key: vcState, title: "STATE", w: 14, grow: growTo(14, 2+dashMaxW(nv, 24, func(i int) string { return serviceState(*svc(i).Service) }))},
		{key: vcCrit, title: "CRIT", w: 4, drop: 2},
		{key: vcAction, title: "LAST", w: 8, drop: 3},
		{key: vcLog, title: "LOG", w: 10, grow: growTo(10, dashMaxW(nv, 40, func(i int) string { return svc(i).Service.LogPath })), drop: 4},
		{key: vcDesc, title: "DESCRIPTION", w: 12, grow: -1, drop: 5},
	}, iw-1)
	n := len(m.views.services)
	meta := dashJoinMeta(dashFilterMeta(m.filters[tabServices]), dashRangeMeta(m.offsets[tabServices], max(at.h-3, 0), n))
	empty := "No tracked services yet — add one with `fleet service add`."
	if len(m.base.services) > 0 {
		empty = "No services match the filter — esc clears it."
	}
	list := m.listBox(r, body, at, tabServices, "Services", meta, cols, -1, false, nil, n, func(row int, c dcol, l *dline, sel bool) {
		sv := &m.base.services[m.views.services[row]]
		switch c.key {
		case vcServer:
			st := sNone
			if !sv.Reachable {
				st = sCrit
			}
			dashCell(l, c, st, sv.Server.Name, sel)
		case vcService:
			dashCell(l, c, sBold, sv.Service.Name, sel)
		case vcState:
			st, glyph := dashServiceGlyph(*sv.Service)
			if sel {
				st = sSel
			}
			l.putW(st, glyph, 1)
			dashCell(l, dcol{w: c.w - 1}, st, " "+serviceState(*sv.Service), sel)
		case vcCrit:
			txt := ""
			if sv.Service.Critical {
				txt = "yes"
			}
			dashCell(l, c, sWarn, txt, sel)
		case vcAction:
			dashCell(l, c, sMuted, sv.Service.LastAction, sel)
		case vcLog:
			dashCell(l, c, sDim, sv.Service.LogPath, sel)
		case vcDesc:
			dashCell(l, c, sMuted, sv.Service.Description, sel)
		}
	}, empty)
	if g.hidden {
		return list
	}
	return dplace(drect{0, 0, body.w, body.h}, dplaced{g.list, list}, dplaced{g.detail, m.serviceDetailBox(r, g.detail.w, g.detail.h)})
}

func (m *model) serviceDetailBox(r *drctx, w, h int) []string {
	sv := m.selectedService()
	b := &dbox{p: r.p, w: w, h: h, title: "Service"}
	iw, ih := b.inner()
	if sv == nil {
		b.lines = dashEmptyBody(r, iw, ih, "Select a service to see its details.", sMuted)
		return b.render()
	}
	b.title = sv.Service.Name
	st, glyph := dashServiceGlyph(*sv.Service)
	b.meta, b.metaSty = glyph+" "+serviceState(*sv.Service), st
	l := newDLine(r.p, iw)
	var lines []string
	emit := func() { lines = append(lines, l.String()); l = newDLine(r.p, iw) }
	l.put(sMuted, "server ")
	l.put(sBold, sv.Server.Name)
	sStatus := dsOnline
	if m.base != nil {
		if i, ok := m.base.byName[sv.Server.Name]; ok {
			sStatus = m.base.rows[i].status
		}
	}
	l.put(sStatus.style(), "  "+sStatus.glyph()+" "+sStatus.label())
	emit()
	kv := func(k, v string) {
		l.put(sMuted, k)
		l.text(sNone, dashIfEmpty(v), l.room())
		emit()
	}
	kv("load state   ", sv.Service.LoadState)
	kv("active state ", sv.Service.ActiveState)
	kv("sub state    ", sv.Service.SubState)
	crit := "no"
	if sv.Service.Critical {
		crit = "yes"
	}
	kv("critical     ", crit)
	kv("last action  ", sv.Service.LastAction)
	kv("log path     ", sv.Service.LogPath)
	if sv.Service.Description != "" {
		kv("description  ", sv.Service.Description)
	}
	// Cached log tail for this service, if any.
	for i := range m.snapshot.CachedLogs {
		lp := &m.snapshot.CachedLogs[i]
		if lp.Server != sv.Server.Name || lp.Service != sv.Service.Name || !lp.Available {
			continue
		}
		emit()
		l.put(sTitle, "Recent cached log")
		emit()
		room := max(ih-len(lines)-3, 0)
		start := max(len(lp.Lines)-room, 0)
		for _, line := range lp.Lines[start:] {
			l.text(sDim, "  "+line.Text, l.room())
			emit()
		}
		break
	}
	b.lines = dashFitDetail(r, lines, iw, ih, []dhint{{"L", "follow log"}, {"R", "restart"}, {"s", "ssh"}, {"f", "files"}})
	return b.render()
}

// ---------------------------------------------------------------------------
// Logs
// ---------------------------------------------------------------------------

const (
	lcServer = iota
	lcService
	lcLines
	lcLast
)

func (m *model) renderLogsTab(r *drctx, body drect) []string {
	g := m.geom(tabLogs)
	if g.hidden && m.zoom {
		return m.logViewerBox(r, body, drect{0, 0, body.w, body.h})
	}
	at := g.list
	iw := max(at.w-4, 1)
	lg := func(i int) *core.CachedLogPreview { return &m.snapshot.CachedLogs[m.views.logs[i]] }
	nl := len(m.views.logs)
	cols := dashFitCols([]dcol{
		{key: lcServer, title: "SERVER", w: 8, grow: growTo(8, dashMaxW(nl, 20, func(i int) string { return lg(i).Server }))},
		{key: lcService, title: "SERVICE", w: 10, grow: growTo(10, dashMaxW(nl, 28, func(i int) string { return lg(i).Service }))},
		{key: lcLines, title: "LINES", w: 5, right: true},
		{key: lcLast, title: "LAST LINE", w: 10, grow: -1, drop: 1},
	}, iw-1)
	n := len(m.views.logs)
	meta := dashJoinMeta(dashFilterMeta(m.filters[tabLogs]), dashRangeMeta(m.offsets[tabLogs], max(at.h-3, 0), n))
	empty := "No cached logs yet — read or follow a tracked service log to warm the cache."
	if len(m.snapshot.CachedLogs) > 0 {
		empty = "No logs match the filter — esc clears it."
	}
	list := m.listBox(r, body, at, tabLogs, "Cached logs", meta, cols, -1, false, nil, n, func(row int, c dcol, l *dline, sel bool) {
		lp := &m.snapshot.CachedLogs[m.views.logs[row]]
		switch c.key {
		case lcServer:
			dashCell(l, c, sNone, lp.Server, sel)
		case lcService:
			dashCell(l, c, sBold, lp.Service, sel)
		case lcLines:
			txt, st := "empty", sDim
			if lp.Path == "" && !lp.Available {
				txt = "·" // not read yet (periodic refreshes skip previews)
			}
			if lp.Available {
				txt, st = strconv.Itoa(len(lp.Lines)), sMuted
				if lp.Truncated {
					txt += "+"
				}
			}
			dashCell(l, c, st, txt, sel)
		case lcLast:
			txt := ""
			if len(lp.Lines) > 0 {
				txt = lp.Lines[len(lp.Lines)-1].Text
			}
			dashCell(l, c, sDim, txt, sel)
		}
	}, empty)
	if g.hidden {
		return list
	}
	return dplace(drect{0, 0, body.w, body.h}, dplaced{g.list, list}, dplaced{g.detail, m.logViewerBox(r, body, g.detail)})
}

func (m *model) logViewerBox(r *drctx, body, at drect) []string {
	lp := m.selectedLog()
	b := &dbox{p: r.p, w: at.w, h: at.h, title: "Log", focus: m.viewerFocus}
	iw, ih := b.inner()
	r.zone(dashViewerID(), body.x+at.x, body.x+at.x+at.w-1, body.y+at.y)
	for y := 1; y < at.h; y++ {
		r.zone(dashViewerID(), body.x+at.x, body.x+at.x+at.w-1, body.y+at.y+y)
	}
	if lp == nil {
		b.lines = dashEmptyBody(r, iw, ih, "Select a cached log on the left.", sMuted)
		return b.render()
	}
	b.title = lp.Server + " / " + lp.Service
	texts, nums, full := m.logLines(lp)
	maxTop := max(len(texts)-ih, 0)
	top := maxTop - clamp(m.logScroll, 0, maxTop)
	if m.logFollow {
		top = maxTop
	}
	src := "preview"
	if full {
		src = "cached tail"
	}
	b.meta = dashJoinMeta(dashFilterMeta(m.logSearch), dashRangeMeta(top, ih, len(texts))+" lines", src)
	if len(texts) == 0 {
		msg := "No cached lines yet — `L` follows the live log."
		if m.logSearch != "" {
			msg = "No lines match “" + m.logSearch + "”."
		}
		b.lines = dashEmptyBody(r, iw, ih, msg, sMuted)
		return b.render()
	}
	numW := len(strconv.Itoa(dashMaxInt(nums)))
	l := newDLine(r.p, iw)
	for i := top; i < len(texts) && i < top+ih; i++ {
		l.reset(iw)
		l.textR(sDim, strconv.Itoa(nums[i]), numW)
		l.put(sBorder, " │ ")
		l.text(sNone, texts[i], l.room())
		b.lines = append(b.lines, l.String())
	}
	return b.render()
}

func dashMaxInt(vs []int) int {
	out := 0
	for _, v := range vs {
		if v > out {
			out = v
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Alerts
// ---------------------------------------------------------------------------

const (
	acSev = iota
	acState
	acServer
	acMessage
	acCode
	acCount
	acAge
)

func (m *model) renderAlertsTab(r *drctx, body drect) []string {
	g := m.geom(tabAlerts)
	if g.hidden && m.zoom {
		return m.alertDetailBox(r, body.w, body.h)
	}
	at := g.list
	iw := max(at.w-4, 1)
	al := func(i int) *fleetalerts.Alert { return &m.base.alerts[m.views.alerts[i]] }
	na := len(m.views.alerts)
	cols := dashFitCols([]dcol{
		{key: acSev, title: "SEV", w: 4},
		{key: acState, title: "STATE", w: 10},
		{key: acServer, title: "SERVER", w: 8, grow: growTo(8, dashMaxW(na, 20, func(i int) string { return al(i).Server }))},
		{key: acMessage, title: "MESSAGE", w: 28, grow: -1},
		{key: acCode, title: "CODE", w: 12, grow: growTo(12, dashMaxW(na, 28, func(i int) string { return al(i).Code })), drop: 3},
		{key: acCount, title: "SEEN", w: 5, right: true, drop: 2},
		{key: acAge, title: "AGE", w: 4, right: true},
	}, iw-1)
	n := len(m.views.alerts)
	s := m.base.stats
	sevLabel := "sev " + alertSevLabels[m.alertSev]
	stateLabel := "state " + alertStateLabels[m.alertState]
	meta := dashJoinMeta(dashFilterMeta(m.filters[tabAlerts]), sevLabel, stateLabel, dashRangeMeta(m.offsets[tabAlerts], max(at.h-3, 0), n))
	title := "Alerts  ▲" + strconv.Itoa(s.OpenCritical) + " crit  ▲" + strconv.Itoa(s.OpenWarning) + " warn  ●" + strconv.Itoa(s.OpenInfo) + " info open"
	if at.w < 90 {
		title = "Alerts"
	}
	empty := "No alerts. The fleet is quiet right now."
	if len(m.base.alerts) > 0 {
		empty = "No alerts match — v/t cycle the severity/state filters, esc clears text."
	}
	list := m.listBox(r, body, at, tabAlerts, title, meta, cols, -1, false, nil, n, func(row int, c dcol, l *dline, sel bool) {
		a := &m.base.alerts[m.views.alerts[row]]
		state := core.AlertState(*a, r.now)
		dim := state != "open"
		switch c.key {
		case acSev:
			st := dashSevStyle(a.Severity)
			if dim {
				st = sDim
			}
			dashCell(l, c, st, dashSevShort(a.Severity), sel)
		case acState:
			st := sBold
			if dim {
				st = sMuted
			}
			dashCell(l, c, st, state, sel)
		case acServer:
			dashCell(l, c, sNone, dashIfEmpty(a.Server), sel)
		case acMessage:
			st := sNone
			if dim {
				st = sMuted
			}
			dashCell(l, c, st, a.Message, sel)
		case acCode:
			dashCell(l, c, sDim, a.Code, sel)
		case acCount:
			txt := ""
			if a.Occurrences > 1 {
				txt = "×" + strconv.Itoa(a.Occurrences)
			}
			dashCell(l, c, sMuted, txt, sel)
		case acAge:
			dashCell(l, c, sDim, dashAge(r.now, dashAlertTime(*a)), sel)
		}
	}, empty)
	// The severity/state chips in the border are clickable.
	if len(list) > 0 {
		top := dashStripSGR(list[0])
		if i := strings.Index(top, sevLabel); i >= 0 {
			x := body.x + at.x + dashWidth(top[:i])
			r.zone(dashChipID(0), x, x+dashWidth(sevLabel)-1, body.y+at.y)
		}
		if i := strings.Index(top, stateLabel); i >= 0 {
			x := body.x + at.x + dashWidth(top[:i])
			r.zone(dashChipID(1), x, x+dashWidth(stateLabel)-1, body.y+at.y)
		}
	}
	if g.hidden {
		return list
	}
	return dplace(drect{0, 0, body.w, body.h}, dplaced{g.list, list}, dplaced{g.detail, m.alertDetailBox(r, g.detail.w, g.detail.h)})
}

func (m *model) alertDetailBox(r *drctx, w, h int) []string {
	a := m.selectedAlert()
	b := &dbox{p: r.p, w: w, h: h, title: "Alert"}
	iw, ih := b.inner()
	if a == nil {
		b.lines = dashEmptyBody(r, iw, ih, "Select an alert to see its details.", sMuted)
		return b.render()
	}
	state := core.AlertState(*a, r.now)
	b.title = dashSevShort(a.Severity) + " · " + dashIfEmpty(a.Server)
	b.meta, b.metaSty = state, sBold
	if state == "open" {
		b.metaSty = dashSevStyle(a.Severity)
	}
	l := newDLine(r.p, iw)
	var lines []string
	emit := func() { lines = append(lines, l.String()); l = newDLine(r.p, iw) }
	for _, wl := range dashWrap(a.Message, iw, 4) {
		l.put(sBold, wl)
		emit()
	}
	emit()
	kv := func(k, v string, st dstyle) {
		l.put(sMuted, k)
		l.text(st, dashIfEmpty(v), l.room())
		emit()
	}
	kv("id          ", a.ID, sNone)
	kv("code        ", a.Code, sNone)
	kv("severity    ", string(a.Severity), dashSevStyle(a.Severity))
	kv("first seen  ", dashFmtWhen(r.now, a.CreatedAt), sNone)
	kv("last update ", dashFmtWhen(r.now, a.UpdatedAt), sNone)
	occ := strconv.Itoa(max(a.Occurrences, 1)) + " occurrence(s)"
	if a.NotifyCount > 0 {
		occ += " · notified " + strconv.Itoa(a.NotifyCount) + "×"
	}
	kv("activity    ", occ, sNone)
	if a.AcknowledgedAt != nil {
		kv("acknowledged", " "+dashFmtWhen(r.now, *a.AcknowledgedAt), sOK)
	} else {
		kv("acknowledged", " no", sMuted)
	}
	if a.SuppressedUntil != nil && a.SuppressedUntil.After(r.now) {
		kv("suppressed  ", "until "+dashFmtWhen(r.now, *a.SuppressedUntil), sWarn)
	}
	b.lines = dashFitDetail(r, lines, iw, ih, []dhint{{"a", "ack"}, {"z", "suppress"}, {"u", "unsuppress"}, {"s", "ssh"}})
	return b.render()
}

func dashFmtWhen(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	lt := t.Local()
	s := lt.Format("2006-01-02 15:04:05")
	if t.After(now) {
		return s + " (in " + dashAge(t, now) + ")"
	}
	return s + " (" + dashAge(now, t) + " ago)"
}

// wrapText word-wraps untrusted text into at most maxLines lines of width w.
func dashWrap(raw string, w, maxLines int) []string {
	words := strings.Fields(dashClean(raw))
	var out []string
	cur := ""
	for _, word := range words {
		for dashWidth(word) > w {
			cut, _ := dashCut(word, w)
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			out = append(out, cut)
			word = word[len(cut):]
		}
		switch {
		case cur == "":
			cur = word
		case dashWidth(cur)+1+dashWidth(word) <= w:
			cur += " " + word
		default:
			out = append(out, cur)
			cur = word
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	if len(out) > maxLines {
		out = out[:maxLines]
		last := out[maxLines-1]
		cut, _ := dashCut(last, w-1)
		out[maxLines-1] = cut + "…"
	}
	return out
}

// ---------------------------------------------------------------------------
// Ops
// ---------------------------------------------------------------------------

const (
	ocTime = iota
	ocAction
	ocTarget
	ocOperator
)

func (m *model) renderOpsTab(r *drctx, body drect) []string {
	g := m.geom(tabOps)
	if m.zoom {
		// Small terminals: enter shows the controller panel (and, when the
		// audit detail pane is hidden too, the selected entry below it).
		if g.hidden {
			ch := min(body.h, 16)
			return dplace(drect{0, 0, body.w, body.h},
				dplaced{drect{0, 0, body.w, ch}, m.controllerBox(r, body.w, ch)},
				dplaced{drect{0, ch, body.w, body.h - ch}, m.auditDetailBox(r, body.w, body.h-ch)})
		}
		return m.controllerBox(r, body.w, body.h)
	}
	var parts []dplaced
	if g.aside.w > 0 {
		parts = append(parts, dplaced{g.aside, m.controllerBox(r, g.aside.w, g.aside.h)})
	}
	at := g.list
	iw := max(at.w-4, 1)
	today := r.now.Local().Format("2006-01-02")
	au := func(i int) *logs.AuditEntry { return &m.snapshot.RecentAudit[m.views.audit[i]] }
	nau := len(m.views.audit)
	timeW := 8
	for i := 0; i < nau; i++ {
		if au(i).Timestamp.Local().Format("2006-01-02") != today {
			timeW = 12
			break
		}
	}
	cols := dashFitCols([]dcol{
		{key: ocTime, title: "TIME", w: timeW},
		{key: ocAction, title: "ACTION", w: 10, grow: growTo(10, dashMaxW(nau, 28, func(i int) string { return au(i).Action }))},
		{key: ocTarget, title: "TARGET", w: 12, grow: -1},
		{key: ocOperator, title: "OPERATOR", w: 8, grow: growTo(8, dashMaxW(nau, 20, func(i int) string { return au(i).Operator })), drop: 2},
	}, iw-1)
	n := len(m.views.audit)
	meta := dashJoinMeta(dashFilterMeta(m.filters[tabOps]), dashRangeMeta(m.offsets[tabOps], max(at.h-3, 0), n))
	list := m.listBox(r, body, at, tabOps, "Audit trail", meta, cols, -1, false, nil, n, func(row int, c dcol, l *dline, sel bool) {
		e := &m.snapshot.RecentAudit[m.views.audit[row]]
		switch c.key {
		case ocTime:
			lt := e.Timestamp.Local()
			ts := lt.Format("Jan 02 15:04")
			if lt.Format("2006-01-02") == today {
				ts = lt.Format("15:04:05")
			}
			dashCell(l, c, sDim, ts, sel)
		case ocAction:
			dashCell(l, c, sAccent, e.Action, sel)
		case ocTarget:
			dashCell(l, c, sNone, e.Target, sel)
		case ocOperator:
			dashCell(l, c, sMuted, e.Operator, sel)
		}
	}, "No audit activity yet.")
	parts = append(parts, dplaced{g.list, list})
	if !g.hidden {
		parts = append(parts, dplaced{g.detail, m.auditDetailBox(r, g.detail.w, g.detail.h)})
	}
	return dplace(drect{0, 0, body.w, body.h}, parts...)
}

func (m *model) auditDetailBox(r *drctx, w, h int) []string {
	b := &dbox{p: r.p, w: w, h: h, title: "Entry"}
	iw, ih := b.inner()
	if m.auditIndex < 0 || m.auditIndex >= len(m.views.audit) {
		b.lines = dashEmptyBody(r, iw, ih, "Select an audit entry.", sMuted)
		return b.render()
	}
	e := &m.snapshot.RecentAudit[m.views.audit[m.auditIndex]]
	b.title = e.Action
	b.meta = dashFmtWhen(r.now, e.Timestamp)
	l := newDLine(r.p, iw)
	var lines []string
	emit := func() { lines = append(lines, l.String()); l = newDLine(r.p, iw) }
	l.put(sMuted, "operator ")
	l.text(sNone, dashIfEmpty(e.Operator), l.room())
	emit()
	l.put(sMuted, "target   ")
	l.text(sNone, dashIfEmpty(e.Target), l.room())
	emit()
	if e.Details != "" {
		for i, wl := range dashWrap(e.Details, iw-9, max(ih-2, 1)) {
			if i == 0 {
				l.put(sMuted, "details  ")
			} else {
				l.pad(sNone, 9)
			}
			l.put(sNone, wl)
			emit()
		}
	}
	b.lines = dashFitDetail(r, lines, iw, ih, nil)
	return b.render()
}

func (m *model) controllerBox(r *drctx, w, h int) []string {
	st := m.snapshot.Status
	b := &dbox{p: r.p, w: w, h: h, title: "Controller", meta: dashVersion(st.Version)}
	iw, ih := b.inner()
	l := newDLine(r.p, iw)
	var lines []string
	emit := func() { lines = append(lines, l.String()); l = newDLine(r.p, iw) }
	kv := func(k, v string, vs dstyle) {
		l.put(sMuted, k)
		l.text(vs, dashIfEmpty(v), l.room())
		emit()
	}
	kv("alias      ", st.Alias, sBold)
	kv("mode       ", string(st.DefaultMode), sNone)
	kv("database   ", string(st.DatabaseBackend), sNone)
	kv("updates    ", st.Channel+" · "+string(st.Policy), sNone)
	rb, rbs := "not available", sMuted
	if m.snapshot.RollbackAvailable {
		rb, rbs = "ready", sOK
	}
	kv("rollback   ", rb, rbs)
	kv("config dir ", st.ConfigDir, sDim)
	if !m.loadedAt.IsZero() {
		kv("refresh    ", "every "+dashFmtInterval(m.interval)+" · last took "+m.loadTook.Round(time.Millisecond).String(), sDim)
	}
	emit()
	l.put(sTitle, "Templates")
	l.put(sMuted, " "+strconv.Itoa(len(m.snapshot.Templates)))
	emit()
	if len(m.snapshot.Templates) == 0 {
		l.put(sDim, "  none")
		emit()
	}
	for _, t := range m.snapshot.Templates {
		l.text(sNone, "  "+t, l.room())
		emit()
	}
	emit()
	l.put(sTitle, "Controller keys")
	emit()
	names := make([]string, 0, len(st.Fingerprints))
	for k := range st.Fingerprints {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) == 0 {
		l.put(sDim, "  no key fingerprints available")
		emit()
	}
	for _, k := range names {
		l.put(sMuted, "  "+k+" ")
		l.text(sDim, st.Fingerprints[k], l.room())
		emit()
	}
	b.lines = dashFitDetail(r, lines, iw, ih, nil)
	return b.render()
}

// ---------------------------------------------------------------------------
// Help overlay
// ---------------------------------------------------------------------------

var dashHelp = []struct {
	section string
	keys    []dhint
}{
	{"Navigate", []dhint{
		{"1-6 / tab / ←→", "switch tab"},
		{"j k / ↑ ↓", "move selection"},
		{"pgup pgdn", "page (ctrl+u / ctrl+d)"},
		{"g G / home end", "first / last"},
		{"enter", "open detail · read log · drill down"},
		{"esc", "back · clear filter"},
		{"mouse", "click tabs, rows, headers; wheel scrolls"},
	}},
	{"Data", []dhint{
		{"r", "refresh now"},
		{"p", "pause / resume auto-refresh"},
		{"+ -", "slower / faster refresh"},
		{"/", "filter (words AND together; tag=value works)"},
	}},
	{"Servers", []dhint{
		{"o O", "cycle sort column / reverse"},
		{"s", "ssh into the row's server (any tab)"},
		{"f", "file manager on the row's server"},
		{"c", "reconnect (confirm)"},
		{"m", "collect metrics now"},
	}},
	{"Services & Logs", []dhint{
		{"L", "follow the live log"},
		{"R", "restart the service (confirm)"},
		{"enter", "focus the log viewer; / searches lines"},
	}},
	{"Alerts", []dhint{
		{"a", "acknowledge (confirm)"},
		{"z", "suppress for 1h/6h/24h/7d"},
		{"u", "lift a suppression"},
		{"v t", "cycle severity / state filter"},
	}},
	{"General", []dhint{
		{"?", "toggle this help"},
		{"q ctrl+c", "quit"},
	}},
}

func (m *model) renderHelp(r *drctx, body drect) []string {
	keyW := 0
	total := 0
	for _, sec := range dashHelp {
		total += len(sec.keys) + 2
		for _, k := range sec.keys {
			keyW = max(keyW, dashWidth(k.key))
		}
	}
	labelW := 0
	for _, sec := range dashHelp {
		for _, k := range sec.keys {
			labelW = max(labelW, dashWidth(k.label))
		}
	}
	colW := 2 + keyW + 2 + labelW
	cols := 1
	if body.w >= 2*colW+4+4 && body.h < total+2 {
		cols = 2
	}
	if body.w < colW+4 {
		colW = max(body.w-4, 10)
	}
	// Split sections into balanced columns.
	var colsLines [2][]string
	ci, acc := 0, 0
	for _, sec := range dashHelp {
		if cols == 2 && ci == 0 && acc >= (total+1)/2 {
			ci = 1
		}
		l := newDLine(r.p, colW)
		l.put(sTitle, sec.section)
		colsLines[ci] = append(colsLines[ci], l.String())
		for _, k := range sec.keys {
			l.reset(colW)
			l.pad(sNone, 2)
			l.text(sKey, k.key, keyW)
			l.pad(sNone, 2)
			l.text(sMuted, k.label, l.room())
			colsLines[ci] = append(colsLines[ci], l.String())
		}
		colsLines[ci] = append(colsLines[ci], dashRunSpace.n(colW))
		acc += len(sec.keys) + 2
	}
	var content []string
	if cols == 2 {
		n := max(len(colsLines[0]), len(colsLines[1]))
		for i := 0; i < n; i++ {
			a, c := dashRunSpace.n(colW), dashRunSpace.n(colW)
			if i < len(colsLines[0]) {
				a = colsLines[0][i]
			}
			if i < len(colsLines[1]) {
				c = colsLines[1][i]
			}
			content = append(content, a+"    "+c)
		}
	} else {
		content = colsLines[0]
	}
	for len(content) > 0 && strings.TrimSpace(dashStripSGR(content[len(content)-1])) == "" {
		content = content[:len(content)-1]
	}
	iw := colW*cols + 4*(cols-1)
	w := min(iw+4, body.w)
	h := min(len(content)+2, body.h)
	box := &dbox{p: r.p, w: w, h: h, title: "Keyboard & mouse", meta: "esc closes", focus: true}
	biw, _ := box.inner()
	for _, c := range content {
		if biw < iw {
			l := newDLine(r.p, biw)
			l.put(sNone, dashStripSGR(c))
			c = l.String()
		}
		box.lines = append(box.lines, c)
	}
	lines := box.render()
	x := max((body.w-w)/2, 0)
	y := max((body.h-h)/2, 0)
	r.zone(dashHelpID(), body.x+x, body.x+x+w-1, body.y+y)
	return dplace(drect{0, 0, body.w, body.h}, dplaced{drect{x, y, w, h}, lines})
}
