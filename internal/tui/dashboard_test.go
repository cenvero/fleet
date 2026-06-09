// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/core"
	tea "github.com/charmbracelet/bubbletea"
	zone "github.com/lrstanley/bubblezone"
)

// clickZone renders the model (populating bubblezone positions), then crafts a
// left-click at the top-left cell of the given zone and feeds it to handleMouse.
func clickZone(t *testing.T, m *model, id string) bool {
	t.Helper()
	_ = m.View() // enqueues zone positions via zone.Scan (processed async by the worker)
	var z *zone.ZoneInfo
	for range 250 {
		if z = zone.Get(id); !z.IsZero() {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if z.IsZero() {
		t.Fatalf("zone %q not found in rendered view", id)
	}
	return m.handleMouse(tea.MouseMsg{
		X:      z.StartX,
		Y:      z.StartY,
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
	})
}

func TestHandleMouseSelectsTabAndRows(t *testing.T) {
	m := model{
		width:     140,
		height:    40,
		activeTab: tabServers,
		snapshot: core.DashboardSnapshot{
			GeneratedAt: time.Now().UTC(),
			Servers: []core.ServerRecord{
				{Name: "alpha"},
				{Name: "beta"},
				{Name: "gamma"},
			},
			CachedLogs: []core.CachedLogPreview{
				{Server: "alpha", Service: "nginx.service", Available: true},
				{Server: "beta", Service: "api.service", Available: true},
			},
			RecentAlerts: []alerts.Alert{
				{ID: "a1", Message: "alpha alert"},
			},
		},
	}

	if !clickZone(t, &m, dashTabID(int(tabAlerts))) {
		t.Fatalf("expected tab click to be handled")
	}
	if m.activeTab != tabAlerts {
		t.Fatalf("expected active tab to switch to alerts, got %v", m.activeTab)
	}

	m.activeTab = tabServers
	if !clickZone(t, &m, dashRowID(int(tabServers), 2)) {
		t.Fatalf("expected server list click to be handled")
	}
	if m.serverIndex != 2 {
		t.Fatalf("expected server index 2, got %d", m.serverIndex)
	}

	m.activeTab = tabLogs
	if !clickZone(t, &m, dashRowID(int(tabLogs), 1)) {
		t.Fatalf("expected log list click to be handled")
	}
	if m.logIndex != 1 {
		t.Fatalf("expected log index 1, got %d", m.logIndex)
	}
}

func TestHandleMouseWheelMovesSelection(t *testing.T) {
	t.Parallel()

	m := model{
		activeTab: tabServers,
		snapshot: core.DashboardSnapshot{
			Servers: []core.ServerRecord{
				{Name: "alpha"},
				{Name: "beta"},
				{Name: "gamma"},
			},
		},
	}

	m.serverIndex = 1
	if !m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress}) {
		t.Fatalf("expected wheel down to be handled")
	}
	if m.serverIndex != 2 {
		t.Fatalf("expected server index 2 after wheel down, got %d", m.serverIndex)
	}
	if !m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress}) {
		t.Fatalf("expected wheel up to be handled")
	}
	if m.serverIndex != 1 {
		t.Fatalf("expected server index 1 after wheel up, got %d", m.serverIndex)
	}
}
