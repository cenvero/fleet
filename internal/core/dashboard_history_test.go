// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

func newDashboardTestApp(t *testing.T) *App {
	t.Helper()
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeDirect,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
}

// Regression: the summary totals used to be computed AFTER the alert list was
// truncated to the 8 most recent alerts, so a fleet with 20 critical alerts
// reported at most 8.
func TestDashboardSnapshotAlertTotalsCountEveryAlert(t *testing.T) {
	t.Parallel()
	app := newDashboardTestApp(t)

	now := time.Now().UTC()
	for i := 0; i < 12; i++ {
		if err := app.Alerts.Save(alerts.Alert{ID: fmt.Sprintf("crit-%02d", i), Server: "web", Severity: alerts.SeverityCritical,
			Message: "down", UpdatedAt: now.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		if err := app.Alerts.Save(alerts.Alert{ID: fmt.Sprintf("warn-%02d", i), Severity: alerts.SeverityWarning, Message: "hot",
			UpdatedAt: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Alerts.Save(alerts.Alert{ID: "info-1", Severity: alerts.SeverityInfo, Message: "fyi", UpdatedAt: now.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := app.AckAlert("crit-00"); err != nil {
		t.Fatal(err)
	}
	if err := app.SuppressAlert("warn-00", time.Hour); err != nil {
		t.Fatal(err)
	}

	snapshot, err := app.DashboardSnapshot()
	if err != nil {
		t.Fatalf("DashboardSnapshot() error = %v", err)
	}
	if got := len(snapshot.RecentAlerts); got != DefaultDashboardRecentAlerts {
		t.Fatalf("RecentAlerts = %d, want the %d most recent", got, DefaultDashboardRecentAlerts)
	}
	want := DashboardSummary{CriticalAlerts: 12, WarningAlerts: 5, InfoAlerts: 1, MonitoredAlerts: 18}
	if snapshot.Summary != want {
		t.Fatalf("summary = %+v, want %+v", snapshot.Summary, want)
	}

	data, err := app.DashboardData(DashboardOptions{RecentAlerts: 3})
	if err != nil {
		t.Fatalf("DashboardData() error = %v", err)
	}
	if len(data.RecentAlerts) != 3 || len(data.Alerts) != 18 {
		t.Fatalf("recent=%d all=%d, want 3 and 18", len(data.RecentAlerts), len(data.Alerts))
	}
	stats := data.AlertStats
	if stats.Total != 18 || stats.Critical != 12 || stats.Warning != 5 || stats.Info != 1 {
		t.Fatalf("severity stats = %+v", stats)
	}
	if stats.Acknowledged != 1 || stats.Suppressed != 1 || stats.Open != 16 {
		t.Fatalf("state stats = %+v", stats)
	}
	if stats.OpenCritical != 11 || stats.OpenWarning != 4 || stats.OpenInfo != 1 {
		t.Fatalf("open stats = %+v", stats)
	}
}

func TestComputeAlertStatsStates(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Minute), now.Add(time.Minute)
	list := []alerts.Alert{
		{ID: "a", Severity: alerts.SeverityCritical},
		{ID: "b", Severity: alerts.SeverityCritical, AcknowledgedAt: &past},
		{ID: "c", Severity: alerts.SeverityWarning, SuppressedUntil: &future},
		// An expired suppression window no longer hides the alert.
		{ID: "d", Severity: alerts.SeverityWarning, SuppressedUntil: &past},
		// Suppression wins over acknowledgement while it is running.
		{ID: "e", Severity: alerts.SeverityInfo, AcknowledgedAt: &past, SuppressedUntil: &future},
	}
	got := ComputeAlertStats(list, now)
	want := AlertStats{Total: 5, Critical: 2, Warning: 2, Info: 1, Open: 2, Acknowledged: 1, Suppressed: 2, OpenCritical: 1, OpenWarning: 1}
	if got != want {
		t.Fatalf("ComputeAlertStats() = %+v, want %+v", got, want)
	}
	if s := AlertState(list[3], now); s != "open" {
		t.Fatalf("expired suppression state = %q, want open", s)
	}
}

// dashboardStatus replaces a second ListServers pass; it must stay identical to
// Status() so the dashboard never shows different controller facts.
func TestDashboardStatusMatchesStatus(t *testing.T) {
	t.Parallel()
	app := newDashboardTestApp(t)
	for _, name := range []string{"a-01", "b-01", "c-01"} {
		if err := app.AddServer(ServerRecord{Name: name, Address: "10.0.0.1"}); err != nil {
			t.Fatal(err)
		}
	}
	want, err := app.Status()
	if err != nil {
		t.Fatal(err)
	}
	got, err := app.dashboardStatus(3)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dashboardStatus() = %+v\nStatus() = %+v", got, want)
	}
	data, err := app.DashboardData(DashboardOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(data.Status, want) {
		t.Fatalf("DashboardData().Status = %+v, want %+v", data.Status, want)
	}
}

func TestDashboardDataCarriesTagsAndAuditLimit(t *testing.T) {
	t.Parallel()
	app := newDashboardTestApp(t)
	if err := app.AddServer(ServerRecord{Name: "web-01", Address: "10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if err := NewTagStore(app.ConfigDir).SetTags("web-01", map[string]string{"role": "web"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if err := app.AddServer(ServerRecord{Name: fmt.Sprintf("n-%02d", i), Address: "10.0.0.2"}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := app.DashboardData(DashboardOptions{RecentAudit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if data.Tags["web-01"]["role"] != "web" {
		t.Fatalf("tags = %#v", data.Tags)
	}
	if len(data.RecentAudit) != 20 {
		t.Fatalf("RecentAudit = %d, want 20", len(data.RecentAudit))
	}
	if data.RecentAudit[0].Target != "n-29" {
		t.Fatalf("newest audit first: got %q", data.RecentAudit[0].Target)
	}
	if len(data.Servers) != 31 || data.Status.ServerCount != 31 {
		t.Fatalf("servers=%d count=%d", len(data.Servers), data.Status.ServerCount)
	}
}

func TestRecentMetricHistoryIsBoundedAndOldestFirst(t *testing.T) {
	t.Parallel()
	app := newDashboardTestApp(t)
	base := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 40; i++ {
		for _, server := range []string{"web-01", "db-01"} {
			snap := proto.MetricsSnapshot{Timestamp: base.Add(time.Duration(i) * time.Minute), CPUPercent: float64(i), MemoryPercent: 50, DiskPercent: 70, Load1: 1.5}
			if server == "db-01" {
				snap.CPUPercent = 99
			}
			payload, _ := json.Marshal(snap)
			if err := app.MetricsDB.AppendMetricSnapshot(server, snap.Timestamp, string(payload)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A corrupt row is skipped, not fatal.
	if err := app.MetricsDB.AppendMetricSnapshot("web-01", base.Add(-time.Hour), "{not json"); err != nil {
		t.Fatal(err)
	}

	points, err := app.RecentMetricHistory("web-01", 10)
	if err != nil {
		t.Fatalf("RecentMetricHistory() error = %v", err)
	}
	if len(points) != 10 {
		t.Fatalf("points = %d, want 10", len(points))
	}
	for i, p := range points {
		if want := float64(30 + i); p.CPU != want {
			t.Fatalf("point %d cpu = %v, want %v (oldest first, only web-01)", i, p.CPU, want)
		}
	}
	if !points[9].Timestamp.Equal(base.Add(39 * time.Minute)) {
		t.Fatalf("newest point at %v", points[9].Timestamp)
	}

	all, err := app.RecentMetricHistory("web-01", 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 40 { // 41 rows, the corrupt one skipped
		t.Fatalf("all points = %d, want 40", len(all))
	}
	if _, err := app.RecentMetricHistory("", 10); err == nil {
		t.Fatalf("expected an error for an empty server name (it would scan every server)")
	}
	none, err := app.RecentMetricHistory("missing", 10)
	if err != nil || len(none) != 0 {
		t.Fatalf("unknown server: points=%d err=%v", len(none), err)
	}
}

func TestDashboardLogTailReadsCacheWithoutAuditing(t *testing.T) {
	t.Parallel()
	app := newDashboardTestApp(t)
	if err := app.AddServer(ServerRecord{Name: "web-01", Address: "10.0.0.1",
		Services: []ServiceRecord{{Name: "nginx.service", LogPath: "/var/log/nginx/access.log"}}}); err != nil {
		t.Fatal(err)
	}
	lines := make([]proto.LogLine, 0, 50)
	for i := 1; i <= 50; i++ {
		lines = append(lines, proto.LogLine{Number: i, Text: fmt.Sprintf("line %d", i)})
	}
	if err := app.aggregatedLogs().Append("web-01", "nginx.service", lines); err != nil {
		t.Fatal(err)
	}
	before, err := app.AuditEntries()
	if err != nil {
		t.Fatal(err)
	}
	tail, err := app.DashboardLogTail("web-01", "nginx.service", 20)
	if err != nil {
		t.Fatalf("DashboardLogTail() error = %v", err)
	}
	if len(tail.Lines) != 20 || tail.Lines[19].Text != "line 50" || !tail.Truncated || !tail.Available {
		t.Fatalf("unexpected tail: %d lines, last=%q truncated=%v", len(tail.Lines), tail.Lines[len(tail.Lines)-1].Text, tail.Truncated)
	}
	after, err := app.AuditEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("DashboardLogTail wrote %d audit entries; a passive dashboard read must not", len(after)-len(before))
	}
	if _, err := app.DashboardLogTail("web-01", "untracked.service", 10); err == nil {
		t.Fatalf("expected an error for an untracked service")
	}
}
