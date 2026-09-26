// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

// MaxDashboardHistoryPoints bounds a single metric-history read so a dashboard
// can never ask the metrics store for an unbounded scan.
const MaxDashboardHistoryPoints = 512

// MaxDashboardLogTailLines bounds a single cached-log tail read.
const MaxDashboardLogTailLines = 1000

// MetricPoint is one historical metrics sample, as used for dashboard
// sparklines.
type MetricPoint struct {
	Timestamp time.Time `json:"timestamp"`
	CPU       float64   `json:"cpu_percent"`
	Memory    float64   `json:"memory_percent"`
	Disk      float64   `json:"disk_percent"`
	Load1     float64   `json:"load1,omitempty"`
}

// RecentMetricHistory returns up to limit of the most recent metric samples
// recorded for server, oldest first. It is a read-only, bounded query against
// the metric_snapshots table (WHERE server = ? ORDER BY timestamp DESC LIMIT n),
// served by the table's server/timestamp indexes. Rows whose payload cannot be
// decoded are skipped. An empty table yields an empty slice, not an error.
func (a *App) RecentMetricHistory(server string, limit int) ([]MetricPoint, error) {
	server = strings.TrimSpace(server)
	if server == "" {
		// An empty server would turn the query into a scan across every server.
		return nil, fmt.Errorf("metric history requires a server name")
	}
	if a == nil || a.MetricsDB == nil {
		return nil, nil
	}
	if limit <= 0 {
		return nil, nil
	}
	if limit > MaxDashboardHistoryPoints {
		limit = MaxDashboardHistoryPoints
	}
	entries, err := a.MetricsDB.ListMetricSnapshots(server, limit)
	if err != nil {
		return nil, err
	}
	points := make([]MetricPoint, 0, len(entries))
	// entries are newest first; emit oldest first for left-to-right charts.
	for i := len(entries) - 1; i >= 0; i-- {
		var snapshot proto.MetricsSnapshot
		if err := json.Unmarshal([]byte(entries[i].Payload), &snapshot); err != nil {
			continue
		}
		ts := snapshot.Timestamp
		if ts.IsZero() {
			ts = entries[i].Timestamp
		}
		points = append(points, MetricPoint{
			Timestamp: ts,
			CPU:       snapshot.CPUPercent,
			Memory:    snapshot.MemoryPercent,
			Disk:      snapshot.DiskPercent,
			Load1:     snapshot.Load1,
		})
	}
	return points, nil
}

// DashboardLogTail returns the last lines of the controller's aggregated cache
// for one tracked service. Like the dashboard's cached log previews it is a
// passive read of data the controller already holds: it makes no RPC and, unlike
// the operator-facing `fleet service logs --cached`, records no audit entry (a
// dashboard re-reads it on every refresh). The service must be tracked on the
// server.
func (a *App) DashboardLogTail(server, service string, lines int) (CachedLogPreview, error) {
	if _, _, err := a.trackedService(server, service); err != nil {
		return CachedLogPreview{}, err
	}
	if lines <= 0 {
		lines = DefaultDashboardLogTailLines
	}
	if lines > MaxDashboardLogTailLines {
		lines = MaxDashboardLogTailLines
	}
	result, err := a.aggregatedLogs().Read(server, service, "", lines)
	if err != nil {
		return CachedLogPreview{}, err
	}
	return CachedLogPreview{
		Server:    server,
		Service:   service,
		Path:      result.Path,
		Lines:     result.Lines,
		Truncated: result.Truncated,
		Available: len(result.Lines) > 0,
	}, nil
}
