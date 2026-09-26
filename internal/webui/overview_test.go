// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
)

func getOverview(t *testing.T, s *Server, base string) (int, overviewPayload, string) {
	t.Helper()
	res, err := http.Get(base + "/api/overview?t=" + s.Token())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var out overviewPayload
	_ = json.Unmarshal(body, &out)
	return res.StatusCode, out, string(body)
}

func seedOverview(t *testing.T, s *Server) {
	t.Helper()
	now := time.Now().UTC()
	if err := s.app.SaveServer(core.ServerRecord{
		Name: "web-01", Mode: transport.ModeDirect, Address: "10.0.0.5",
		KeyPath: "/very/secret/id_ed25519", EnrollSecret: "enroll-secret-value",
		Agent:    core.AgentInstall{LoginKey: "/secret/login-key"},
		Observed: core.ServerObservation{Reachable: true, OS: "linux", Arch: "amd64", LastSeen: now, AgentVersion: "2.4.3"},
		Metrics:  proto.MetricsSnapshot{Timestamp: now, CPUPercent: 42.5, MemoryPercent: 61, DiskPercent: 93},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.app.SaveServer(core.ServerRecord{
		Name: "db-01", Mode: transport.ModeReverse,
		Observed: core.ServerObservation{Reachable: false, LastSeen: now.Add(-time.Hour), LastError: "connection refused"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := core.NewTagStore(s.app.ConfigDir).SetTags("web-01", map[string]string{"role": "web", "env": "prod"}); err != nil {
		t.Fatal(err)
	}
	acked := now
	suppressed := now.Add(time.Hour)
	for _, a := range []alerts.Alert{
		{ID: "a1", Server: "web-01", Severity: alerts.SeverityCritical, Message: "disk 93%", CreatedAt: now, UpdatedAt: now},
		{ID: "a2", Server: "db-01", Severity: alerts.SeverityWarning, Message: "unreachable", CreatedAt: now, UpdatedAt: now},
		{ID: "a3", Server: "web-01", Severity: alerts.SeverityWarning, Message: "acked", CreatedAt: now, AcknowledgedAt: &acked},
		{ID: "a4", Server: "web-01", Severity: alerts.SeverityInfo, Message: "muted", CreatedAt: now, SuppressedUntil: &suppressed},
	} {
		if err := s.app.Alerts.Save(a); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOverviewListsServersAlertsAndTags(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	seedOverview(t, s)
	code, out, raw := getOverview(t, s, ts.URL)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	// Credential-bearing fields must never reach the browser.
	for _, secret := range []string{"enroll-secret-value", "/very/secret/id_ed25519", "/secret/login-key", "key_path", "enroll_secret", "login_key"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("overview leaked %q: %s", secret, raw)
		}
	}
	if out.Summary.Total != 2 || out.Summary.Online != 1 || out.Summary.Offline != 1 {
		t.Fatalf("summary = %+v", out.Summary)
	}
	if out.Summary.Alerts.Critical != 1 || out.Summary.Alerts.Warning != 1 || out.Summary.Alerts.Info != 0 {
		t.Fatalf("alert summary = %+v (acked/suppressed alerts must not count)", out.Summary.Alerts)
	}
	if len(out.Alerts) != 2 {
		t.Fatalf("open alerts = %+v", out.Alerts)
	}
	byName := map[string]overviewServer{}
	for _, srv := range out.Servers {
		byName[srv.Name] = srv
	}
	web := byName["web-01"]
	if web.Status != "online" || web.Mode != "direct" || web.Metrics == nil || web.Metrics.DiskPercent != 93 || web.Tags["role"] != "web" || web.Alerts.Critical != 1 {
		t.Fatalf("web-01 = %+v", web)
	}
	db := byName["db-01"]
	if db.Status != "offline" || db.Mode != "reverse" || db.Metrics != nil || db.LastError != "connection refused" || db.Alerts.Warning != 1 {
		t.Fatalf("db-01 = %+v", db)
	}
}

func TestOverviewHonoursLaunchToken(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	seedOverview(t, s)

	s.SetCommandAuthorizer(func(command string) error {
		if command == "alerts" || command == "tag" {
			return errors.New("denied: command not in allow list")
		}
		return nil
	})
	code, out, raw := getOverview(t, s, ts.URL)
	if code != http.StatusOK || !out.AlertsRestricted || !out.TagsRestricted || len(out.Alerts) != 0 {
		t.Fatalf("restricted overview: %d %s", code, raw)
	}
	for _, srv := range out.Servers {
		if len(srv.Tags) != 0 || srv.Alerts.Critical+srv.Alerts.Warning+srv.Alerts.Info != 0 {
			t.Fatalf("restricted data leaked for %s: %+v", srv.Name, srv)
		}
	}

	s.SetCommandAuthorizer(func(string) error { return errors.New("denied: unknown or revoked token") })
	if code, _, raw := getOverview(t, s, ts.URL); code != http.StatusForbidden || !strings.Contains(raw, "revoked") {
		t.Fatalf("denied overview: %d %s", code, raw)
	}
}

func TestServerStatusMatchesCLI(t *testing.T) {
	t.Parallel()
	cases := []struct {
		rec  core.ServerRecord
		want string
	}{
		{core.ServerRecord{Observed: core.ServerObservation{Reachable: true}}, "online"},
		{core.ServerRecord{Agent: core.AgentInstall{Status: "failed"}}, "install-failed"},
		{core.ServerRecord{Agent: core.AgentInstall{Status: "reconcile-required"}}, "reconcile-required"},
		{core.ServerRecord{Observed: core.ServerObservation{LastError: "x"}}, "offline"},
		{core.ServerRecord{}, "pending"},
	}
	for _, c := range cases {
		if got := serverStatus(c.rec); got != c.want {
			t.Fatalf("serverStatus(%+v) = %q, want %q", c.rec, got, c.want)
		}
	}
}
