// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"net/http"
	"sort"
	"time"

	"github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/core"
)

// The Fleet overview is strictly read-only: it is served from GET endpoints
// that only call core read APIs (ListServers, ListAlerts, the tag store), and
// it deliberately projects server records onto a display-only shape so no
// credential-bearing field (key paths, enrollment secrets, agent login keys)
// can ever be serialized to the browser.

type overviewMetrics struct {
	At            time.Time `json:"at,omitzero"`
	CPUPercent    float64   `json:"cpu_percent"`
	MemoryPercent float64   `json:"memory_percent"`
	DiskPercent   float64   `json:"disk_percent"`
	MemoryUsed    uint64    `json:"memory_used_bytes,omitempty"`
	MemoryTotal   uint64    `json:"memory_total_bytes,omitempty"`
	DiskUsed      uint64    `json:"disk_used_bytes,omitempty"`
	DiskTotal     uint64    `json:"disk_total_bytes,omitempty"`
	Load1         float64   `json:"load1,omitempty"`
	UptimeSeconds uint64    `json:"uptime_seconds,omitempty"`
}

type overviewAlertCounts struct {
	Critical int `json:"critical"`
	Warning  int `json:"warning"`
	Info     int `json:"info"`
}

type overviewServer struct {
	Name         string              `json:"name"`
	Status       string              `json:"status"`
	Reachable    bool                `json:"reachable"`
	Mode         string              `json:"mode"`
	Address      string              `json:"address,omitempty"`
	OS           string              `json:"os,omitempty"`
	Arch         string              `json:"arch,omitempty"`
	Node         string              `json:"node,omitempty"`
	AgentVersion string              `json:"agent_version,omitempty"`
	LastSeen     time.Time           `json:"last_seen,omitzero"`
	LastError    string              `json:"last_error,omitempty"`
	Metrics      *overviewMetrics    `json:"metrics,omitempty"`
	Tags         map[string]string   `json:"tags,omitempty"`
	Alerts       overviewAlertCounts `json:"alerts"`
}

type overviewAlert struct {
	ID          string    `json:"id"`
	Server      string    `json:"server,omitempty"`
	Severity    string    `json:"severity"`
	Code        string    `json:"code,omitempty"`
	Message     string    `json:"message"`
	CreatedAt   time.Time `json:"created_at,omitzero"`
	UpdatedAt   time.Time `json:"updated_at,omitzero"`
	Occurrences int       `json:"occurrences,omitempty"`
}

type overviewPayload struct {
	GeneratedAt      time.Time        `json:"generated_at"`
	Summary          overviewSummary  `json:"summary"`
	Servers          []overviewServer `json:"servers"`
	Alerts           []overviewAlert  `json:"alerts"`
	AlertsRestricted bool             `json:"alerts_restricted,omitempty"`
	TagsRestricted   bool             `json:"tags_restricted,omitempty"`
}

type overviewSummary struct {
	Total   int                 `json:"total"`
	Online  int                 `json:"online"`
	Offline int                 `json:"offline"`
	Other   int                 `json:"other"`
	Alerts  overviewAlertCounts `json:"alerts"`
}

// serverStatus mirrors the CLI's `server list` STATUS column.
func serverStatus(server core.ServerRecord) string {
	switch {
	case server.Observed.Reachable:
		return "online"
	case server.Agent.Status == "failed":
		return "install-failed"
	case server.Agent.Status == "reconcile-required":
		return "reconcile-required"
	case !server.Observed.LastSeen.IsZero() || server.Observed.LastError != "":
		return "offline"
	default:
		return "pending"
	}
}

// alertOpen reports whether an alert still needs attention: not acknowledged
// and not currently suppressed.
func alertOpen(a alerts.Alert, now time.Time) bool {
	if a.AcknowledgedAt != nil {
		return false
	}
	if a.SuppressedUntil != nil && a.SuppressedUntil.After(now) {
		return false
	}
	return true
}

func (s *Server) buildOverview() (overviewPayload, int, error) {
	// RBAC: the overview is what `fleet server list` + `fleet alerts` + `fleet
	// tag` would show. When the UI was launched under a scoped token, the same
	// read checks apply; the server list is mandatory, the rest degrade.
	if err := s.authorize("server"); err != nil {
		return overviewPayload{}, http.StatusForbidden, err
	}
	servers, err := s.app.ListServers()
	if err != nil {
		return overviewPayload{}, http.StatusBadGateway, err
	}
	now := time.Now().UTC()
	out := overviewPayload{GeneratedAt: now, Servers: make([]overviewServer, 0, len(servers)), Alerts: []overviewAlert{}}

	var tags map[string]map[string]string
	if s.authorize("tag") == nil {
		tags = core.NewTagStore(s.app.ConfigDir).AllTags()
	} else {
		out.TagsRestricted = true
	}

	perServer := map[string]*overviewAlertCounts{}
	if s.authorize("alerts") == nil {
		list, err := s.app.ListAlerts("", "")
		if err != nil {
			return overviewPayload{}, http.StatusBadGateway, err
		}
		for _, a := range list {
			if !alertOpen(a, now) {
				continue
			}
			out.Alerts = append(out.Alerts, overviewAlert{
				ID: a.ID, Server: a.Server, Severity: string(a.Severity), Code: a.Code, Message: a.Message,
				CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt, Occurrences: a.Occurrences,
			})
			c := perServer[a.Server]
			if c == nil {
				c = &overviewAlertCounts{}
				perServer[a.Server] = c
			}
			switch a.Severity {
			case alerts.SeverityCritical:
				c.Critical++
				out.Summary.Alerts.Critical++
			case alerts.SeverityWarning:
				c.Warning++
				out.Summary.Alerts.Warning++
			default:
				c.Info++
				out.Summary.Alerts.Info++
			}
		}
	} else {
		out.AlertsRestricted = true
	}

	for _, srv := range servers {
		row := overviewServer{
			Name:         srv.Name,
			Status:       serverStatus(srv),
			Reachable:    srv.Observed.Reachable,
			Mode:         string(srv.Mode),
			Address:      srv.Address,
			OS:           srv.Observed.OS,
			Arch:         srv.Observed.Arch,
			Node:         srv.Observed.NodeName,
			AgentVersion: srv.Observed.AgentVersion,
			LastSeen:     srv.Observed.LastSeen,
			LastError:    srv.Observed.LastError,
			Tags:         tags[srv.Name],
		}
		if m := srv.Metrics; !m.Timestamp.IsZero() {
			row.Metrics = &overviewMetrics{
				At: m.Timestamp, CPUPercent: m.CPUPercent, MemoryPercent: m.MemoryPercent, DiskPercent: m.DiskPercent,
				MemoryUsed: m.MemoryUsedBytes, MemoryTotal: m.MemoryTotalBytes, DiskUsed: m.DiskUsedBytes, DiskTotal: m.DiskTotalBytes,
				Load1: m.Load1, UptimeSeconds: m.UptimeSeconds,
			}
		}
		if c := perServer[srv.Name]; c != nil {
			row.Alerts = *c
		}
		switch row.Status {
		case "online":
			out.Summary.Online++
		case "offline":
			out.Summary.Offline++
		default:
			out.Summary.Other++
		}
		out.Servers = append(out.Servers, row)
	}
	out.Summary.Total = len(out.Servers)
	sort.SliceStable(out.Servers, func(i, j int) bool { return out.Servers[i].Name < out.Servers[j].Name })
	return out, http.StatusOK, nil
}

// handleOverview serves the read-only Fleet overview (GET only).
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	payload, status, err := s.buildOverview()
	if err != nil {
		writeJSONStatus(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, payload)
}

// authorize applies the launching RBAC token (if any) to a read the web UI is
// about to perform on the operator's behalf. Without a token it allows
// everything, exactly like an unscoped CLI invocation.
func (s *Server) authorize(command string) error {
	if s.authorizer == nil {
		return nil
	}
	return s.authorizer(command)
}
