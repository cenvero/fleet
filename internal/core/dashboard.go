// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/cenvero/fleet/internal/alerts"
)

func (a *App) DashboardSnapshot() (DashboardSnapshot, error) {
	status, err := a.Status()
	if err != nil {
		return DashboardSnapshot{}, err
	}

	servers, err := a.ListServers()
	if err != nil {
		return DashboardSnapshot{}, err
	}

	recentAlerts, err := a.ListAlerts("", "")
	if err != nil {
		return DashboardSnapshot{}, err
	}
	if len(recentAlerts) > 8 {
		recentAlerts = recentAlerts[:8]
	}
	recentAudit, err := a.AuditEntries()
	if err != nil {
		return DashboardSnapshot{}, err
	}
	if len(recentAudit) > 8 {
		recentAudit = recentAudit[:8]
	}
	templates, err := a.ListTemplates()
	if err != nil {
		return DashboardSnapshot{}, err
	}
	cachedLogs, err := a.cachedLogPreviews(servers, 12)
	if err != nil {
		return DashboardSnapshot{}, err
	}

	summary := DashboardSummary{
		MonitoredAlerts: len(recentAlerts),
	}
	for _, server := range servers {
		if server.Observed.Reachable {
			summary.OnlineServers++
			continue
		}
		summary.OfflineServers++
	}
	for _, alert := range recentAlerts {
		switch alert.Severity {
		case alerts.SeverityCritical:
			summary.CriticalAlerts++
		case alerts.SeverityWarning:
			summary.WarningAlerts++
		case alerts.SeverityInfo:
			summary.InfoAlerts++
		}
	}

	return DashboardSnapshot{
		Status:            status,
		Summary:           summary,
		Servers:           servers,
		CachedLogs:        cachedLogs,
		RecentAlerts:      recentAlerts,
		RecentAudit:       recentAudit,
		Templates:         templates,
		RollbackAvailable: rollbackAvailable(a.ConfigDir),
		GeneratedAt:       time.Now().UTC(),
	}, nil
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
