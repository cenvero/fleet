// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	fleetalerts "github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/version"
	"golang.org/x/mod/semver"
)

// ---------------------------------------------------------------------------
// Loader: one long-lived App shared by every background refresh
// ---------------------------------------------------------------------------

// dashRecentAudit is how many audit entries the live console keeps (Ops tab).
const dashRecentAudit = 200

var errDashboardClosed = errors.New("dashboard is closing")

// dashLoader owns the single core.App the dashboard reads through. Every
// refresh, history and log-tail read runs in a tea.Cmd goroutine; the mutex
// serialises them (and Close) so the App is never used concurrently with its
// own shutdown. The App is reopened only when config.toml changes on disk (for
// example after `fleet database shift`), never per refresh.
type dashLoader struct {
	mu        sync.Mutex
	configDir string
	app       *core.App
	stamp     dashFileStamp
	closed    bool
	// servers skips re-decoding unchanged server files between refreshes.
	servers core.ServerListCache
}

type dashFileStamp struct {
	size    int64
	modTime time.Time
}

func dashStatStamp(path string) dashFileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return dashFileStamp{}
	}
	return dashFileStamp{size: info.Size(), modTime: info.ModTime()}
}

func newDashLoader(configDir string, app *core.App) *dashLoader {
	l := &dashLoader{configDir: configDir, app: app}
	if app != nil {
		l.configDir = app.ConfigDir
		l.stamp = dashStatStamp(core.ConfigPath(app.ConfigDir))
	}
	return l
}

func (l *dashLoader) ensure() error {
	stamp := dashStatStamp(core.ConfigPath(l.configDir))
	if l.app != nil && stamp == l.stamp {
		return nil
	}
	app, err := core.Open(l.configDir)
	if err != nil {
		if errors.Is(err, core.ErrNotInitialized) {
			err = fmt.Errorf("%w; run `fleet init` first", err)
		}
		return err
	}
	if l.app != nil {
		_ = l.app.Close()
	}
	l.app, l.stamp = app, stamp
	return nil
}

// with runs fn against the shared App.
func (l *dashLoader) with(fn func(*core.App) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errDashboardClosed
	}
	if err := l.ensure(); err != nil {
		return err
	}
	return fn(l.app)
}

func (l *dashLoader) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	if l.app != nil {
		_ = l.app.Close()
		l.app = nil
	}
}

// load reads a dashboard dataset. Only a full load reads every tracked
// service's cached log for the previews; periodic refreshes list the log
// sources and leave the reading to the one log on screen.
func (l *dashLoader) load(full bool) (core.DashboardData, error) {
	var data core.DashboardData
	err := l.with(func(app *core.App) error {
		var err error
		data, err = app.DashboardData(core.DashboardOptions{RecentAudit: dashRecentAudit, SkipLogPreviews: !full, ServerCache: &l.servers})
		return err
	})
	return data, err
}

// ---------------------------------------------------------------------------
// Derived data
// ---------------------------------------------------------------------------

type dashSrvStatus uint8

const (
	dsOnline dashSrvStatus = iota
	dsDegraded
	dsOffline
)

func (s dashSrvStatus) label() string {
	switch s {
	case dsDegraded:
		return "degraded"
	case dsOffline:
		return "offline"
	default:
		return "online"
	}
}

func (s dashSrvStatus) glyph() string {
	switch s {
	case dsDegraded:
		return "◐"
	case dsOffline:
		return "○"
	default:
		return "●"
	}
}

func (s dashSrvStatus) style() dstyle {
	switch s {
	case dsDegraded:
		return sWarn
	case dsOffline:
		return sCrit
	default:
		return sOK
	}
}

// srvRow is the per-server data every frame needs, computed once per load.
type dashSrvRow struct {
	idx        int
	status     dashSrvStatus
	openAlerts int
	worstSev   fleetalerts.Severity
	failedSvc  int
	tags       string // "env=prod role=web", keys sorted
	agent      string // display version: semver, raw build string, or "-"
	agentSem   string // canonical semver ("" when not a release build)
	agentOld   bool
	hasMetrics bool
	search     string // lower-cased haystack for the filter
}

type serviceRow struct {
	// Server and Service point into the snapshot the row was derived from
	// (never copied: sorting rows of full server records is expensive).
	Server    *core.ServerRecord
	Service   *core.ServiceRecord
	Reachable bool
	srv       int // index into snapshot.Servers
	rank      int
	search    string
}

// fleetAgg holds the overview numbers.
type dashFleetAgg struct {
	total, online, degraded, offline int
	services, failedSvc, startingSvc int
	reporting                        int
	avgCPU, avgMem, avgDisk          float64
	p95CPU, p95Mem, p95Disk          float64
	hotCPU, hotMem, hotDisk          int
	topCPU, topMem, topDisk          []int // row indices, hottest first
	newestAgent                      string
	agentOld, agentUnknown           int
	alertingServers                  int
}

// dashBase is everything derived from one loaded dataset (independent of
// filters and sort order). It is immutable once built.
type dashBase struct {
	rows     []dashSrvRow
	byName   map[string]int
	services []serviceRow // default order: problems first
	alerts   []fleetalerts.Alert
	stats    core.AlertStats
	fleet    dashFleetAgg
	now      time.Time
}

// dashViews holds the filtered/sorted orders for each list tab. Immutable.
type dashViews struct {
	servers  []int // indices into base.rows
	services []int // indices into base.services
	logs     []int // indices into snapshot.CachedLogs
	alerts   []int // indices into base.alerts
	audit    []int // indices into snapshot.RecentAudit
}

func dashBuildBase(snap *core.DashboardSnapshot, allAlerts []fleetalerts.Alert, stats *core.AlertStats, tags map[string]map[string]string, now time.Time) *dashBase {
	if allAlerts == nil {
		allAlerts = snap.RecentAlerts
	}
	b := &dashBase{
		rows:   make([]dashSrvRow, len(snap.Servers)),
		byName: make(map[string]int, len(snap.Servers)),
		alerts: allAlerts,
		now:    now,
	}
	if stats != nil {
		b.stats = *stats
	} else {
		b.stats = core.ComputeAlertStats(allAlerts, now)
	}

	// Worst open alert per server.
	type alertAgg struct {
		n     int
		worst fleetalerts.Severity
	}
	perServer := map[string]alertAgg{}
	for _, a := range allAlerts {
		if a.Server == "" || core.AlertState(a, now) != "open" {
			continue
		}
		agg := perServer[a.Server]
		agg.n++
		if dashSevRank(a.Severity) > dashSevRank(agg.worst) {
			agg.worst = a.Severity
		}
		perServer[a.Server] = agg
	}
	b.fleet.alertingServers = len(perServer)

	// Newest agent version in the fleet.
	newest := ""
	for i := range snap.Servers {
		if v, ok := version.NormalizeSemVer(snap.Servers[i].Observed.AgentVersion); ok {
			if newest == "" || semver.Compare(v, newest) > 0 {
				newest = v
			}
		}
	}
	b.fleet.newestAgent = newest

	var cpus, mems, disks []float64
	for i := range snap.Servers {
		s := &snap.Servers[i]
		r := dashSrvRow{idx: i}
		agg := perServer[s.Name]
		r.openAlerts, r.worstSev = agg.n, agg.worst
		for _, svc := range s.Services {
			switch dashSvcRank(svc) {
			case 0:
				r.failedSvc++
				b.fleet.failedSvc++
			case 2:
				b.fleet.startingSvc++
			}
		}
		b.fleet.services += len(s.Services)
		switch {
		case !s.Observed.Reachable:
			r.status = dsOffline
			b.fleet.offline++
		case r.failedSvc > 0 || agg.worst == fleetalerts.SeverityCritical || agg.worst == fleetalerts.SeverityWarning:
			r.status = dsDegraded
			b.fleet.degraded++
		default:
			r.status = dsOnline
			b.fleet.online++
		}
		if v, ok := version.NormalizeSemVer(s.Observed.AgentVersion); ok {
			r.agent, r.agentSem = v, v
			if newest != "" && semver.Compare(v, newest) < 0 {
				r.agentOld = true
				b.fleet.agentOld++
			}
		} else {
			// Non-release builds ("dev") are shown as reported.
			r.agent = dashIfEmpty(dashClean(strings.TrimSpace(s.Observed.AgentVersion)))
			b.fleet.agentUnknown++
		}
		r.tags = dashFormatTags(tags[s.Name])
		r.hasMetrics = !s.Metrics.Timestamp.IsZero() || s.Metrics.CPUPercent > 0 || s.Metrics.MemoryPercent > 0 || s.Metrics.DiskPercent > 0
		if r.hasMetrics && s.Observed.Reachable {
			b.fleet.reporting++
			cpus = append(cpus, s.Metrics.CPUPercent)
			mems = append(mems, s.Metrics.MemoryPercent)
			disks = append(disks, s.Metrics.DiskPercent)
			if s.Metrics.CPUPercent >= dashCPUWarn {
				b.fleet.hotCPU++
			}
			if s.Metrics.MemoryPercent >= dashMemWarn {
				b.fleet.hotMem++
			}
			if s.Metrics.DiskPercent >= dashDiskWarn {
				b.fleet.hotDisk++
			}
		}
		r.search = strings.ToLower(strings.Join([]string{
			s.Name, r.tags, r.status.label(), string(s.Mode), s.Address, r.agent, s.Observed.NodeName,
		}, " "))
		b.rows[i] = r
		b.byName[s.Name] = i
	}
	b.fleet.total = len(snap.Servers)
	b.fleet.avgCPU, b.fleet.p95CPU = dashMeanP95(cpus)
	b.fleet.avgMem, b.fleet.p95Mem = dashMeanP95(mems)
	b.fleet.avgDisk, b.fleet.p95Disk = dashMeanP95(disks)
	b.fleet.topCPU = dashTopRows(snap, b.rows, func(s *core.ServerRecord) float64 { return s.Metrics.CPUPercent })
	b.fleet.topMem = dashTopRows(snap, b.rows, func(s *core.ServerRecord) float64 { return s.Metrics.MemoryPercent })
	b.fleet.topDisk = dashTopRows(snap, b.rows, func(s *core.ServerRecord) float64 { return s.Metrics.DiskPercent })

	// Services, problems first.
	for i := range snap.Servers {
		s := &snap.Servers[i]
		for j := range s.Services {
			svc := &s.Services[j]
			b.services = append(b.services, serviceRow{
				Server:    s,
				Service:   svc,
				Reachable: s.Observed.Reachable,
				srv:       i,
				rank:      dashSvcRank(*svc),
				search:    strings.ToLower(s.Name + " " + svc.Name + " " + serviceState(*svc) + " " + svc.Description),
			})
		}
	}
	sort.SliceStable(b.services, func(i, j int) bool {
		a, c := &b.services[i], &b.services[j]
		if a.rank != c.rank {
			return a.rank < c.rank
		}
		if a.Service.Critical != c.Service.Critical {
			return a.Service.Critical
		}
		if a.Server.Name != c.Server.Name {
			return a.Server.Name < c.Server.Name
		}
		return a.Service.Name < c.Service.Name
	})
	return b
}

// svcRank orders services problems-first: 0 failed, 1 inactive/unknown on a
// critical service, 2 activating, 3 healthy/other.
func dashSvcRank(svc core.ServiceRecord) int {
	state := strings.ToLower(svc.ActiveState)
	switch {
	case strings.Contains(state, "failed"):
		return 0
	case svc.Critical && state != "" && state != "active" && !strings.Contains(state, "activating"):
		return 1
	case strings.Contains(state, "activating") || strings.Contains(state, "reloading"):
		return 2
	default:
		return 3
	}
}

func dashSevRank(s fleetalerts.Severity) int {
	switch s {
	case fleetalerts.SeverityCritical:
		return 3
	case fleetalerts.SeverityWarning:
		return 2
	case fleetalerts.SeverityInfo:
		return 1
	default:
		return 0
	}
}

func dashFormatTags(tags map[string]string) string {
	if len(tags) == 0 {
		return ""
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+tags[k])
	}
	return strings.Join(parts, " ")
}

func dashMeanP95(values []float64) (float64, float64) {
	if len(values) == 0 {
		return 0, 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	idx := int(float64(len(sorted)-1) * 0.95)
	return sum / float64(len(values)), sorted[idx]
}

const dashTopN = 16

func dashTopRows(snap *core.DashboardSnapshot, rows []dashSrvRow, metric func(*core.ServerRecord) float64) []int {
	// Keep the dashTopN hottest reachable servers with an insertion into a
	// small sorted buffer: O(n·k) with k=16, no full sort per metric.
	type hot struct {
		v    float64
		name string
		i    int
	}
	top := make([]hot, 0, dashTopN+1)
	for i := range rows {
		s := &snap.Servers[rows[i].idx]
		if !rows[i].hasMetrics || !s.Observed.Reachable {
			continue
		}
		h := hot{v: metric(s), name: s.Name, i: i}
		pos := len(top)
		for pos > 0 && (top[pos-1].v < h.v || (top[pos-1].v == h.v && top[pos-1].name > h.name)) {
			pos--
		}
		if pos >= dashTopN {
			continue
		}
		top = append(top, hot{})
		copy(top[pos+1:], top[pos:])
		top[pos] = h
		if len(top) > dashTopN {
			top = top[:dashTopN]
		}
	}
	out := make([]int, len(top))
	for k, h := range top {
		out[k] = h.i
	}
	return out
}

// ---------------------------------------------------------------------------
// Sorting and filtering
// ---------------------------------------------------------------------------

type serverSortCol int

const (
	ssName serverSortCol = iota
	ssStatus
	ssCPU
	ssMem
	ssDisk
	ssLoad
	ssSeen
	ssAgent
	ssMode
	ssCount
)

var serverSortNames = [ssCount]string{"name", "status", "cpu", "mem", "disk", "load", "seen", "agent", "mode"}

// filterTerms splits a filter into lower-cased AND terms.
func dashTerms(filter string) []string {
	return strings.Fields(strings.ToLower(filter))
}

func dashMatchAll(haystack string, terms []string) bool {
	for _, t := range terms {
		if !strings.Contains(haystack, t) {
			return false
		}
	}
	return true
}

func dashBuildViews(m *model, b *dashBase) *dashViews {
	v := &dashViews{}
	snap := &m.snapshot

	// Servers.
	terms := dashTerms(m.filters[tabServers])
	v.servers = make([]int, 0, len(b.rows))
	for i := range b.rows {
		if dashMatchAll(b.rows[i].search, terms) {
			v.servers = append(v.servers, i)
		}
	}
	col, desc := m.sortCol, m.sortDesc
	sort.SliceStable(v.servers, func(i, j int) bool {
		ra, rb := &b.rows[v.servers[i]], &b.rows[v.servers[j]]
		c := dashCompareServers(snap, ra, rb, col)
		if c == 0 {
			// Stable tie-break by name, always ascending.
			return snap.Servers[ra.idx].Name < snap.Servers[rb.idx].Name
		}
		if desc {
			return c > 0
		}
		return c < 0
	})

	// Services.
	terms = dashTerms(m.filters[tabServices])
	v.services = make([]int, 0, len(b.services))
	for i := range b.services {
		if dashMatchAll(b.services[i].search, terms) {
			v.services = append(v.services, i)
		}
	}

	// Logs.
	terms = dashTerms(m.filters[tabLogs])
	v.logs = make([]int, 0, len(snap.CachedLogs))
	for i := range snap.CachedLogs {
		lp := &snap.CachedLogs[i]
		if len(terms) == 0 || dashMatchAll(strings.ToLower(lp.Server+" "+lp.Service), terms) {
			v.logs = append(v.logs, i)
		}
	}

	// Alerts: severity/state filters + text, open first, then severity, newest.
	terms = dashTerms(m.filters[tabAlerts])
	now := b.now
	v.alerts = make([]int, 0, len(b.alerts))
	for i := range b.alerts {
		a := &b.alerts[i]
		if sev := alertSevFilters[m.alertSev]; sev != "" && string(a.Severity) != sev {
			continue
		}
		if st := alertStateFilters[m.alertState]; st != "" && core.AlertState(*a, now) != st {
			continue
		}
		if len(terms) > 0 && !dashMatchAll(strings.ToLower(a.ID+" "+a.Server+" "+a.Code+" "+a.Message+" "+string(a.Severity)), terms) {
			continue
		}
		v.alerts = append(v.alerts, i)
	}
	sort.SliceStable(v.alerts, func(i, j int) bool {
		a, c := &b.alerts[v.alerts[i]], &b.alerts[v.alerts[j]]
		oa, oc := core.AlertState(*a, now) == "open", core.AlertState(*c, now) == "open"
		if oa != oc {
			return oa
		}
		if ra, rc := dashSevRank(a.Severity), dashSevRank(c.Severity); ra != rc {
			return ra > rc
		}
		return dashAlertTime(*a).After(dashAlertTime(*c))
	})

	// Audit (already newest first).
	terms = dashTerms(m.filters[tabOps])
	v.audit = make([]int, 0, len(snap.RecentAudit))
	for i := range snap.RecentAudit {
		e := &snap.RecentAudit[i]
		if len(terms) == 0 || dashMatchAll(strings.ToLower(e.Action+" "+e.Target+" "+e.Operator+" "+e.Details), terms) {
			v.audit = append(v.audit, i)
		}
	}
	return v
}

var (
	alertSevFilters   = [...]string{"", string(fleetalerts.SeverityCritical), string(fleetalerts.SeverityWarning), string(fleetalerts.SeverityInfo)}
	alertSevLabels    = [...]string{"all", "critical", "warning", "info"}
	alertStateFilters = [...]string{"", "open", "acked", "suppressed"}
	alertStateLabels  = [...]string{"all", "open", "acked", "suppressed"}
)

func dashAlertTime(a fleetalerts.Alert) time.Time {
	if !a.UpdatedAt.IsZero() {
		return a.UpdatedAt
	}
	return a.CreatedAt
}

func dashCmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func dashCompareServers(snap *core.DashboardSnapshot, a, b *dashSrvRow, col serverSortCol) int {
	sa, sb := &snap.Servers[a.idx], &snap.Servers[b.idx]
	switch col {
	case ssStatus:
		// Worst first in ascending order: offline, degraded, online.
		return int(b.status) - int(a.status)
	case ssCPU:
		return dashCmpFloat(dashMetricOrNeg(a, sa.Metrics.CPUPercent), dashMetricOrNeg(b, sb.Metrics.CPUPercent))
	case ssMem:
		return dashCmpFloat(dashMetricOrNeg(a, sa.Metrics.MemoryPercent), dashMetricOrNeg(b, sb.Metrics.MemoryPercent))
	case ssDisk:
		return dashCmpFloat(dashMetricOrNeg(a, sa.Metrics.DiskPercent), dashMetricOrNeg(b, sb.Metrics.DiskPercent))
	case ssLoad:
		return dashCmpFloat(dashMetricOrNeg(a, sa.Metrics.Load1), dashMetricOrNeg(b, sb.Metrics.Load1))
	case ssSeen:
		// Ascending = most recently seen first.
		return -dashCmpTime(sa.Observed.LastSeen, sb.Observed.LastSeen)
	case ssAgent:
		return semver.Compare(a.agentSem, b.agentSem)
	case ssMode:
		return strings.Compare(string(sa.Mode), string(sb.Mode))
	default:
		return strings.Compare(sa.Name, sb.Name)
	}
}

func dashMetricOrNeg(r *dashSrvRow, v float64) float64 {
	if !r.hasMetrics {
		return -1
	}
	return v
}

func dashCmpTime(a, b time.Time) int {
	switch {
	case a.Before(b):
		return -1
	case a.After(b):
		return 1
	}
	return 0
}

// defaultSortDesc is the natural direction when a column is first selected:
// utilisation and load read best hottest-first.
func dashDefaultDesc(col serverSortCol) bool {
	switch col {
	case ssCPU, ssMem, ssDisk, ssLoad, ssAgent:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Small formatting helpers
// ---------------------------------------------------------------------------

// shortAge renders a compact age: 12s, 5m, 3h, 2d.
func dashAge(now, ts time.Time) string {
	if ts.IsZero() {
		return "-"
	}
	d := now.Sub(ts)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}

func dashBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return strconv.FormatUint(b, 10) + "B"
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit && exp < 5; n /= unit {
		div *= unit
		exp++
	}
	v := float64(b) / float64(div)
	prec := 1
	if v >= 100 {
		prec = 0
	}
	return strconv.FormatFloat(v, 'f', prec, 64) + string("KMGTPE"[exp])
}

func dashUptime(sec uint64) string {
	switch {
	case sec == 0:
		return "-"
	case sec < 3600:
		return strconv.FormatUint(sec/60, 10) + "m"
	case sec < 48*3600:
		return strconv.FormatUint(sec/3600, 10) + "h"
	default:
		return strconv.FormatUint(sec/86400, 10) + "d"
	}
}

func dashAuditKey(e logs.AuditEntry) string {
	return strconv.FormatInt(e.Timestamp.UnixNano(), 36) + "\x00" + e.Action + "\x00" + e.Target
}
