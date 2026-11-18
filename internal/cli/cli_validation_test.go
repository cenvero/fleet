// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/logs"
)

func runFleetIn(t *testing.T, configDir string, args ...string) (string, error) {
	t.Helper()
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"--config-dir", configDir}, args...))
	err := root.Execute()
	return out.String(), err
}

// `fleet tag <typo>` used to report "has no tags" and `fleet tag <typo> k=v`
// stored tags for a server that doesn't exist. Clearing tags a removed server
// still has keeps working.
func TestTagRejectsUnknownServer(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	for _, args := range [][]string{{"tag", "nope"}, {"tag", "nope", "env=prod"}} {
		if _, err := runFleetIn(t, configDir, args...); err == nil || !strings.Contains(err.Error(), `server "nope" not found`) {
			t.Fatalf("%v: err = %v, want server not found", args, err)
		}
	}
	if got := core.NewTagStore(configDir).GetTags("nope"); len(got) != 0 {
		t.Fatalf("tags stored for an unknown server: %v", got)
	}

	if err := core.NewTagStore(configDir).SetTags("gone", map[string]string{"env": "prod"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runFleetIn(t, configDir, "tag", "gone", "env="); err != nil {
		t.Fatalf("clearing a removed server's tags: %v", err)
	}
	if got := core.NewTagStore(configDir).GetTags("gone"); len(got) != 0 {
		t.Fatalf("tags not cleared: %v", got)
	}
}

func TestTagRejectsKeysAndValuesThatBreakFilters(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	app, err := core.Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.AddServer(core.ServerRecord{Name: "web-01", Address: "127.0.0.1", Mode: "direct"}); err != nil {
		app.Close()
		t.Fatal(err)
	}
	app.Close()
	for _, pair := range []string{"bad key=1", "a,b=1", "role=web,db"} {
		if _, err := runFleetIn(t, configDir, "tag", "web-01", pair); err == nil {
			t.Errorf("tag %q accepted", pair)
		}
	}
	if _, err := runFleetIn(t, configDir, "tag", "web-01", "role=web", "team=ops-1"); err != nil {
		t.Fatalf("valid tags: %v", err)
	}
}

func TestUpdateChannelRejectsUnknownChannel(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	if _, err := runFleetIn(t, configDir, "update", "channel", "bogus"); err == nil || !strings.Contains(err.Error(), "unknown update channel") {
		t.Fatalf("err = %v, want unknown update channel", err)
	}
	cfg, err := core.LoadConfig(core.ConfigPath(configDir))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Updates.Channel != "stable" {
		t.Fatalf("channel = %q after a rejected change, want stable", cfg.Updates.Channel)
	}
}

func TestFileDefaultsRejectNegativeParallel(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	if _, err := runFleetIn(t, configDir, "file", "defaults", "set", "--parallel", "-3"); err == nil || !strings.Contains(err.Error(), "--parallel") {
		t.Fatalf("err = %v, want --parallel validation error", err)
	}
}

// With the daemon stopped a reverse-mode server is unreachable, not an agent
// error.
func TestClassifyDaemonNotRunningAsUnreachable(t *testing.T) {
	err := errors.New("the fleet daemon is not running (reverse-mode servers are reached through it); start it with `fleet start`, or run `fleet daemon`: open control.token: no such file or directory")
	if got := classifyAgentError(err); got != "unreachable" {
		t.Fatalf("classifyAgentError = %q, want unreachable", got)
	}
}

// `fleet logs --server` / `--service` filter the audit log on their own; they
// used to apply only together with --search, so the whole log came back.
func TestLogsFiltersWithoutSearch(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	audit := logs.NewAuditLog(filepath.Join(configDir, "logs", "_audit.log"))
	for _, e := range []logs.AuditEntry{
		{Action: "service.restart", Target: "web-01", Details: "service=nginx"},
		{Action: "service.restart", Target: "db-01", Details: "service=postgres"},
	} {
		if err := audit.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ flag, value, want, unwanted string }{
		{"--server", "web-01", "web-01", "db-01"},
		{"--service", "nginx", "nginx", "postgres"},
	} {
		out, err := runFleetIn(t, configDir, "logs", c.flag, c.value)
		if err != nil {
			t.Fatalf("logs %s %s: %v", c.flag, c.value, err)
		}
		var entries []logs.AuditEntry
		if err := json.Unmarshal([]byte(out[strings.Index(out, "["):]), &entries); err != nil {
			t.Fatalf("logs %s %s output: %v\n%s", c.flag, c.value, err, out)
		}
		if len(entries) == 0 {
			t.Fatalf("logs %s %s returned nothing", c.flag, c.value)
		}
		for _, e := range entries {
			payload, _ := json.Marshal(e)
			if !strings.Contains(string(payload), c.want) || strings.Contains(string(payload), c.unwanted) {
				t.Fatalf("logs %s %s returned an unmatched entry: %s", c.flag, c.value, payload)
			}
		}
	}
}

// Notices (here a pending config migration) are no longer printed before
// ordinary commands; `fleet errors` lists them with the command that fixes them.
func TestNoticesOnlyInFleetErrors(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	cfg, err := core.LoadConfig(core.ConfigPath(configDir))
	if err != nil {
		t.Fatal(err)
	}
	cfg.InitVersion = core.CurrentInitVersion - 1
	cfg.Updates.Policy = "disabled" // keep the update check offline
	if err := core.SaveConfig(core.ConfigPath(configDir), cfg); err != nil {
		t.Fatal(err)
	}
	out, err := runFleetIn(t, configDir, "server", "list")
	if err != nil {
		t.Fatalf("server list: %v", err)
	}
	if strings.Contains(out, "adjust-init") {
		t.Fatalf("an ordinary command printed a notice:\n%s", out)
	}
	out, err = runFleetIn(t, configDir, "errors")
	if err != nil || !strings.Contains(out, "Fix: fleet adjust-init") {
		t.Fatalf("fleet errors = %v\n%s", err, out)
	}
	// Bare `fleet` lists them too, after the help.
	out, err = runFleetIn(t, configDir)
	if err != nil || !strings.Contains(out, "Fix: fleet adjust-init") || !strings.Contains(out, "Usage:") {
		t.Fatalf("bare fleet = %v\n%s", err, out)
	}
	out, err = runFleetIn(t, configDir, "errors", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var notices []core.Notice
	if err := json.Unmarshal([]byte(out), &notices); err != nil || len(notices) == 0 || notices[0].ID != "adjust-init" {
		t.Fatalf("fleet errors --json = %v %+v\n%s", err, notices, out)
	}
}
