// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

func TestDashboardSnapshotSummarizesFleetState(t *testing.T) {
	t.Parallel()

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
	defer app.Close()

	if err := app.AddServer(ServerRecord{
		Name:    "web-01",
		Address: "10.0.0.1",
		Mode:    transport.ModeDirect,
		Observed: ServerObservation{
			Reachable: true,
			LastSeen:  time.Now().Add(-2 * time.Minute).UTC(),
			OS:        "linux",
			Arch:      "amd64",
		},
		Metrics: proto.MetricsSnapshot{
			CPUPercent:    31.2,
			MemoryPercent: 44.8,
			DiskPercent:   61.1,
		},
		Services: []ServiceRecord{
			{Name: "nginx.service", LogPath: "/var/log/nginx/access.log"},
		},
	}); err != nil {
		t.Fatalf("AddServer(web-01) error = %v", err)
	}
	if err := app.AddServer(ServerRecord{
		Name:    "edge-01",
		Address: "10.0.0.2",
		Mode:    transport.ModeReverse,
		Observed: ServerObservation{
			Reachable: false,
			LastError: "dial timeout",
		},
	}); err != nil {
		t.Fatalf("AddServer(edge-01) error = %v", err)
	}

	if err := app.Alerts.Save(alerts.Alert{
		ID:       "critical-1",
		Server:   "edge-01",
		Severity: alerts.SeverityCritical,
		Message:  "edge-01 is unreachable",
	}); err != nil {
		t.Fatalf("Alerts.Save(critical) error = %v", err)
	}
	if err := app.Alerts.Save(alerts.Alert{
		ID:       "warning-1",
		Server:   "web-01",
		Severity: alerts.SeverityWarning,
		Message:  "CPU usage is elevated on web-01",
	}); err != nil {
		t.Fatalf("Alerts.Save(warning) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "templates", "web.toml"), []byte("name = \"web\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(template) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "data", "update-rollback.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(rollback state) error = %v", err)
	}
	if err := app.aggregatedLogs().Append("web-01", "nginx.service", []proto.LogLine{
		{Number: 1, Text: "boot complete"},
		{Number: 2, Text: "GET /health 200"},
	}); err != nil {
		t.Fatalf("aggregatedLogs().Append() error = %v", err)
	}

	snapshot, err := app.DashboardSnapshot()
	if err != nil {
		t.Fatalf("DashboardSnapshot() error = %v", err)
	}
	if snapshot.Status.ServerCount != 2 {
		t.Fatalf("expected 2 servers in status, got %d", snapshot.Status.ServerCount)
	}
	if snapshot.Summary.OnlineServers != 1 || snapshot.Summary.OfflineServers != 1 {
		t.Fatalf("unexpected server summary: %#v", snapshot.Summary)
	}
	if snapshot.Summary.CriticalAlerts != 1 || snapshot.Summary.WarningAlerts != 1 {
		t.Fatalf("unexpected alert summary: %#v", snapshot.Summary)
	}
	if len(snapshot.Servers) != 2 {
		t.Fatalf("expected 2 servers in snapshot, got %d", len(snapshot.Servers))
	}
	if len(snapshot.RecentAlerts) != 2 {
		t.Fatalf("expected 2 recent alerts, got %d", len(snapshot.RecentAlerts))
	}
	if len(snapshot.RecentAudit) == 0 {
		t.Fatalf("expected recent audit entries to be present")
	}
	if len(snapshot.Templates) != 1 || snapshot.Templates[0] != "web.toml" {
		t.Fatalf("unexpected templates: %#v", snapshot.Templates)
	}
	if len(snapshot.CachedLogs) != 1 {
		t.Fatalf("expected 1 cached log preview, got %d", len(snapshot.CachedLogs))
	}
	if !snapshot.CachedLogs[0].Available || snapshot.CachedLogs[0].Service != "nginx.service" {
		t.Fatalf("unexpected cached log preview: %#v", snapshot.CachedLogs)
	}
	if !snapshot.RollbackAvailable {
		t.Fatalf("expected rollback availability to be true")
	}
}
