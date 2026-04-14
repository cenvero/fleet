// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/core"
	tea "github.com/charmbracelet/bubbletea"
)

func TestHandleMouseSelectsTabAndRows(t *testing.T) {
	t.Parallel()

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

	layout := m.layout("")
	tabRect := layout.tabRects[int(tabAlerts)]
	if !m.handleMouse(tea.MouseMsg{
		X:      tabRect.x,
		Y:      tabRect.y,
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
	}) {
		t.Fatalf("expected tab click to be handled")
	}
	if m.activeTab != tabAlerts {
		t.Fatalf("expected active tab to switch to alerts, got %v", m.activeTab)
	}

	m.activeTab = tabServers
	layout = m.layout("")
	if !m.handleMouse(tea.MouseMsg{
		X:      layout.serverList.x,
		Y:      layout.serverList.y + 2,
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
	}) {
		t.Fatalf("expected server list click to be handled")
	}
	if m.serverIndex != 2 {
		t.Fatalf("expected server index 2, got %d", m.serverIndex)
	}

	m.activeTab = tabLogs
	layout = m.layout("")
	if !m.handleMouse(tea.MouseMsg{
		X:      layout.logList.x,
		Y:      layout.logList.y + 1,
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
	}) {
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
