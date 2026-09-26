// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	fleetalerts "github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/store"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"
	"github.com/muesli/termenv"
)

var dashTestNow = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func plainLines(frame string) []string {
	return strings.Split(ansiSeq.ReplaceAllString(frame, ""), "\n")
}

// testDashData builds an in-memory dataset of n servers with services, tags,
// cached logs, 20 alerts (more than the 8-entry recent list) and audit entries.
func testDashData(n int, now time.Time) core.DashboardData {
	servers := make([]core.ServerRecord, n)
	tags := map[string]map[string]string{}
	var logsPreview []core.CachedLogPreview
	var sources []core.DashboardLogSource
	for i := range servers {
		name := fmt.Sprintf("node-%03d", i)
		role := []string{"web", "db", "cache"}[i%3]
		s := core.ServerRecord{
			Name: name, Address: fmt.Sprintf("10.0.%d.%d", i/250, i%250+1), Port: 2222, User: "root",
			Mode: transport.ModeDirect,
			Observed: core.ServerObservation{Reachable: i%10 != 9, LastSeen: now.Add(-time.Duration(i%120) * time.Second),
				NodeName: name, AgentVersion: "2.4.3", OS: "linux", Arch: "amd64"},
			Metrics: proto.MetricsSnapshot{Timestamp: now.Add(-30 * time.Second),
				CPUPercent: float64((i * 37) % 100), MemoryPercent: float64((i*53)%90 + 5), DiskPercent: float64((i*29)%80 + 10),
				Load1: float64(i%8) / 2, MemoryTotalBytes: 16 << 30, MemoryUsedBytes: 4 << 30, DiskTotalBytes: 500 << 30, DiskUsedBytes: 100 << 30},
		}
		if i%11 == 4 {
			s.Observed.AgentVersion = "2.4.1"
		}
		if i%9 == 2 {
			s.Mode = transport.ModeReverse
		}
		if i%3 == 0 {
			state := "active"
			if i%7 == 0 {
				state = "failed"
			}
			s.Services = []core.ServiceRecord{{Name: "nginx.service", LogPath: "/var/log/nginx/access.log", Critical: true, ActiveState: state, SubState: "running"}}
			lines := make([]proto.LogLine, 12)
			for l := range lines {
				lines[l] = proto.LogLine{Number: l + 1, Text: fmt.Sprintf("GET /health %d 200", l)}
			}
			logsPreview = append(logsPreview, core.CachedLogPreview{Server: name, Service: "nginx.service", Path: "/cache/" + name + "/nginx.service.log", Lines: lines, Available: true})
			sources = append(sources, core.DashboardLogSource{Server: name, Service: "nginx.service", LogPath: "/var/log/nginx/access.log"})
		}
		tags[name] = map[string]string{"role": role, "env": []string{"prod", "staging"}[i%2]}
		servers[i] = s
	}
	var all []fleetalerts.Alert
	sev := func(i int) fleetalerts.Severity {
		switch {
		case i < 12:
			return fleetalerts.SeverityCritical
		case i < 17:
			return fleetalerts.SeverityWarning
		}
		return fleetalerts.SeverityInfo
	}
	past := now.Add(-time.Minute)
	future := now.Add(time.Hour)
	for i := 0; i < 20; i++ {
		a := fleetalerts.Alert{ID: fmt.Sprintf("alert-%02d", i), Server: fmt.Sprintf("node-%03d", i%max(n, 1)), Severity: sev(i),
			Code: "metrics.cpu", Message: fmt.Sprintf("alert %d message", i), CreatedAt: now.Add(-time.Duration(i) * time.Minute),
			UpdatedAt: now.Add(-time.Duration(i) * time.Minute)}
		if i == 1 {
			a.AcknowledgedAt = &past
		}
		if i == 13 {
			a.SuppressedUntil = &future
		}
		all = append(all, a)
	}
	var audit []logs.AuditEntry
	for i := 0; i < 30; i++ {
		audit = append(audit, logs.AuditEntry{Timestamp: now.Add(-time.Duration(i) * time.Minute), Action: "exec", Target: fmt.Sprintf("node-%03d", i), Operator: "ops"})
	}
	stats := core.ComputeAlertStats(all, now)
	return core.DashboardData{
		DashboardSnapshot: core.DashboardSnapshot{
			Status: core.Status{Alias: "prod", Version: "2.4.3", DatabaseBackend: store.BackendSQLite, Channel: "stable",
				Policy: update.PolicyNotifyOnly, ServerCount: n, Fingerprints: map[string]string{"ed25519": "SHA256:abc"}},
			Summary:      core.DashboardSummary{CriticalAlerts: stats.Critical, WarningAlerts: stats.Warning, InfoAlerts: stats.Info, MonitoredAlerts: stats.Total},
			Servers:      servers,
			CachedLogs:   logsPreview,
			RecentAlerts: all[:8],
			RecentAudit:  audit,
			Templates:    []string{"web.toml"},
			GeneratedAt:  now,
		},
		Alerts:     all,
		AlertStats: stats,
		Tags:       tags,
		LogSources: sources,
	}
}

func newTestDash(t testing.TB, n, w, h int) model {
	t.Helper()
	// A closed loader: commands that would read the controller fail fast
	// instead of touching a real config dir.
	rt := newDashRuntime(&dashLoader{closed: true}, true, "", "", "/cfg")
	m := newDashboardModel(rt, 0)
	m.now = dashTestNow
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	next, _ = next.Update(dashboardLoadedMsg{data: testDashData(n, dashTestNow), full: true})
	return next.(model)
}

func keyMsg(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

func press(m model, keys ...string) model {
	for _, k := range keys {
		next, _ := m.Update(keyMsg(k))
		m = next.(model)
	}
	return m
}

func typeText(m model, s string) model {
	for _, r := range s {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(model)
	}
	return m
}

func visibleServerNames(m *model) []string {
	m.ensureDerived()
	out := make([]string, 0, len(m.views.servers))
	for _, i := range m.views.servers {
		out = append(out, m.snapshot.Servers[m.base.rows[i].idx].Name)
	}
	return out
}

// Every tab must fill the terminal exactly — never more lines than the
// terminal has (which is what scrolled the old header away) and never a line
// wider than the terminal.
func TestDashboardViewFitsTerminalAtEverySize(t *testing.T) {
	sizes := [][2]int{{80, 24}, {120, 40}, {160, 45}, {200, 60}, {60, 16}}
	for _, sz := range sizes {
		for tab := tabOverview; tab < dashNumTabs; tab++ {
			m := newTestDash(t, 500, sz[0], sz[1])
			m = press(m, string(rune('1'+tab)))
			frame := m.View()
			lines := plainLines(frame)
			if len(lines) != sz[1] {
				t.Fatalf("%dx%d tab %s: %d lines, want %d", sz[0], sz[1], dashboardTabs[tab], len(lines), sz[1])
			}
			for i, line := range lines {
				if w := lipgloss.Width(line); w != sz[0] {
					t.Fatalf("%dx%d tab %s line %d: width %d, want %d:\n%q", sz[0], sz[1], dashboardTabs[tab], i, w, sz[0], line)
				}
			}
			if !strings.Contains(lines[0], "FLEET") {
				t.Fatalf("%dx%d tab %s: header missing from the first line: %q", sz[0], sz[1], dashboardTabs[tab], lines[0])
			}
			if !strings.Contains(lines[1], "1 ") || !strings.Contains(lines[1], "6 ") {
				t.Fatalf("%dx%d tab %s: tab bar missing: %q", sz[0], sz[1], dashboardTabs[tab], lines[1])
			}
		}
	}
}

func TestDashboardTooSmallTerminal(t *testing.T) {
	m := newTestDash(t, 10, 50, 12)
	lines := plainLines(m.View())
	if len(lines) != 12 {
		t.Fatalf("got %d lines, want 12", len(lines))
	}
	if !strings.Contains(strings.Join(lines, "\n"), "Terminal too small") {
		t.Fatalf("expected a too-small notice, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestDashboardOverviewShowsCorrectAlertTotals(t *testing.T) {
	m := newTestDash(t, 50, 160, 45)
	frame := strings.Join(plainLines(m.View()), "\n")
	// 20 alerts: 12 critical (1 acked), 5 warning (1 suppressed), 3 info —
	// counted over all alerts, not the 8-entry recent list.
	for _, want := range []string{"20 total", "11 critical", "4 warning", "3 info", "18 open", "1 acked", "1 suppressed"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("overview missing %q:\n%s", want, frame)
		}
	}
	if !strings.Contains(plainLines(m.View())[1], "5 Alerts 18") {
		t.Fatalf("alerts tab count should be the 18 open alerts: %q", plainLines(m.View())[1])
	}
}

// A model built only from a classic snapshot (no full alert list) still
// derives totals from what it has.
func TestDashboardDerivesStatsFromSnapshotOnly(t *testing.T) {
	data := testDashData(5, dashTestNow)
	m := model{width: 120, height: 40, snapshot: data.DashboardSnapshot, now: dashTestNow}
	m.ensureDerived()
	if m.base.stats.Total != 8 {
		t.Fatalf("stats over RecentAlerts = %d, want 8", m.base.stats.Total)
	}
}

func TestServerSortCyclesAndReverses(t *testing.T) {
	m := newTestDash(t, 100, 160, 45)
	m = press(m, "2")
	names := visibleServerNames(&m)
	if !slices.IsSorted(names) {
		t.Fatalf("default order should be by name: %v", names[:5])
	}
	m = press(m, "o") // status
	m = press(m, "o") // cpu (hottest first)
	if m.sortCol != ssCPU || !m.sortDesc {
		t.Fatalf("sort = %v desc=%v, want cpu desc", m.sortCol, m.sortDesc)
	}
	cpu := func(m *model) []float64 {
		var out []float64
		for _, i := range m.views.servers {
			out = append(out, m.snapshot.Servers[m.base.rows[i].idx].Metrics.CPUPercent)
		}
		return out
	}
	got := cpu(&m)
	if !slices.IsSortedFunc(got, func(a, b float64) int { return int(b - a) }) {
		t.Fatalf("cpu not descending: %v", got[:10])
	}
	m = press(m, "O")
	got = cpu(&m)
	if !slices.IsSorted(got) {
		t.Fatalf("cpu not ascending after O: %v", got[:10])
	}
	frame := strings.Join(plainLines(m.View()), "\n")
	if !strings.Contains(frame, "CPU▲") || !strings.Contains(frame, "sort cpu") {
		t.Fatalf("sort indicator missing:\n%s", frame)
	}
	// Status sort puts offline servers first.
	m = newTestDash(t, 100, 160, 45)
	m = press(m, "2", "o")
	first := m.base.rows[m.views.servers[0]]
	if first.status != dsOffline {
		t.Fatalf("status sort should list offline first, got %v", first.status.label())
	}
}

func TestServerFilterIsIncrementalAndCombinesTerms(t *testing.T) {
	m := newTestDash(t, 90, 160, 45)
	m = press(m, "2", "/")
	if !m.filtering {
		t.Fatalf("/ should start filtering")
	}
	m = typeText(m, "role=db")
	if got := len(m.views.servers); got != 30 {
		t.Fatalf("role=db matched %d, want 30", got)
	}
	m = typeText(m, " env=prod")
	for _, name := range visibleServerNames(&m) {
		var i int
		fmt.Sscanf(name, "node-%03d", &i)
		if i%3 != 1 || i%2 != 0 {
			t.Fatalf("%s does not match role=db env=prod", name)
		}
	}
	m = press(m, "enter")
	if m.filtering || m.filters[tabServers] == "" {
		t.Fatalf("enter should keep the filter and stop editing")
	}
	frame := strings.Join(plainLines(m.View()), "\n")
	if !strings.Contains(frame, "/role=db env=prod") {
		t.Fatalf("active filter should be shown in the list title:\n%s", frame)
	}
	m = press(m, "esc")
	if len(m.views.servers) != 90 {
		t.Fatalf("esc should clear the filter, still %d rows", len(m.views.servers))
	}
	// Status words filter too.
	m = press(m, "/")
	m = typeText(m, "offline")
	if len(m.views.servers) != 9 {
		t.Fatalf("offline matched %d, want 9", len(m.views.servers))
	}
	m = press(m, "backspace", "backspace", "backspace", "backspace", "backspace", "backspace", "backspace")
	if len(m.views.servers) != 90 {
		t.Fatalf("clearing the filter by backspace should restore all rows")
	}
}

func TestSelectionSurvivesRefreshSortAndFilter(t *testing.T) {
	m := newTestDash(t, 200, 160, 45)
	m = press(m, "2")
	for i := 0; i < 42; i++ {
		m = press(m, "j")
	}
	if s, _ := m.selectedServer(); s == nil || s.Name != "node-042" {
		t.Fatalf("selected %v, want node-042", s)
	}
	// Refresh with servers inserted before it and the list reversed.
	data := testDashData(200, dashTestNow)
	extra := data.Servers[0]
	extra.Name = "aaa-new"
	data.Servers = append([]core.ServerRecord{extra}, data.Servers...)
	slices.Reverse(data.Servers)
	next, _ := m.Update(dashboardLoadedMsg{data: data})
	m = next.(model)
	if s, _ := m.selectedServer(); s == nil || s.Name != "node-042" {
		t.Fatalf("after refresh selected %v, want node-042", s)
	}
	// Re-sorting keeps it.
	m = press(m, "o", "o")
	if s, _ := m.selectedServer(); s == nil || s.Name != "node-042" {
		t.Fatalf("after sort selected %v, want node-042", s)
	}
	// The cursor window scrolled to keep the selection visible.
	rows := m.visibleRows(tabServers)
	if m.serverIndex < m.offsets[tabServers] || m.serverIndex >= m.offsets[tabServers]+rows {
		t.Fatalf("cursor %d outside window [%d,%d)", m.serverIndex, m.offsets[tabServers], m.offsets[tabServers]+rows)
	}
	// Filtered out and back: the selection returns.
	m = press(m, "/")
	m = typeText(m, "node-1")
	m = press(m, "esc")
	if s, _ := m.selectedServer(); s == nil || s.Name != "node-042" {
		t.Fatalf("after filter round-trip selected %v, want node-042", s)
	}
}

func TestServersTabRendersOnlyVisibleRows(t *testing.T) {
	m := newTestDash(t, 500, 160, 45)
	m = press(m, "2")
	frame := strings.Join(plainLines(m.View()), "\n")
	if !strings.Contains(frame, "node-000") || strings.Contains(frame, "node-499") {
		t.Fatalf("expected only the first window of servers to be drawn")
	}
	m = press(m, "G")
	frame = strings.Join(plainLines(m.View()), "\n")
	if !strings.Contains(frame, "node-499") || strings.Contains(frame, "node-000 ") {
		t.Fatalf("G should scroll to the last window")
	}
	if !strings.Contains(frame, "of 500") {
		t.Fatalf("range indicator missing")
	}
}

func TestDashboardNoColorRendering(t *testing.T) {
	prev := lipgloss.ColorProfile()
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	t.Setenv("NO_COLOR", "1")
	lipgloss.SetColorProfile(termenv.Ascii)

	m := newTestDash(t, 30, 120, 40)
	for tab := tabOverview; tab < dashNumTabs; tab++ {
		m = press(m, string(rune('1'+tab)))
		frame := m.View()
		if regexp.MustCompile(`\x1b\[[0-9;]*(38|48);`).MatchString(frame) {
			t.Fatalf("tab %s: NO_COLOR frame contains colour sequences", dashboardTabs[tab])
		}
		for _, seq := range ansiSeq.FindAllString(frame, -1) {
			switch seq {
			case "\x1b[0m", "\x1b[1m", "\x1b[4m", "\x1b[7m", "\x1b[1;7m", "\x1b[1;4m", "\x1b[4;7m", "\x1b[1;4;7m":
			default:
				t.Fatalf("tab %s: unexpected sequence %q under NO_COLOR", dashboardTabs[tab], seq)
			}
		}
	}
	// The selected row stays identifiable without colour: reverse video plus
	// a cursor marker.
	m = press(m, "2")
	frame := m.View()
	if !strings.Contains(frame, "\x1b[1;7m›") && !strings.Contains(frame, "\x1b[7m›") {
		t.Fatalf("selected row should be reverse video under NO_COLOR")
	}
}

func TestDashboardColorProfileUsesPalette(t *testing.T) {
	prev := lipgloss.ColorProfile()
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	lipgloss.SetColorProfile(termenv.ANSI256)
	m := newTestDash(t, 30, 120, 40)
	if !strings.Contains(m.View(), "38;5;") {
		t.Fatalf("expected 256-colour sequences under the ANSI256 profile")
	}
}

func TestDashboardSanitizesUntrustedText(t *testing.T) {
	data := testDashData(3, dashTestNow)
	data.Alerts[0].Message = "evil \x1b]0;pwned\x07 \x1b[2J\x1b[31mred\u009b line"
	data.CachedLogs[0].Lines[11].Text = "log \x1b[2J\x1b]52;c;ZXZpbA==\x07 tail"
	rt := newDashRuntime(nil, true, "", "", "/cfg")
	m := newDashboardModel(rt, 0)
	m.now = dashTestNow
	next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 45})
	next, _ = next.Update(dashboardLoadedMsg{data: data})
	m = next.(model)
	for _, tab := range []string{"1", "4", "5"} {
		frame := press(m, tab).View()
		for _, bad := range []string{"\x1b]", "\x1b[2J", "\x07", "\u009b", "\x1b[31m"} {
			if strings.Contains(frame, bad) {
				t.Fatalf("tab %s: frame leaks %q", tab, bad)
			}
		}
	}
}

func TestMouseMotionReusesCachedFrame(t *testing.T) {
	m := newTestDash(t, 500, 160, 45)
	m = press(m, "2")
	first := m.View()
	rev := m.rev
	next, _ := m.Update(tea.MouseMsg{X: 10, Y: 10, Action: tea.MouseActionMotion, Button: tea.MouseButtonNone})
	m = next.(model)
	if m.rev != rev {
		t.Fatalf("mouse motion changed the model revision")
	}
	if got := m.View(); got != first {
		t.Fatalf("mouse motion produced a different frame")
	}
	m = press(m, "j")
	if m.View() == first {
		t.Fatalf("moving the selection must produce a new frame")
	}
}

func TestAutoRefreshScheduling(t *testing.T) {
	rt := newDashRuntime(&dashLoader{closed: true}, true, "", "", "/cfg")
	m := newDashboardModel(rt, 5*time.Second)
	m.now = dashTestNow
	next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 45})
	next, _ = next.Update(dashboardLoadedMsg{data: testDashData(3, dashTestNow)})
	m = next.(model)
	if m.inflight {
		t.Fatalf("load should clear inflight")
	}
	m.lastStart = dashTestNow
	// Not due yet.
	next, _ = m.Update(dashClockMsg(dashTestNow.Add(2 * time.Second)))
	m = next.(model)
	if m.inflight {
		t.Fatalf("refresh started before the interval elapsed")
	}
	// Due: a refresh starts (non-blocking: it is a Cmd).
	next, cmd := m.Update(dashClockMsg(dashTestNow.Add(6 * time.Second)))
	m = next.(model)
	if !m.inflight || cmd == nil {
		t.Fatalf("refresh should start when due")
	}
	// A failed refresh keeps the last good data and reports inline.
	next, _ = m.Update(dashboardLoadedMsg{err: fmt.Errorf("database is locked")})
	m = next.(model)
	if len(m.snapshot.Servers) != 3 || m.lastErr == nil {
		t.Fatalf("failed refresh should keep data and record the error")
	}
	if header := plainLines(m.View())[0]; !strings.Contains(header, "refresh failed: database is locked · ● live") {
		t.Fatalf("header should show the refresh error: %q", header)
	}
	// Paused: nothing starts.
	m = press(m, "p")
	m.lastStart = time.Time{}
	next, _ = m.Update(dashClockMsg(dashTestNow.Add(time.Minute)))
	m = next.(model)
	if m.inflight {
		t.Fatalf("paused dashboard must not refresh")
	}
	if header := plainLines(m.View())[0]; !strings.Contains(header, "paused") {
		t.Fatalf("header should say paused: %q", header)
	}
	// Interval keys.
	m = press(m, "+")
	if m.interval != 10*time.Second {
		t.Fatalf("+ should lengthen the interval, got %v", m.interval)
	}
	m = press(m, "-", "-", "-", "-")
	if m.interval != time.Second {
		t.Fatalf("- should shorten to the 1s floor, got %v", m.interval)
	}
	// Stale marker once data is old.
	m = press(m, "p")
	m.inflight = true
	next, _ = m.Update(dashClockMsg(dashTestNow.Add(10 * time.Minute)))
	m = next.(model)
	if header := plainLines(m.View())[0]; !strings.Contains(header, "stale") {
		t.Fatalf("header should flag stale data: %q", header)
	}
}

func TestDashboardUpdateAtSeveralSizes(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {120, 40}, {200, 60}} {
		m := newTestDash(t, 120, sz[0], sz[1])
		// Walk every tab, move, page, open details, help, and back.
		for tab := 1; tab <= 6; tab++ {
			m = press(m, fmt.Sprint(tab), "j", "j", "pgdown", "k", "enter", "esc", "?", "esc")
			if m.overlay != dashOverlayNone || m.zoom {
				t.Fatalf("%v tab %d: overlay/zoom left open", sz, tab)
			}
			lines := plainLines(m.View())
			if len(lines) != sz[1] {
				t.Fatalf("%v tab %d: %d lines", sz, tab, len(lines))
			}
		}
		// Narrow terminals zoom into the detail pane on enter.
		m = press(m, "2")
		if m.geom(tabServers).hidden {
			m = press(m, "enter")
			if !m.zoom {
				t.Fatalf("%v: enter should zoom into the server detail", sz)
			}
			frame := strings.Join(plainLines(m.View()), "\n")
			if !strings.Contains(frame, "Resources") {
				t.Fatalf("%v: zoomed detail should show resources:\n%s", sz, frame)
			}
			m = press(m, "esc")
		}
	}
}

func TestServerDetailShowsHistorySparkline(t *testing.T) {
	m := newTestDash(t, 10, 160, 45)
	m = press(m, "2")
	s, _ := m.selectedServer()
	frame := strings.Join(plainLines(m.View()), "\n")
	if !strings.Contains(frame, "Resources · current") {
		t.Fatalf("without history the detail should show current values:\n%s", frame)
	}
	cmd := m.afterMove()
	if cmd == nil {
		t.Fatalf("selecting a server should schedule a history read")
	}
	points := make([]core.MetricPoint, 30)
	for i := range points {
		points[i] = core.MetricPoint{Timestamp: dashTestNow.Add(time.Duration(i-30) * time.Minute), CPU: float64(i * 3), Memory: 40, Disk: 60}
	}
	next, _ := m.Update(dashHistMsg{server: s.Name, stamp: s.Metrics.Timestamp, points: points})
	m = next.(model)
	frame = strings.Join(plainLines(m.View()), "\n")
	if !strings.Contains(frame, "30 samples") || !strings.ContainsAny(frame, "▁▂▃▄▅▆▇") {
		t.Fatalf("history sparkline missing:\n%s", frame)
	}
}

func TestLogViewerScrollsAndSearches(t *testing.T) {
	m := newTestDash(t, 12, 160, 45)
	m = press(m, "4")
	lp := m.selectedLog()
	if lp == nil {
		t.Fatalf("no log selected")
	}
	lines := make([]proto.LogLine, 200)
	for i := range lines {
		lines[i] = proto.LogLine{Number: i + 1, Text: fmt.Sprintf("request %d status=%d", i+1, 200+(i%3)*100)}
	}
	key := lp.Server + "\x00" + lp.Service
	next, _ := m.Update(dashLogMsg{key: key, stamp: dashLogStamp(lp), preview: core.CachedLogPreview{Server: lp.Server, Service: lp.Service, Lines: lines, Available: true}})
	m = next.(model)
	frame := strings.Join(plainLines(m.View()), "\n")
	if !strings.Contains(frame, "request 200 ") || !strings.Contains(frame, "cached tail") {
		t.Fatalf("viewer should follow the end of the loaded tail:\n%s", frame)
	}
	m = press(m, "enter")
	if !m.viewerFocus {
		t.Fatalf("enter should focus the viewer")
	}
	m = press(m, "g")
	frame = strings.Join(plainLines(m.View()), "\n")
	if !strings.Contains(frame, "request 1 ") {
		t.Fatalf("g should scroll the viewer to the top:\n%s", frame)
	}
	m = press(m, "/")
	m = typeText(m, "status=400")
	m = press(m, "enter")
	texts, _, _ := m.logLines(m.selectedLog())
	if len(texts) != 66 {
		t.Fatalf("search matched %d lines, want 66", len(texts))
	}
	m = press(m, "esc", "esc")
	if m.viewerFocus || m.logSearch != "" {
		t.Fatalf("esc should leave the viewer and clear the search")
	}
}

func TestAlertFiltersAndOrdering(t *testing.T) {
	m := newTestDash(t, 30, 160, 45)
	m = press(m, "5")
	if len(m.views.alerts) != 20 {
		t.Fatalf("all alerts = %d", len(m.views.alerts))
	}
	// Open alerts first, most severe first.
	first := m.base.alerts[m.views.alerts[0]]
	if first.Severity != fleetalerts.SeverityCritical || core.AlertState(first, dashTestNow) != "open" {
		t.Fatalf("first alert = %+v", first)
	}
	last := m.base.alerts[m.views.alerts[len(m.views.alerts)-1]]
	if core.AlertState(last, dashTestNow) == "open" {
		t.Fatalf("handled alerts should sort after open ones")
	}
	m = press(m, "v") // critical
	if len(m.views.alerts) != 12 {
		t.Fatalf("critical filter = %d, want 12", len(m.views.alerts))
	}
	m = press(m, "t") // open
	if len(m.views.alerts) != 11 {
		t.Fatalf("critical+open = %d, want 11", len(m.views.alerts))
	}
	m = press(m, "t", "t", "t", "v", "v", "v")
	if len(m.views.alerts) != 20 {
		t.Fatalf("filters should cycle back to all, got %d", len(m.views.alerts))
	}
}

func TestAlertAcknowledgeNeedsConfirmation(t *testing.T) {
	m := newTestDash(t, 30, 160, 45)
	m = press(m, "5", "a")
	if m.prompt == nil {
		t.Fatalf("a should ask for confirmation")
	}
	if footer := plainLines(m.View())[44]; !strings.Contains(footer, "Acknowledge critical alert alert-00") {
		t.Fatalf("prompt not shown: %q", footer)
	}
	m = press(m, "n")
	if m.prompt != nil || m.busy != "" {
		t.Fatalf("n should cancel without running anything")
	}
	// An already-acknowledged alert is not re-acked.
	m.alertIndex = 0
	for i := range m.views.alerts {
		if m.base.alerts[m.views.alerts[i]].AcknowledgedAt != nil {
			m.setCursor(tabAlerts, i)
		}
	}
	m = press(m, "a")
	if m.prompt != nil || !strings.Contains(m.flash, "already acknowledged") {
		t.Fatalf("acking an acked alert should just say so (flash %q)", m.flash)
	}
}

// fakeFleet writes a shell script standing in for the fleet binary that records
// its argv and FLEET_TOKEN.
func fakeFleet(t *testing.T, exitCode int, stderr string) (exe, out string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stand-in needs a POSIX shell")
	}
	dir := t.TempDir()
	out = filepath.Join(dir, "argv.txt")
	exe = filepath.Join(dir, "fleet")
	script := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do printf '%%s\\n' \"$a\"; done > %q\nprintf 'TOKEN=%%s\\n' \"$FLEET_TOKEN\" >> %q\nprintf '%%s' %q >&2\nexit %d\n", out, out, stderr, exitCode)
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return exe, out
}

func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatalf("expected a command")
	}
	return cmd()
}

func TestAlertActionsRunTheCLIWithConfigDirAndToken(t *testing.T) {
	exe, out := fakeFleet(t, 0, "")
	m := newTestDash(t, 30, 160, 45)
	m.rt.exe = exe
	m.rt.configDir = "/etc/fleet-test"
	m.rt.token = "tok-123"
	m.rt.environ = func() []string { return []string{"PATH=/usr/bin:/bin", "FLEET_TOKEN=someone-else"} }

	m = press(m, "5", "a")
	next, cmd := m.Update(keyMsg("y"))
	m = next.(model)
	if m.busy == "" {
		t.Fatalf("action should be marked busy while it runs")
	}
	msg := runCmd(t, cmd)
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := "--config-dir\n/etc/fleet-test\nalerts\nack\n--\nalert-00\nTOKEN=tok-123\n"
	if string(data) != want {
		t.Fatalf("child argv/env =\n%s\nwant\n%s", data, want)
	}
	next, refresh := m.Update(msg)
	m = next.(model)
	if m.busy != "" || !strings.Contains(m.flash, "acknowledged alert-00") || m.flashErr {
		t.Fatalf("flash = %q err=%v busy=%q", m.flash, m.flashErr, m.busy)
	}
	if refresh == nil || !m.inflight {
		t.Fatalf("a finished action should trigger a refresh")
	}

	// Suppress offers durations; "1" picks one hour.
	m.inflight = false
	m = press(m, "z")
	if m.prompt == nil || len(m.prompt.choices) != 4 {
		t.Fatalf("z should offer suppression durations")
	}
	next, cmd = m.Update(keyMsg("1"))
	m = next.(model)
	runCmd(t, cmd)
	data, _ = os.ReadFile(out)
	if !strings.Contains(string(data), "alerts\nsuppress\n--for\n1h0m0s\n--\n") {
		t.Fatalf("suppress argv = %q", data)
	}
}

func TestFailedActionReportsCLIError(t *testing.T) {
	exe, _ := fakeFleet(t, 1, "Error: denied: unknown or revoked token\n")
	m := newTestDash(t, 5, 160, 45)
	m.rt.exe = exe
	m = press(m, "2", "c")
	if m.prompt == nil || !strings.Contains(m.prompt.question, "Reconnect node-000") {
		t.Fatalf("reconnect should ask for confirmation")
	}
	next, cmd := m.Update(keyMsg("y"))
	m = next.(model)
	next, _ = m.Update(runCmd(t, cmd))
	m = next.(model)
	if !m.flashErr || !strings.Contains(m.flash, "denied: unknown or revoked token") {
		t.Fatalf("flash = %q (err=%v)", m.flash, m.flashErr)
	}
}

func TestInteractiveActionsSuspendTheTUI(t *testing.T) {
	m := newTestDash(t, 12, 160, 45)
	m.rt.exe = "/opt/fleet/bin/fleet"
	m.rt.configDir = "/cfg"
	var got []*exec.Cmd
	m.rt.execProcess = func(c *exec.Cmd, cb tea.ExecCallback) tea.Cmd {
		got = append(got, c)
		return func() tea.Msg { return cb(nil) }
	}
	m = press(m, "2", "j") // node-001
	m = press(m, "s", "f")
	m = press(m, "3", "L") // first service row
	if len(got) != 3 {
		t.Fatalf("expected 3 exec'd commands, got %d", len(got))
	}
	wantArgs := [][]string{
		{"/opt/fleet/bin/fleet", "--config-dir", "/cfg", "ssh", "--", "node-001"},
		{"/opt/fleet/bin/fleet", "--config-dir", "/cfg", "files", "--", "node-001"},
	}
	for i, want := range wantArgs {
		if !slices.Equal(got[i].Args, want) {
			t.Fatalf("command %d = %q, want %q", i, got[i].Args, want)
		}
	}
	if a := got[2].Args; !slices.Equal(a[3:6], []string{"service", "logs", "--follow"}) || a[6] != "--" {
		t.Fatalf("follow command = %q", a)
	}
	// No free-form commands: unknown keys run nothing.
	before := len(got)
	m = press(m, "!", ":", "x")
	if len(got) != before {
		t.Fatalf("unexpected command launched")
	}
}

func TestChildEnvPinsTheDashboardToken(t *testing.T) {
	rt := newDashRuntime(nil, true, "fleet", "tok", "/cfg")
	rt.environ = func() []string { return []string{"A=1", "FLEET_TOKEN=other", "B=2"} }
	env := rt.childEnv()
	if !slices.Equal(env, []string{"A=1", "B=2", "FLEET_TOKEN=tok"}) {
		t.Fatalf("env = %q", env)
	}
	rt.token = ""
	if env := rt.childEnv(); !slices.Equal(env, []string{"A=1", "FLEET_TOKEN=other", "B=2"}) {
		t.Fatalf("without a dashboard token the environment passes through: %q", env)
	}
}

func TestOverviewClickDrillsIntoServer(t *testing.T) {
	m := newTestDash(t, 60, 160, 45)
	target := m.snapshot.Servers[m.base.rows[m.base.fleet.topCPU[0]].idx].Name
	if !clickZone(t, &m, dashHotID(0, 0)) {
		t.Fatalf("hot spot click not handled")
	}
	if m.activeTab != tabServers {
		t.Fatalf("click should switch to the Servers tab")
	}
	if s, _ := m.selectedServer(); s == nil || s.Name != target {
		t.Fatalf("selected %v, want %s", s, target)
	}
}

func TestHeaderClickSortsServers(t *testing.T) {
	m := newTestDash(t, 60, 200, 60)
	m = press(m, "2")
	if !clickZone(t, &m, dashColID(int(ssDisk))) {
		t.Fatalf("header click not handled")
	}
	if m.sortCol != ssDisk || !m.sortDesc {
		t.Fatalf("sort = %v desc=%v", m.sortCol, m.sortDesc)
	}
	if !clickZone(t, &m, dashColID(int(ssDisk))) || m.sortDesc {
		t.Fatalf("second click should reverse the sort")
	}
}

func TestLoaderReusesOneAppUntilConfigChanges(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := core.Initialize(core.InitOptions{ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeDirect,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly}); err != nil {
		t.Fatal(err)
	}
	app, err := core.Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	l := newDashLoader(configDir, app)
	defer l.Close()
	if _, err := l.load(false); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := l.load(false); err != nil {
		t.Fatalf("load: %v", err)
	}
	if l.app != app {
		t.Fatalf("refresh reopened the App")
	}
	// A config change (e.g. `fleet database shift`) reopens it.
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(core.ConfigPath(configDir), future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := l.load(false); err != nil {
		t.Fatalf("load: %v", err)
	}
	if l.app == app {
		t.Fatalf("config change should reopen the App")
	}
	l.Close()
	if _, err := l.load(false); err == nil {
		t.Fatalf("load after Close should fail")
	}
}

// Periodic refreshes do not re-read every cached log: they list the sources and
// keep the previews of the last full load.
func TestPeriodicRefreshKeepsLogPreviewsWithoutReading(t *testing.T) {
	m := newTestDash(t, 12, 160, 45)
	if len(m.snapshot.CachedLogs) != 4 || !m.snapshot.CachedLogs[0].Available {
		t.Fatalf("full load previews = %+v", m.snapshot.CachedLogs)
	}
	data := testDashData(15, dashTestNow) // one more tracked service (node-012)
	data.CachedLogs = nil                 // what SkipLogPreviews returns
	next, _ := m.Update(dashboardLoadedMsg{data: data})
	m = next.(model)
	if len(m.snapshot.CachedLogs) != 5 {
		t.Fatalf("sources = %d, want 5", len(m.snapshot.CachedLogs))
	}
	if !m.snapshot.CachedLogs[0].Available || len(m.snapshot.CachedLogs[0].Lines) != 12 {
		t.Fatalf("previous preview should be carried over: %+v", m.snapshot.CachedLogs[0])
	}
	last := m.snapshot.CachedLogs[4]
	if last.Server != "node-012" || last.Available || last.Path != "" {
		t.Fatalf("new source should be listed unread: %+v", last)
	}
	// The next automatic refresh is a partial one; a manual r is full.
	m.inflight = false
	m.rt.loader = &dashLoader{closed: true}
	cmd := m.startRefresh()
	if msg := cmd().(dashboardLoadedMsg); msg.full {
		t.Fatalf("automatic refresh within %v of a full load should skip previews", dashFullEvery)
	}
	m.inflight = false
	next, cmd = m.Update(keyMsg("r"))
	m = next.(model)
	if !m.inflight || cmd == nil {
		t.Fatalf("r should start a refresh")
	}
	var full bool
	for _, msg := range drainBatch(cmd) {
		if lm, ok := msg.(dashboardLoadedMsg); ok {
			full = lm.full
		}
	}
	if !full {
		t.Fatalf("a manual refresh should re-read the log previews")
	}
}

// drainBatch runs a command and, for a tea.Batch, its children.
func drainBatch(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, drainBatch(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// Rectangular zones cover every cell of their area, not just one line.
func TestMouseZonesCoverWholePanels(t *testing.T) {
	m := newTestDash(t, 12, 160, 45)
	m = press(m, "4")
	_ = m.View()
	var z = waitZone(t, dashViewerID())
	if z.EndY-z.StartY < 10 {
		t.Fatalf("viewer zone spans rows %d..%d, want the whole pane", z.StartY, z.EndY)
	}
	mid := tea.MouseMsg{X: (z.StartX + z.EndX) / 2, Y: (z.StartY + z.EndY) / 2, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
	if !m.handleMouse(mid) || !m.viewerFocus {
		t.Fatalf("a click in the middle of the log viewer should focus it")
	}
	m = press(m, "esc", "1")
	_ = m.View()
	z = waitZone(t, dashOverviewBoxID(1))
	inside := tea.MouseMsg{X: z.StartX + 3, Y: z.StartY + 3, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
	if !m.handleMouse(inside) || m.activeTab != tabAlerts {
		t.Fatalf("a click inside the Alerts KPI box should open the Alerts tab")
	}
}

func waitZone(t *testing.T, id string) *zone.ZoneInfo {
	t.Helper()
	for range 250 {
		if z := zone.Get(id); !z.IsZero() {
			return z
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("zone %q not registered", id)
	return nil
}

// Keys that arrive together in one read come as one multi-rune message.
func TestCoalescedKeysAreHandledOneByOne(t *testing.T) {
	m := newTestDash(t, 50, 160, 45)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2jjj")})
	m = next.(model)
	if m.activeTab != tabServers || m.serverIndex != 3 {
		t.Fatalf("tab=%v index=%d, want Servers/3", m.activeTab, m.serverIndex)
	}
	// A confirmation key typed in the same burst as the action never
	// confirms it: the operator has not seen the prompt yet.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("5ay")})
	m = next.(model)
	if m.prompt == nil || m.busy != "" {
		t.Fatalf("prompt should be open and nothing running (busy=%q)", m.busy)
	}
	// Inside the filter, a burst is text.
	m = press(m, "esc", "2", "/")
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("node-01")})
	m = next.(model)
	if m.filters[tabServers] != "node-01" {
		t.Fatalf("filter = %q", m.filters[tabServers])
	}
}
