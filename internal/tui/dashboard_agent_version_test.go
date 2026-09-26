// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/core"
)

// A development-build agent reads "dev" in the dashboard, as in `fleet server
// list` and the web UI, and is counted apart from genuinely unknown versions.
func TestDashboardShowsDevAgentVersion(t *testing.T) {
	snap := &core.DashboardSnapshot{Servers: []core.ServerRecord{
		{Name: "a", Observed: core.ServerObservation{AgentVersion: "dev"}},
		{Name: "b", Observed: core.ServerObservation{AgentVersion: ""}},
	}}
	b := dashBuildBase(snap, nil, nil, nil, time.Now())
	if got := b.rows[b.byName["a"]].agent; got != "dev" {
		t.Fatalf("dev agent shown as %q, want dev", got)
	}
	if got := b.rows[b.byName["b"]].agent; got != "-" {
		t.Fatalf("unknown agent shown as %q, want -", got)
	}
	if b.fleet.agentDev != 1 || b.fleet.agentUnknown != 1 {
		t.Fatalf("agentDev=%d agentUnknown=%d, want 1 and 1", b.fleet.agentDev, b.fleet.agentUnknown)
	}
}
