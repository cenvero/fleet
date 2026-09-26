// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	fleetalerts "github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/core"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"
)

func dashTabID(i int) string { return "dash-tab-" + strconv.Itoa(i) }
func dashRowID(tab, i int) string {
	return "dash-row-" + strconv.Itoa(tab) + "-" + strconv.Itoa(i)
}
func dashColID(i int) string { return "dash-col-" + strconv.Itoa(i) }
func dashHotID(metric, i int) string {
	return "dash-hot-" + strconv.Itoa(metric) + "-" + strconv.Itoa(i)
}
func dashChipID(kind int) string       { return "dash-chip-" + strconv.Itoa(kind) }
func dashAlertRowID(i int) string      { return "dash-ovalert-" + strconv.Itoa(i) }
func dashViewerID() string             { return "dash-viewer" }
func dashHelpID() string               { return "dash-help" }
func dashPromptID(choice int) string   { return "dash-prompt-" + strconv.Itoa(choice) }
func dashRefreshToggleID() string      { return "dash-refresh-toggle" }
func dashOverviewBoxID(box int) string { return "dash-ovbox-" + strconv.Itoa(box) }

// pageStyle is shared with the file manager views.
var pageStyle = lipgloss.NewStyle().
	Padding(1, 2).
	Foreground(lipgloss.Color("#e7ecef")).
	Background(lipgloss.Color("#0a0e14"))

type dashboardTab int

const (
	tabOverview dashboardTab = iota
	tabServers
	tabServices
	tabLogs
	tabAlerts
	tabOps
	dashNumTabs
)

var dashboardTabs = []string{"Overview", "Servers", "Services", "Logs", "Alerts", "Ops"}

// Refresh interval steps for +/-.
var dashIntervals = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute}

const (
	dashDefaultInterval = 5 * time.Second
	dashHistoryPoints   = 120
	dashLogTailLines    = 400
	dashFlashFor        = 6 * time.Second
	dashDebounce        = 120 * time.Millisecond
	// dashFullEvery is how often an automatic refresh also re-reads every
	// cached log preview (manual refreshes always do).
	dashFullEvery = 2 * time.Minute
	// dashTailEvery throttles re-reading the log on screen.
	dashTailEvery = 10 * time.Second
)

// DashboardOptions configures RunDashboardWithOptions.
type DashboardOptions struct {
	// ConfigDir is the controller configuration directory.
	ConfigDir string
	// Token is the RBAC token id the dashboard was launched with (--token or
	// FLEET_TOKEN). Every action the dashboard takes runs as a child `fleet`
	// process that receives this token (via FLEET_TOKEN), so RBAC, cmd-policy
	// and audit attribution apply exactly as they do on the command line.
	Token string
	// Interval is the initial auto-refresh period (default 5s).
	Interval time.Duration
}

// dashRuntime is the dashboard's shared, pointer-held state: the data loader,
// action launcher settings, per-server caches and the rendered-frame cache. The
// model itself stays a value type as Bubble Tea expects.
type dashRuntime struct {
	loader    *dashLoader
	dark      bool
	exe       string
	token     string
	configDir string
	environ   func() []string
	// execProcess hands the terminal to an interactive child (tea.ExecProcess;
	// replaceable in tests).
	execProcess func(*exec.Cmd, tea.ExecCallback) tea.Cmd

	// Frame cache: View returns the previous frame untouched while the model
	// revision and terminal size are unchanged (idle ticks, mouse motion).
	frame    string
	frameRev uint64
	frameW   int
	frameH   int
	frameOK  bool

	hist     map[string]dashHist
	histWant string
	histBusy map[string]bool

	logTail  map[string]dashLogTail
	logWant  string
	logBusy  map[string]bool
	scanBuf  []byte
	lastScan string
}

type dashHist struct {
	stamp          time.Time // Metrics.Timestamp the history was read for
	cpu, mem, disk []float64
	first, last    time.Time
	err            error
}

type dashLogTail struct {
	stamp   string // identity of the preview the tail was read for
	gen     uint64 // data generation it was read in
	at      time.Time
	preview core.CachedLogPreview
	err     error
}

type dashOverlay int

const (
	dashOverlayNone dashOverlay = iota
	dashOverlayHelp
)

type model struct {
	configDir    string
	snapshot     core.DashboardSnapshot
	err          error
	width        int
	height       int
	loading      bool
	activeTab    dashboardTab
	serverIndex  int
	serviceIndex int
	logIndex     int
	alertIndex   int
	auditIndex   int

	// Live data beyond the classic snapshot.
	alertsAll  []fleetalerts.Alert
	alertStats *core.AlertStats
	tags       map[string]map[string]string

	// Refresh state.
	gen        uint64
	loadedAt   time.Time
	loadTook   time.Duration
	lastErr    error
	lastErrAt  time.Time
	inflight   bool
	refreshSeq uint64
	lastStart  time.Time
	lastFull   time.Time
	forceFull  bool
	interval   time.Duration
	paused     bool
	now        time.Time

	// Derived data (immutable; rebuilt on load / filter / sort changes).
	base  *dashBase
	views *dashViews

	// View state.
	offsets     [dashNumTabs]int
	filters     [dashNumTabs]string
	selKeys     [dashNumTabs]string
	sortCol     serverSortCol
	sortDesc    bool
	alertSev    int
	alertState  int
	filtering   bool
	viewerFocus bool
	logScroll   int
	logFollow   bool
	logSearch   string
	zoom        bool
	overlay     dashOverlay
	prompt      *dashPrompt
	flash       string
	flashErr    bool
	flashAt     time.Time
	busy        string

	rt  *dashRuntime
	rev uint64
}

// ---------------------------------------------------------------------------
// Entry points
// ---------------------------------------------------------------------------

// RunDashboard launches the live terminal dashboard for configDir.
func RunDashboard(configDir string) error {
	return RunDashboardWithOptions(DashboardOptions{ConfigDir: configDir})
}

// RunDashboardWithOptions launches the live terminal dashboard.
func RunDashboardWithOptions(opts DashboardOptions) error {
	// Open the App before taking over the terminal: "not initialized" and any
	// start-up warnings print normally, and the same App then serves every
	// background refresh.
	app, err := core.Open(opts.ConfigDir)
	if err != nil {
		if errors.Is(err, core.ErrNotInitialized) {
			return fmt.Errorf("%w; run `fleet init` first", err)
		}
		return err
	}
	loader := newDashLoader(opts.ConfigDir, app)
	defer loader.Close()

	// bubblezone answers "which row/tab is under the mouse"; zones are
	// registered once per rendered frame.
	zone.NewGlobal()
	// Query the terminal background once, before Bubble Tea owns stdin.
	dark := lipgloss.HasDarkBackground()
	exe, _ := os.Executable()

	rt := newDashRuntime(loader, dark, exe, opts.Token, app.ConfigDir)
	m := newDashboardModel(rt, opts.Interval)
	// Cell-motion mouse mode: clicks, wheel and drags only — plain pointer
	// movement does not generate an event (and a frame) per cell.
	_, err = tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}

func newDashRuntime(loader *dashLoader, dark bool, exe, token, configDir string) *dashRuntime {
	return &dashRuntime{
		loader:      loader,
		dark:        dark,
		exe:         exe,
		token:       strings.TrimSpace(token),
		configDir:   configDir,
		environ:     os.Environ,
		execProcess: tea.ExecProcess,
		hist:        map[string]dashHist{},
		histBusy:    map[string]bool{},
		logTail:     map[string]dashLogTail{},
		logBusy:     map[string]bool{},
	}
}

func newDashboardModel(rt *dashRuntime, interval time.Duration) model {
	if interval <= 0 {
		interval = dashDefaultInterval
	}
	m := model{
		width:     120,
		height:    36,
		loading:   true,
		inflight:  true,
		activeTab: tabOverview,
		interval:  interval,
		sortCol:   ssName,
		rt:        rt,
	}
	if rt != nil {
		m.configDir = rt.configDir
	}
	return m
}

// ---------------------------------------------------------------------------
// Messages and commands
// ---------------------------------------------------------------------------

type dashboardLoadedMsg struct {
	data core.DashboardData
	err  error
	seq  uint64
	took time.Duration
	// full is set when log previews were read (see dashLoader.load).
	full bool
}

type dashClockMsg time.Time

type dashHistDueMsg struct{ server string }

type dashHistMsg struct {
	server string
	stamp  time.Time
	points []core.MetricPoint
	err    error
}

type dashLogDueMsg struct{ key string }

type dashLogMsg struct {
	key     string
	stamp   string
	gen     uint64
	preview core.CachedLogPreview
	err     error
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.loadCmd(m.refreshSeq, true), dashClockCmd())
}

func dashClockCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return dashClockMsg(t) })
}

func (m model) loadCmd(seq uint64, full bool) tea.Cmd {
	if m.rt == nil || m.rt.loader == nil {
		return nil
	}
	loader := m.rt.loader
	return func() tea.Msg {
		start := time.Now()
		data, err := loader.load(full)
		return dashboardLoadedMsg{data: data, err: err, seq: seq, took: time.Since(start), full: full}
	}
}

// startRefresh begins a background refresh unless one is already running.
func (m *model) startRefresh() tea.Cmd {
	if m.inflight || m.rt == nil {
		return nil
	}
	now := m.clock()
	full := m.forceFull || m.lastFull.IsZero() || now.Sub(m.lastFull) >= dashFullEvery
	m.forceFull = false
	m.inflight = true
	m.refreshSeq++
	m.lastStart = now
	return m.loadCmd(m.refreshSeq, full)
}

func (m *model) clock() time.Time {
	if m.now.IsZero() {
		return time.Now()
	}
	return m.now
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd, changed := m.update(msg)
	if changed {
		m.rev++
	}
	return m, cmd
}

func (m *model) update(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clampSelections()
		return nil, true
	case dashboardLoadedMsg:
		return m.applyLoad(msg), true
	case dashClockMsg:
		return m.onClock(time.Time(msg)), true
	case dashHistDueMsg:
		return m.fetchHistory(msg.server), false
	case dashHistMsg:
		return nil, m.applyHistory(msg)
	case dashLogDueMsg:
		return m.fetchLogTail(msg.key), false
	case dashLogMsg:
		return nil, m.applyLogTail(msg)
	case dashActionMsg:
		return m.applyAction(msg), true
	case tea.MouseMsg:
		if msg.Action == tea.MouseActionMotion || msg.Action == tea.MouseActionRelease {
			return nil, false
		}
		if !m.handleMouse(msg) {
			return nil, false
		}
		return m.afterMove(), true
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return nil, false
}

func (m *model) applyLoad(msg dashboardLoadedMsg) tea.Cmd {
	m.inflight = false
	now := m.clock()
	if msg.err != nil {
		m.lastErr, m.lastErrAt = msg.err, now
		if m.snapshot.GeneratedAt.IsZero() {
			m.err = msg.err
			m.loading = false
		}
		return nil
	}
	previous := m.snapshot.CachedLogs
	m.snapshot = msg.data.DashboardSnapshot
	if msg.full {
		m.lastFull = now
	} else {
		m.snapshot.CachedLogs = mergeLogPreviews(previous, msg.data.LogSources)
	}
	m.alertsAll = msg.data.Alerts
	stats := msg.data.AlertStats
	m.alertStats = &stats
	m.tags = msg.data.Tags
	m.err, m.lastErr = nil, nil
	m.loading = false
	m.gen++
	m.loadedAt = now
	m.loadTook = msg.took
	m.rebuild(true)
	return m.afterMove()
}

// mergeLogPreviews lists the current log sources, carrying over the previews a
// previous full load read (sources not read yet have an empty Path).
func mergeLogPreviews(previous []core.CachedLogPreview, sources []core.DashboardLogSource) []core.CachedLogPreview {
	byKey := make(map[string]*core.CachedLogPreview, len(previous))
	for i := range previous {
		byKey[previous[i].Server+"\x00"+previous[i].Service] = &previous[i]
	}
	out := make([]core.CachedLogPreview, 0, len(sources))
	for _, src := range sources {
		if p, ok := byKey[src.Server+"\x00"+src.Service]; ok {
			out = append(out, *p)
			continue
		}
		out = append(out, core.CachedLogPreview{Server: src.Server, Service: src.Service})
	}
	return out
}

func (m *model) onClock(t time.Time) tea.Cmd {
	m.now = t
	if m.flash != "" && t.Sub(m.flashAt) > dashFlashFor {
		m.flash = ""
	}
	cmds := []tea.Cmd{dashClockCmd()}
	due := m.lastStart.IsZero() || t.Sub(m.lastStart) >= m.interval
	if !m.paused && !m.inflight && due && m.rt != nil {
		cmds = append(cmds, m.startRefresh())
	}
	return tea.Batch(cmds...)
}

// rebuild recomputes derived data. base=true also rebuilds the per-dataset
// aggregates (after a load); otherwise only filter/sort orders are rebuilt.
func (m *model) rebuild(base bool) {
	if base || m.base == nil {
		m.base = dashBuildBase(&m.snapshot, m.alertsAll, m.alertStats, m.tags, m.clock())
	}
	m.views = dashBuildViews(m, m.base)
	m.restoreSelections()
}

// ensureDerived builds derived data for models constructed without a load
// (tests) or after the snapshot was replaced directly.
func (m *model) ensureDerived() {
	if m.base == nil || m.views == nil {
		m.rebuild(true)
	}
}

// ---------------------------------------------------------------------------
// Selection
// ---------------------------------------------------------------------------

func (m *model) cursorPtr(tab dashboardTab) *int {
	switch tab {
	case tabServers:
		return &m.serverIndex
	case tabServices:
		return &m.serviceIndex
	case tabLogs:
		return &m.logIndex
	case tabAlerts:
		return &m.alertIndex
	case tabOps:
		return &m.auditIndex
	}
	return nil
}

func (m *model) listLen(tab dashboardTab) int {
	m.ensureDerived()
	switch tab {
	case tabServers:
		return len(m.views.servers)
	case tabServices:
		return len(m.views.services)
	case tabLogs:
		return len(m.views.logs)
	case tabAlerts:
		return len(m.views.alerts)
	case tabOps:
		return len(m.views.audit)
	}
	return 0
}

// keyAt returns the persistent identity of row i of a tab's list.
func (m *model) keyAt(tab dashboardTab, i int) string {
	v := m.views
	switch tab {
	case tabServers:
		if i >= 0 && i < len(v.servers) {
			return m.snapshot.Servers[m.base.rows[v.servers[i]].idx].Name
		}
	case tabServices:
		if i >= 0 && i < len(v.services) {
			r := &m.base.services[v.services[i]]
			return r.Server.Name + "\x00" + r.Service.Name
		}
	case tabLogs:
		if i >= 0 && i < len(v.logs) {
			lp := &m.snapshot.CachedLogs[v.logs[i]]
			return lp.Server + "\x00" + lp.Service
		}
	case tabAlerts:
		if i >= 0 && i < len(v.alerts) {
			return m.base.alerts[v.alerts[i]].ID
		}
	case tabOps:
		if i >= 0 && i < len(v.audit) {
			return dashAuditKey(m.snapshot.RecentAudit[v.audit[i]])
		}
	}
	return ""
}

// restoreSelections re-finds each tab's selected item by identity after the
// data, filter or sort order changed, so the selection follows the item
// rather than its row number. An item that disappeared keeps the cursor
// position (clamped) and remembers its key for when it returns.
func (m *model) restoreSelections() {
	for tab := tabServers; tab < dashNumTabs; tab++ {
		cur := m.cursorPtr(tab)
		n := m.listLen(tab)
		key := m.selKeys[tab]
		if tab == tabOps && *cur == 0 {
			// The audit trail is newest-first: an operator watching the
			// newest entry keeps watching the newest entry.
			key = ""
		}
		found := -1
		if key != "" {
			for i := 0; i < n; i++ {
				if m.keyAt(tab, i) == key {
					found = i
					break
				}
			}
		}
		if found >= 0 {
			*cur = found
		} else {
			*cur = clamp(*cur, 0, max(n-1, 0))
			if key == "" {
				m.selKeys[tab] = m.keyAt(tab, *cur)
			}
		}
		m.ensureVisible(tab)
	}
}

func (m *model) setCursor(tab dashboardTab, i int) {
	cur := m.cursorPtr(tab)
	if cur == nil {
		return
	}
	n := m.listLen(tab)
	*cur = clamp(i, 0, max(n-1, 0))
	m.selKeys[tab] = m.keyAt(tab, *cur)
	if tab == tabLogs {
		m.logScroll, m.logFollow = 0, true
	}
	m.ensureVisible(tab)
}

func (m *model) moveSelection(delta int) {
	if m.activeTab == tabLogs && m.viewerFocus {
		m.scrollViewer(delta)
		return
	}
	cur := m.cursorPtr(m.activeTab)
	if cur == nil {
		return
	}
	m.setCursor(m.activeTab, *cur+delta)
}

// ensureVisible scrolls a tab's window so its cursor row is on screen.
func (m *model) ensureVisible(tab dashboardTab) {
	cur := m.cursorPtr(tab)
	if cur == nil {
		return
	}
	rows := m.visibleRows(tab)
	n := m.listLen(tab)
	off := m.offsets[tab]
	if rows <= 0 {
		m.offsets[tab] = clamp(*cur, 0, max(n-1, 0))
		return
	}
	if *cur < off {
		off = *cur
	}
	if *cur >= off+rows {
		off = *cur - rows + 1
	}
	off = clamp(off, 0, max(n-rows, 0))
	m.offsets[tab] = off
}

func (m *model) clampSelections() {
	for tab := tabServers; tab < dashNumTabs; tab++ {
		cur := m.cursorPtr(tab)
		*cur = clamp(*cur, 0, max(m.listLen(tab)-1, 0))
		m.ensureVisible(tab)
	}
}

func (m *model) selectedServer() (*core.ServerRecord, *dashSrvRow) {
	m.ensureDerived()
	if m.serverIndex < 0 || m.serverIndex >= len(m.views.servers) {
		return nil, nil
	}
	r := &m.base.rows[m.views.servers[m.serverIndex]]
	return &m.snapshot.Servers[r.idx], r
}

func (m *model) selectedService() *serviceRow {
	m.ensureDerived()
	if m.serviceIndex < 0 || m.serviceIndex >= len(m.views.services) {
		return nil
	}
	return &m.base.services[m.views.services[m.serviceIndex]]
}

func (m *model) selectedLog() *core.CachedLogPreview {
	m.ensureDerived()
	if m.logIndex < 0 || m.logIndex >= len(m.views.logs) {
		return nil
	}
	return &m.snapshot.CachedLogs[m.views.logs[m.logIndex]]
}

func (m *model) selectedAlert() *fleetalerts.Alert {
	m.ensureDerived()
	if m.alertIndex < 0 || m.alertIndex >= len(m.views.alerts) {
		return nil
	}
	return &m.base.alerts[m.views.alerts[m.alertIndex]]
}

// jumpToServer selects a server by name on the Servers tab, clearing a filter
// that would hide it.
func (m *model) jumpToServer(name string) {
	m.ensureDerived()
	m.activeTab = tabServers
	m.zoom = false
	if _, ok := m.base.byName[name]; ok {
		found := false
		for i := range m.views.servers {
			if m.keyAt(tabServers, i) == name {
				found = true
				break
			}
		}
		if !found && m.filters[tabServers] != "" {
			m.filters[tabServers] = ""
			m.views = dashBuildViews(m, m.base)
		}
	}
	m.selKeys[tabServers] = name
	m.restoreSelections()
}

// afterMove schedules the (debounced) on-demand reads the current selection
// needs: metric history for the selected server, the cached log tail for the
// selected log.
func (m *model) afterMove() tea.Cmd {
	if m.rt == nil || m.rt.loader == nil {
		return nil
	}
	var cmds []tea.Cmd
	if m.activeTab == tabServers || m.activeTab == tabOverview {
		if s, _ := m.selectedServer(); s != nil && m.activeTab == tabServers {
			if e, ok := m.rt.hist[s.Name]; !ok || !e.stamp.Equal(s.Metrics.Timestamp) {
				m.rt.histWant = s.Name
				name := s.Name
				cmds = append(cmds, tea.Tick(dashDebounce, func(time.Time) tea.Msg { return dashHistDueMsg{server: name} }))
			}
		}
	}
	if m.activeTab == tabLogs {
		if lp := m.selectedLog(); lp != nil {
			key := lp.Server + "\x00" + lp.Service
			e, ok := m.rt.logTail[key]
			stale := ok && e.gen != m.gen && m.clock().Sub(e.at) >= dashTailEvery
			if !ok || e.stamp != dashLogStamp(lp) || stale {
				m.rt.logWant = key
				cmds = append(cmds, tea.Tick(dashDebounce, func(time.Time) tea.Msg { return dashLogDueMsg{key: key} }))
			}
		}
	}
	return tea.Batch(cmds...)
}

func dashLogStamp(lp *core.CachedLogPreview) string {
	if len(lp.Lines) == 0 {
		return "empty"
	}
	last := lp.Lines[len(lp.Lines)-1]
	return strconv.Itoa(last.Number) + "\x00" + last.Text
}

func (m *model) fetchHistory(server string) tea.Cmd {
	if m.rt == nil || m.rt.histWant != server || m.rt.histBusy[server] {
		return nil
	}
	s, _ := m.selectedServer()
	if s == nil || s.Name != server {
		return nil
	}
	stamp := s.Metrics.Timestamp
	m.rt.histBusy[server] = true
	loader := m.rt.loader
	return func() tea.Msg {
		var points []core.MetricPoint
		err := loader.with(func(app *core.App) error {
			var err error
			points, err = app.RecentMetricHistory(server, dashHistoryPoints)
			return err
		})
		return dashHistMsg{server: server, stamp: stamp, points: points, err: err}
	}
}

func (m *model) applyHistory(msg dashHistMsg) bool {
	if m.rt == nil {
		return false
	}
	delete(m.rt.histBusy, msg.server)
	if len(m.rt.hist) > 256 {
		clear(m.rt.hist)
	}
	e := dashHist{stamp: msg.stamp, err: msg.err}
	for _, p := range msg.points {
		e.cpu = append(e.cpu, p.CPU)
		e.mem = append(e.mem, p.Memory)
		e.disk = append(e.disk, p.Disk)
	}
	if len(msg.points) > 0 {
		e.first, e.last = msg.points[0].Timestamp, msg.points[len(msg.points)-1].Timestamp
	}
	m.rt.hist[msg.server] = e
	s, _ := m.selectedServer()
	return s != nil && s.Name == msg.server
}

func (m *model) fetchLogTail(key string) tea.Cmd {
	if m.rt == nil || m.rt.logWant != key || m.rt.logBusy[key] {
		return nil
	}
	lp := m.selectedLog()
	if lp == nil || lp.Server+"\x00"+lp.Service != key {
		return nil
	}
	server, service, stamp, gen := lp.Server, lp.Service, dashLogStamp(lp), m.gen
	m.rt.logBusy[key] = true
	loader := m.rt.loader
	return func() tea.Msg {
		var preview core.CachedLogPreview
		err := loader.with(func(app *core.App) error {
			var err error
			preview, err = app.DashboardLogTail(server, service, dashLogTailLines)
			return err
		})
		return dashLogMsg{key: key, stamp: stamp, gen: gen, preview: preview, err: err}
	}
}

func (m *model) applyLogTail(msg dashLogMsg) bool {
	if m.rt == nil {
		return false
	}
	delete(m.rt.logBusy, msg.key)
	if len(m.rt.logTail) > 64 {
		clear(m.rt.logTail)
	}
	m.rt.logTail[msg.key] = dashLogTail{stamp: msg.stamp, gen: msg.gen, at: m.clock(), preview: msg.preview, err: msg.err}
	lp := m.selectedLog()
	return lp != nil && lp.Server+"\x00"+lp.Service == msg.key
}

// logLines returns the lines the log viewer shows for the selected source: the
// on-demand tail when loaded, else the snapshot preview.
func (m *model) logLines(lp *core.CachedLogPreview) ([]string, []int, bool) {
	src := lp.Lines
	full := false
	if m.rt != nil {
		if e, ok := m.rt.logTail[lp.Server+"\x00"+lp.Service]; ok && e.err == nil {
			src = e.preview.Lines
			full = true
		}
	}
	terms := dashTerms(m.logSearch)
	texts := make([]string, 0, len(src))
	nums := make([]int, 0, len(src))
	for _, line := range src {
		if len(terms) > 0 && !dashMatchAll(strings.ToLower(line.Text), terms) {
			continue
		}
		texts = append(texts, line.Text)
		nums = append(nums, line.Number)
	}
	return texts, nums, full
}

func (m *model) scrollViewer(delta int) {
	lp := m.selectedLog()
	if lp == nil {
		return
	}
	texts, _, _ := m.logLines(lp)
	rows := m.viewerRows()
	maxTop := max(len(texts)-rows, 0)
	top := maxTop - m.logScroll
	if m.logFollow {
		top = maxTop
	}
	top = clamp(top+delta, 0, maxTop)
	m.logScroll = maxTop - top
	m.logFollow = m.logScroll == 0
}

// ---------------------------------------------------------------------------
// Keyboard
// ---------------------------------------------------------------------------

func (m *model) handleKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	key := msg.String()
	if key == "ctrl+c" {
		return tea.Quit, false
	}
	if m.prompt != nil {
		return m.handlePromptKey(key), true
	}
	if m.filtering {
		return m.handleFilterKey(msg)
	}
	if m.overlay == dashOverlayHelp {
		switch key {
		case "?", "esc", "q", "enter", "h", "f1":
			m.overlay = dashOverlayNone
			return nil, true
		}
		return nil, false
	}
	m.ensureDerived()
	switch key {
	case "q":
		return tea.Quit, false
	case "?", "f1":
		m.overlay = dashOverlayHelp
		return nil, true
	case "r", "ctrl+r":
		if m.inflight {
			m.setFlash("refresh already in progress", false)
			return nil, true
		}
		m.setFlash("refreshing…", false)
		m.forceFull = true
		return m.startRefresh(), true
	case "p":
		m.paused = !m.paused
		if m.paused {
			m.setFlash("auto-refresh paused (p to resume)", false)
		} else {
			m.setFlash("auto-refresh resumed", false)
		}
		return nil, true
	case "+", "=":
		m.stepInterval(1)
		return nil, true
	case "-", "_":
		m.stepInterval(-1)
		return nil, true
	case "tab", "right", "l":
		m.switchTab(dashboardTab((int(m.activeTab) + 1) % len(dashboardTabs)))
		return m.afterMove(), true
	case "shift+tab", "left", "h":
		m.switchTab(dashboardTab((int(m.activeTab) - 1 + len(dashboardTabs)) % len(dashboardTabs)))
		return m.afterMove(), true
	case "1", "2", "3", "4", "5", "6":
		m.switchTab(dashboardTab(key[0] - '1'))
		return m.afterMove(), true
	case "up", "k":
		m.moveSelection(-1)
		return m.afterMove(), true
	case "down", "j":
		m.moveSelection(1)
		return m.afterMove(), true
	case "pgup", "ctrl+u", "ctrl+b":
		m.moveSelection(-max(m.pageSize(), 1))
		return m.afterMove(), true
	case "pgdown", "ctrl+d", "ctrl+f":
		m.moveSelection(max(m.pageSize(), 1))
		return m.afterMove(), true
	case "home", "g":
		m.moveSelection(-1 << 30)
		return m.afterMove(), true
	case "end", "G":
		m.moveSelection(1 << 30)
		return m.afterMove(), true
	case "/":
		if m.activeTab == tabOverview {
			return nil, false
		}
		m.filtering = true
		return nil, true
	case "esc":
		switch {
		case m.zoom:
			m.zoom = false
		case m.viewerFocus:
			m.viewerFocus = false
		case m.activeTab == tabLogs && m.logSearch != "":
			m.logSearch = ""
		case m.filters[m.activeTab] != "":
			m.filters[m.activeTab] = ""
			m.rebuild(false)
		default:
			return nil, false
		}
		return m.afterMove(), true
	case "enter":
		return m.handleEnter(), true
	case "o":
		if m.activeTab == tabServers {
			m.sortCol = (m.sortCol + 1) % ssCount
			m.sortDesc = dashDefaultDesc(m.sortCol)
			m.rebuild(false)
			return m.afterMove(), true
		}
	case "O":
		if m.activeTab == tabServers {
			m.sortDesc = !m.sortDesc
			m.rebuild(false)
			return m.afterMove(), true
		}
	case "v":
		if m.activeTab == tabAlerts {
			m.alertSev = (m.alertSev + 1) % len(alertSevFilters)
			m.rebuild(false)
			return nil, true
		}
	case "t":
		if m.activeTab == tabAlerts {
			m.alertState = (m.alertState + 1) % len(alertStateFilters)
			m.rebuild(false)
			return nil, true
		}
	}
	if cmd, ok := m.actionKey(key); ok {
		return cmd, true
	}
	return nil, false
}

func (m *model) handleEnter() tea.Cmd {
	switch m.activeTab {
	case tabOverview:
		if len(m.base.fleet.topCPU) > 0 {
			m.jumpToServer(m.snapshot.Servers[m.base.rows[m.base.fleet.topCPU[0]].idx].Name)
		} else {
			m.switchTab(tabServers)
		}
		return m.afterMove()
	case tabLogs:
		g := m.geom(tabLogs)
		if g.hidden {
			m.zoom = !m.zoom
			m.viewerFocus = m.zoom
		} else {
			m.viewerFocus = !m.viewerFocus
		}
		return nil
	case tabOps:
		if g := m.geom(tabOps); g.hidden || g.aside.w == 0 {
			m.zoom = !m.zoom
		}
	default:
		if m.geom(m.activeTab).hidden {
			m.zoom = !m.zoom
		}
	}
	return nil
}

func (m *model) switchTab(tab dashboardTab) {
	if tab == m.activeTab {
		return
	}
	m.activeTab = tab
	m.zoom = false
	m.viewerFocus = false
	m.clampSelections()
}

func (m *model) stepInterval(dir int) {
	idx := 0
	for i, d := range dashIntervals {
		if d <= m.interval {
			idx = i
		}
	}
	idx = clamp(idx+dir, 0, len(dashIntervals)-1)
	m.interval = dashIntervals[idx]
	m.setFlash("auto-refresh every "+dashFmtInterval(m.interval), false)
}

func dashFmtInterval(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	return strconv.Itoa(int(d/time.Second)) + "s"
}

func (m *model) pageSize() int {
	if m.activeTab == tabLogs && m.viewerFocus {
		return m.viewerRows() - 1
	}
	return m.visibleRows(m.activeTab) - 1
}

func (m *model) setFlash(text string, isErr bool) {
	m.flash = text
	m.flashErr = isErr
	m.flashAt = m.clock()
}

func (m *model) handleFilterKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	key := msg.String()
	target := &m.filters[m.activeTab]
	logSearch := m.activeTab == tabLogs && m.viewerFocus
	if logSearch {
		target = &m.logSearch
	}
	switch key {
	case "enter":
		m.filtering = false
		return nil, true
	case "esc":
		m.filtering = false
		*target = ""
	case "backspace", "ctrl+h":
		if r := []rune(*target); len(r) > 0 {
			*target = string(r[:len(r)-1])
		}
	case "ctrl+u":
		*target = ""
	case "up", "down":
		if key == "up" {
			m.moveSelection(-1)
		} else {
			m.moveSelection(1)
		}
		return m.afterMove(), true
	default:
		if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
			if len(*target) < 128 {
				*target += dashClean(string(msg.Runes))
			}
		} else {
			return nil, false
		}
	}
	if logSearch {
		m.logScroll, m.logFollow = 0, true
		return nil, true
	}
	m.rebuild(false)
	return m.afterMove(), true
}

// ---------------------------------------------------------------------------
// Mouse
// ---------------------------------------------------------------------------

func (m *model) handleMouse(msg tea.MouseMsg) bool {
	if msg.Action == tea.MouseActionMotion {
		return false
	}
	m.ensureDerived()
	switch {
	case msg.Button == tea.MouseButtonWheelUp && msg.Action == tea.MouseActionPress:
		if m.activeTab == tabLogs && zone.Get(dashViewerID()).InBounds(msg) {
			m.scrollViewer(-3)
			return true
		}
		m.moveSelection(-1)
		return true
	case msg.Button == tea.MouseButtonWheelDown && msg.Action == tea.MouseActionPress:
		if m.activeTab == tabLogs && zone.Get(dashViewerID()).InBounds(msg) {
			m.scrollViewer(3)
			return true
		}
		m.moveSelection(1)
		return true
	case msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress:
		if m.overlay == dashOverlayHelp {
			m.overlay = dashOverlayNone
			return true
		}
		for i := range dashboardTabs {
			if zone.Get(dashTabID(i)).InBounds(msg) {
				m.switchTab(dashboardTab(i))
				return true
			}
		}
		if m.prompt != nil {
			return false
		}
		if zone.Get(dashRefreshToggleID()).InBounds(msg) {
			m.paused = !m.paused
			return true
		}
		switch m.activeTab {
		case tabOverview:
			return m.clickOverview(msg)
		case tabServers:
			for c := range ssCount {
				if zone.Get(dashColID(int(c))).InBounds(msg) {
					if m.sortCol == c {
						m.sortDesc = !m.sortDesc
					} else {
						m.sortCol, m.sortDesc = c, dashDefaultDesc(c)
					}
					m.rebuild(false)
					return true
				}
			}
		case tabAlerts:
			if zone.Get(dashChipID(0)).InBounds(msg) {
				m.alertSev = (m.alertSev + 1) % len(alertSevFilters)
				m.rebuild(false)
				return true
			}
			if zone.Get(dashChipID(1)).InBounds(msg) {
				m.alertState = (m.alertState + 1) % len(alertStateFilters)
				m.rebuild(false)
				return true
			}
		case tabLogs:
			if zone.Get(dashViewerID()).InBounds(msg) {
				m.viewerFocus = true
				return true
			}
		}
		// Rows of the active tab: only the visible window is marked.
		if cur := m.cursorPtr(m.activeTab); cur != nil {
			start := m.offsets[m.activeTab]
			end := min(start+max(m.visibleRows(m.activeTab), 1), m.listLen(m.activeTab))
			for i := start; i < end; i++ {
				if zone.Get(dashRowID(int(m.activeTab), i)).InBounds(msg) {
					m.setCursor(m.activeTab, i)
					if m.activeTab == tabLogs {
						m.viewerFocus = false
					}
					return true
				}
			}
		}
	}
	return false
}

func (m *model) clickOverview(msg tea.MouseMsg) bool {
	f := &m.base.fleet
	for metric, list := range [][]int{f.topCPU, f.topMem, f.topDisk} {
		for i := range list {
			if zone.Get(dashHotID(metric, i)).InBounds(msg) {
				m.jumpToServer(m.snapshot.Servers[m.base.rows[list[i]].idx].Name)
				return true
			}
		}
	}
	open := m.openAlertOrder()
	for i := range open {
		if zone.Get(dashAlertRowID(i)).InBounds(msg) {
			m.jumpToAlert(m.base.alerts[open[i]].ID)
			return true
		}
	}
	for box, tab := range []dashboardTab{tabServers, tabAlerts} {
		if zone.Get(dashOverviewBoxID(box)).InBounds(msg) {
			m.switchTab(tab)
			return true
		}
	}
	return false
}

// jumpToAlert selects an alert by id on the Alerts tab, resetting filters that
// would hide it.
func (m *model) jumpToAlert(id string) {
	m.switchTab(tabAlerts)
	m.alertSev, m.alertState = 0, 0
	m.filters[tabAlerts] = ""
	m.rebuild(false)
	m.selKeys[tabAlerts] = id
	m.restoreSelections()
}

// ---------------------------------------------------------------------------
// Helpers shared with other views
// ---------------------------------------------------------------------------

func serviceState(service core.ServiceRecord) string {
	state := dashIfEmpty(service.ActiveState)
	if service.SubState != "" {
		state += "/" + service.SubState
	}
	return state
}

// truncate shortens input to width bytes with a trailing "...". Used by the
// file manager views.
func truncate(input string, width int) string {
	if width < 4 || len(input) <= width {
		return input
	}
	return input[:width-3] + "..."
}

func dashIfEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func clamp(value, low, high int) int {
	if high < low {
		return low
	}
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
