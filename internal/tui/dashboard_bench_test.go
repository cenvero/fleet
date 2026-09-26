// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	fleetalerts "github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
	tea "github.com/charmbracelet/bubbletea"
)

// BenchmarkDashboardView measures a full (uncached) frame render at 160x45.
// Every iteration bumps the model revision, i.e. models a state change; idle
// frames are served from the cache (see BenchmarkDashboardMouseMotion).
func BenchmarkDashboardView(b *testing.B) {
	for _, tab := range []dashboardTab{tabOverview, tabServers, tabAlerts} {
		for _, n := range []int{100, 500} {
			m := newTestDash(b, n, 160, 45)
			m = dashPress(m, fmt.Sprint(int(tab)+1))
			b.Run(fmt.Sprintf("tab=%s/servers=%d", dashboardTabs[tab], n), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					m.rev++
					_ = m.View()
				}
			})
		}
	}
}

// BenchmarkDashboardMouseMotion is one pointer-motion event: Update + View.
// With cell-motion mode most motion never reaches the program at all; this
// covers drags and terminals that still report it.
func BenchmarkDashboardMouseMotion(b *testing.B) {
	for _, n := range []int{100, 500} {
		var m tea.Model = dashPress(newTestDash(b, n, 160, 45), "2")
		_ = m.View()
		b.Run(fmt.Sprintf("servers=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				m, _ = m.Update(tea.MouseMsg{X: i % 160, Y: i % 45, Action: tea.MouseActionMotion, Button: tea.MouseButtonNone})
				_ = m.View()
			}
		})
	}
}

// BenchmarkDashboardKeyNavigation is one j/k press on the Servers tab:
// Update + a fresh frame.
func BenchmarkDashboardKeyNavigation(b *testing.B) {
	for _, n := range []int{100, 500} {
		var m tea.Model = dashPress(newTestDash(b, n, 160, 45), "2")
		down, up := keyMsg("j"), keyMsg("k")
		b.Run(fmt.Sprintf("servers=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				msg := down
				if i%64 >= 32 {
					msg = up
				}
				m, _ = m.Update(msg)
				_ = m.View()
			}
		})
	}
}

// BenchmarkDashboardApplyLoad is the UI-goroutine cost of a finished refresh
// (derived rows, aggregates, sort/filter orders, selection restore).
func BenchmarkDashboardApplyLoad(b *testing.B) {
	for _, n := range []int{100, 500} {
		m := dashPress(newTestDash(b, n, 160, 45), "2")
		msg := dashboardLoadedMsg{data: testDashData(n, dashTestNow)}
		b.Run(fmt.Sprintf("servers=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = m.Update(msg)
			}
		})
	}
}

// BenchmarkDashboardRefresh is one background refresh against a real config
// dir through the long-lived loader (the old dashboard reopened the App and
// listed every server twice on each `r`).
func BenchmarkDashboardRefresh(b *testing.B) {
	if testing.Short() {
		b.Skip("builds an on-disk fleet")
	}
	for _, n := range []int{100, 500} {
		dir := benchFleetDir(b, n)
		app, err := core.Open(dir)
		if err != nil {
			b.Fatal(err)
		}
		l := newDashLoader(dir, app)
		for _, full := range []bool{false, true} {
			kind := "periodic"
			if full {
				kind = "full" // initial load / manual r: also reads every cached log preview
			}
			b.Run(fmt.Sprintf("%s/servers=%d", kind, n), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if _, err := l.load(full); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
		l.Close()
	}
}

func benchFleetDir(b *testing.B, n int) string {
	b.Helper()
	dir := filepath.Join(b.TempDir(), "fleet")
	if _, err := core.Initialize(core.InitOptions{ConfigDir: dir, Alias: "bench", DefaultMode: transport.ModeDirect,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly}); err != nil {
		b.Fatal(err)
	}
	app, err := core.Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	defer app.Close()
	cache := logs.NewServiceStore(app.Config.Runtime.AggregatedLogDir, app.Config.Runtime.AggregatedLogMaxSize, app.Config.Runtime.AggregatedLogMaxFiles, 0)
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("node-%03d", i)
		rec := core.ServerRecord{Name: name, Address: "10.0.0.1", Mode: transport.ModeDirect,
			Observed: core.ServerObservation{Reachable: i%10 != 9, LastSeen: now, NodeName: name, AgentVersion: "2.4.3", OS: "linux", Arch: "amd64"},
			Metrics:  proto.MetricsSnapshot{Timestamp: now, CPUPercent: float64(i % 100), MemoryPercent: 40, DiskPercent: 60, Load1: 1}}
		if i%2 == 0 {
			rec.Services = []core.ServiceRecord{{Name: "nginx.service", ActiveState: "active", LogPath: "/var/log/nginx/access.log"}}
		}
		if err := app.AddServer(rec); err != nil {
			b.Fatal(err)
		}
		if i%2 == 0 {
			lines := make([]proto.LogLine, 200)
			for l := range lines {
				lines[l] = proto.LogLine{Number: l + 1, Text: "GET /api/v1/items 200 12ms"}
			}
			if err := cache.Append(name, "nginx.service", lines); err != nil {
				b.Fatal(err)
			}
		}
		if i%10 == 9 {
			if err := app.Alerts.Save(fleetalerts.Alert{ID: "down-" + name, Server: name, Severity: fleetalerts.SeverityCritical, Message: name + " is unreachable"}); err != nil {
				b.Fatal(err)
			}
		}
	}
	// Steady state: server files written a while ago (a live fleet rewrites
	// each file about once per metrics poll).
	entries, err := os.ReadDir(filepath.Join(dir, "servers"))
	if err != nil {
		b.Fatal(err)
	}
	old := now.Add(-time.Minute)
	for _, e := range entries {
		if err := os.Chtimes(filepath.Join(dir, "servers", e.Name()), old, old); err != nil {
			b.Fatal(err)
		}
	}
	return dir
}
