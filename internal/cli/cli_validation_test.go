// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
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
