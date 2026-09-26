// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/crypto"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/version"
)

// Defaults for the bounded lists a dashboard snapshot carries.
const (
	DefaultDashboardRecentAlerts = 8
	DefaultDashboardRecentAudit  = 8
	DefaultDashboardLogTailLines = 12
)

// DashboardOptions bounds the per-refresh work of a dashboard snapshot. Zero
// values select the historical defaults (8 alerts, 8 audit entries, 12 cached
// log lines per tracked service).
type DashboardOptions struct {
	// RecentAlerts caps DashboardSnapshot.RecentAlerts (newest first). The
	// summary totals are always computed over EVERY alert.
	RecentAlerts int
	// RecentAudit caps DashboardSnapshot.RecentAudit (newest first).
	RecentAudit int
	// LogTailLines is how many cached lines each log preview carries.
	LogTailLines int
	// SkipLogPreviews leaves CachedLogs empty instead of reading the cached
	// log of every tracked service (each can hold megabytes across rotated
	// files); LogSources still lists them. A console that refreshes every few
	// seconds reads the one log it is showing on demand instead.
	SkipLogPreviews bool
	// ServerCache, when set, serves unchanged server files from previously
	// decoded records instead of decoding every file again (see
	// ServerListCache). The live dashboard keeps one per session.
	ServerCache *ServerListCache
}

func (o DashboardOptions) withDefaults() DashboardOptions {
	if o.RecentAlerts <= 0 {
		o.RecentAlerts = DefaultDashboardRecentAlerts
	}
	if o.RecentAudit <= 0 {
		o.RecentAudit = DefaultDashboardRecentAudit
	}
	if o.LogTailLines <= 0 {
		o.LogTailLines = DefaultDashboardLogTailLines
	}
	return o
}

// AlertStats breaks the complete alert set down by severity and by state. Every
// alert lands in exactly one state bucket: an alert whose suppression window is
// still running is Suppressed, otherwise an acknowledged alert is Acknowledged,
// otherwise it is Open.
type AlertStats struct {
	Total    int `json:"total"`
	Critical int `json:"critical"`
	Warning  int `json:"warning"`
	Info     int `json:"info"`

	Open         int `json:"open"`
	Acknowledged int `json:"acknowledged"`
	Suppressed   int `json:"suppressed"`

	OpenCritical int `json:"open_critical"`
	OpenWarning  int `json:"open_warning"`
	OpenInfo     int `json:"open_info"`
}

// AlertState classifies one alert as "suppressed", "acked" or "open" at now.
func AlertState(alert alerts.Alert, now time.Time) string {
	switch {
	case alert.SuppressedUntil != nil && alert.SuppressedUntil.After(now):
		return "suppressed"
	case alert.AcknowledgedAt != nil:
		return "acked"
	default:
		return "open"
	}
}

// ComputeAlertStats totals the given alerts. It must be fed the complete alert
// list — never a truncated "recent" slice.
func ComputeAlertStats(list []alerts.Alert, now time.Time) AlertStats {
	var stats AlertStats
	for _, alert := range list {
		stats.Total++
		open := false
		switch AlertState(alert, now) {
		case "suppressed":
			stats.Suppressed++
		case "acked":
			stats.Acknowledged++
		default:
			stats.Open++
			open = true
		}
		switch alert.Severity {
		case alerts.SeverityCritical:
			stats.Critical++
			if open {
				stats.OpenCritical++
			}
		case alerts.SeverityWarning:
			stats.Warning++
			if open {
				stats.OpenWarning++
			}
		case alerts.SeverityInfo:
			stats.Info++
			if open {
				stats.OpenInfo++
			}
		}
	}
	return stats
}

// DashboardData is the live console payload: the classic DashboardSnapshot plus
// the complete alert list (RecentAlerts is its newest prefix), alert statistics
// and the server tags, all read from ONE ListServers pass.
type DashboardData struct {
	DashboardSnapshot
	// Alerts is every alert, newest first.
	Alerts []alerts.Alert `json:"alerts"`
	// AlertStats totals Alerts by severity and state.
	AlertStats AlertStats `json:"alert_stats"`
	// Tags maps server name -> tag key -> value.
	Tags map[string]map[string]string `json:"tags"`
	// LogSources lists every tracked service with a log path, sorted by
	// server then service, whether or not previews were read.
	LogSources []DashboardLogSource `json:"log_sources"`
}

// DashboardLogSource is a tracked service whose log the controller caches.
type DashboardLogSource struct {
	Server  string `json:"server"`
	Service string `json:"service"`
	LogPath string `json:"log_path"`
}

// DashboardSnapshot returns the classic dashboard snapshot with the default
// bounds.
func (a *App) DashboardSnapshot() (DashboardSnapshot, error) {
	data, err := a.DashboardData(DashboardOptions{})
	if err != nil {
		return DashboardSnapshot{}, err
	}
	return data.DashboardSnapshot, nil
}

// DashboardData builds the live dashboard payload. The server directory is
// listed and decoded exactly once (Status() would list it a second time just to
// count servers), and the alert totals are computed over every alert before the
// recent list is truncated.
func (a *App) DashboardData(opts DashboardOptions) (DashboardData, error) {
	opts = opts.withDefaults()

	servers, err := a.listServersCached(opts.ServerCache)
	if err != nil {
		return DashboardData{}, err
	}
	status, err := a.dashboardStatus(len(servers))
	if err != nil {
		return DashboardData{}, err
	}

	allAlerts, err := a.ListAlerts("", "")
	if err != nil {
		return DashboardData{}, err
	}
	now := time.Now().UTC()
	stats := ComputeAlertStats(allAlerts, now)
	recentAlerts := allAlerts
	if len(recentAlerts) > opts.RecentAlerts {
		recentAlerts = recentAlerts[:opts.RecentAlerts]
	}

	recentAudit, err := a.recentAuditEntries(opts.RecentAudit)
	if err != nil {
		return DashboardData{}, err
	}
	templates, err := a.ListTemplates()
	if err != nil {
		return DashboardData{}, err
	}
	var cachedLogs []CachedLogPreview
	if !opts.SkipLogPreviews {
		cachedLogs, err = a.cachedLogPreviews(servers, opts.LogTailLines)
		if err != nil {
			return DashboardData{}, err
		}
	}

	summary := DashboardSummary{
		CriticalAlerts:  stats.Critical,
		WarningAlerts:   stats.Warning,
		InfoAlerts:      stats.Info,
		MonitoredAlerts: stats.Total,
	}
	for _, server := range servers {
		if server.Observed.Reachable {
			summary.OnlineServers++
			continue
		}
		summary.OfflineServers++
	}

	return DashboardData{
		DashboardSnapshot: DashboardSnapshot{
			Status:            status,
			Summary:           summary,
			Servers:           servers,
			CachedLogs:        cachedLogs,
			RecentAlerts:      recentAlerts,
			RecentAudit:       recentAudit,
			Templates:         templates,
			RollbackAvailable: rollbackAvailable(a.ConfigDir),
			GeneratedAt:       now,
		},
		Alerts:     allAlerts,
		AlertStats: stats,
		Tags:       NewTagStore(a.ConfigDir).AllTags(),
		LogSources: dashboardLogSources(servers),
	}, nil
}

func dashboardLogSources(servers []ServerRecord) []DashboardLogSource {
	out := make([]DashboardLogSource, 0, countTrackedServicesWithLogs(servers))
	for _, server := range servers {
		for _, service := range server.Services {
			if service.LogPath != "" {
				out = append(out, DashboardLogSource{Server: server.Name, Service: service.Name, LogPath: service.LogPath})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server == out[j].Server {
			return out[i].Service < out[j].Service
		}
		return out[i].Server < out[j].Server
	})
	return out
}

// dashboardStatus is Status() for a server count the caller already has, so the
// dashboard does not list and decode every server file twice per refresh.
// TestDashboardStatusMatchesStatus pins it to Status().
func (a *App) dashboardStatus(serverCount int) (Status, error) {
	fingerprints, err := crypto.Fingerprints(filepath.Join(a.ConfigDir, "keys"))
	if err != nil {
		return Status{}, err
	}
	return Status{
		Initialized:     true,
		ConfigDir:       a.ConfigDir,
		ProductName:     a.Config.ProductName,
		Version:         version.Version,
		DefaultMode:     a.Config.DefaultMode,
		Alias:           a.Config.Alias,
		ServerCount:     serverCount,
		Channel:         a.Config.Updates.Channel,
		Policy:          a.Config.Updates.Policy,
		DatabaseBackend: a.Config.Database.Backend,
		Fingerprints:    fingerprints,
	}, nil
}

// recentAuditEntries returns the newest n audit entries, newest first.
func (a *App) recentAuditEntries(n int) ([]logs.AuditEntry, error) {
	// Tail reads backwards from the end of the log, so the cost depends on n,
	// not on how large the audit log has grown.
	entries, err := a.AuditLog.Tail(n)
	if err != nil {
		return nil, err
	}
	if len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	entries = slices.Clone(entries)
	slices.Reverse(entries)
	return entries, nil
}

func rollbackAvailable(configDir string) bool {
	_, err := os.Stat(filepath.Join(configDir, "data", "update-rollback.json"))
	return err == nil
}

func (a *App) cachedLogPreviews(servers []ServerRecord, tailLines int) ([]CachedLogPreview, error) {
	previews := make([]CachedLogPreview, 0, countTrackedServicesWithLogs(servers))
	for _, server := range servers {
		for _, service := range server.Services {
			if service.LogPath == "" {
				continue
			}
			result, err := a.aggregatedLogs().Read(server.Name, service.Name, "", tailLines)
			if err != nil {
				return nil, err
			}
			previews = append(previews, CachedLogPreview{
				Server:    server.Name,
				Service:   service.Name,
				Path:      result.Path,
				Lines:     result.Lines,
				Truncated: result.Truncated,
				Available: len(result.Lines) > 0,
			})
		}
	}
	sort.Slice(previews, func(i, j int) bool {
		if previews[i].Server == previews[j].Server {
			return previews[i].Service < previews[j].Service
		}
		return previews[i].Server < previews[j].Server
	})
	return previews, nil
}

func countTrackedServicesWithLogs(servers []ServerRecord) int {
	total := 0
	for _, server := range servers {
		for _, service := range server.Services {
			if service.LogPath != "" {
				total++
			}
		}
	}
	return total
}
