// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleetcrypto "github.com/cenvero/fleet/internal/crypto"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
)

func noticeIDs(notices []Notice) map[string]Notice {
	out := map[string]Notice{}
	for _, n := range notices {
		out[n.ID] = n
	}
	return out
}

// Notices are derived from the controller's own state: a fresh controller has
// none, each cause produces one with the command that fixes it, and fixing
// the cause makes it go away.
func TestCollectNoticesFollowsTheControllerState(t *testing.T) {
	app := newImportTestApp(t)
	dir := app.ConfigDir
	// Keep the update check offline: no notice comes from the network here.
	cfg := app.Config
	cfg.Updates.Policy = update.PolicyDisabled
	if err := SaveConfig(ConfigPath(dir), cfg); err != nil {
		t.Fatal(err)
	}
	if got := CollectNotices(dir); len(got) != 0 {
		t.Fatalf("fresh controller has notices: %+v", got)
	}

	cfg.Crypto.PassphraseProtected = true
	cfg.InitVersion = CurrentInitVersion - 1
	keyPath := filepath.Join(dir, "keys", cfg.Crypto.PrimaryKey)
	plainKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	encDir := t.TempDir()
	if err := fleetcrypto.GenerateKeySet(encDir, fleetcrypto.Algorithm(cfg.Crypto.Algorithm), []byte("secret")); err != nil {
		t.Fatal(err)
	}
	encKey, err := os.ReadFile(filepath.Join(encDir, cfg.Crypto.PrimaryKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, encKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(ConfigPath(dir), cfg); err != nil {
		t.Fatal(err)
	}
	if err := writeAgentSyncState(dir, AgentSyncState{LastFailed: 1, LastFailedNames: []string{"web-01\x1b[31m"}}); err != nil {
		t.Fatal(err)
	}
	if err := app.AddServer(ServerRecord{Name: "edge-01", Address: "unknown", Mode: transport.ModeReverse}); err != nil {
		t.Fatal(err)
	}

	got := noticeIDs(CollectNotices(dir))
	for id, fix := range map[string]string{
		"passphrase-keys":   "ssh-keygen -p -N '' -f " + keyPath,
		"adjust-init":       "fleet adjust-init",
		"agent-sync-failed": "fleet sync-agent auto status",
		"daemon-needed":     "fleet start",
	} {
		n, ok := got[id]
		if !ok {
			t.Fatalf("missing notice %q (have %v)", id, got)
		}
		if n.Fix != fix {
			t.Fatalf("notice %q fix = %q, want %q", id, n.Fix, fix)
		}
	}
	// Server names come from records an agent influenced: control characters
	// must not reach the terminal.
	if d := got["agent-sync-failed"].Detail; containsControl(d) {
		t.Fatalf("notice detail carries control characters: %q", d)
	}

	// Fixing the causes clears them. Removing the passphrase from the key is
	// enough on its own, even though config.toml still records one.
	if err := os.WriteFile(keyPath, plainKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if n := noticeIDs(CollectNotices(dir)); n["passphrase-keys"].ID != "" {
		t.Fatal("passphrase notice remains after the passphrase was removed from the key")
	}
	cfg.InitVersion = CurrentInitVersion
	if err := SaveConfig(ConfigPath(dir), cfg); err != nil {
		t.Fatal(err)
	}
	if err := writeAgentSyncState(dir, AgentSyncState{}); err != nil {
		t.Fatal(err)
	}
	if err := app.RemoveServerWithOptions("edge-01", RemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if left := CollectNotices(dir); len(left) != 0 {
		t.Fatalf("notices left after fixing every cause: %+v", left)
	}
}

func containsControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// Problems the background helper records reach `fleet errors`: a start that
// failed (until one succeeds) and a task whose last run failed.
func TestCollectNoticesReportsHelperProblems(t *testing.T) {
	app := newImportTestApp(t)
	dir := app.ConfigDir
	cfg := app.Config
	cfg.Updates.Policy = update.PolicyDisabled
	if err := SaveConfig(ConfigPath(dir), cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	status := HelperStatus{
		Format: 1, StartedAt: now.Add(-time.Hour),
		LastLaunchError: "exec format error\x1b[2J", LastLaunchErrorAt: now,
		Tasks: map[string]HelperTaskStatus{
			"jobs":       {Interval: "5s", LastResult: HelperResultError, LastError: "jobs.json is damaged"},
			"agent-sync": {Interval: "5m", LastResult: HelperResultOK},
		},
	}
	data, _ := json.Marshal(status)
	if err := os.WriteFile(HelperStatusPath(dir), data, 0o600); err != nil {
		t.Fatal(err)
	}
	got := noticeIDs(CollectNotices(dir))
	if n, ok := got["helper-not-running"]; !ok || n.Fix != "fleet helper restart" || containsControl(n.Detail) {
		t.Fatalf("helper start failure: %+v (all: %v)", n, got)
	}
	if n, ok := got["helper-task-jobs"]; !ok || !strings.Contains(n.Detail, "damaged") {
		t.Fatalf("failed task: %+v", n)
	}
	if _, ok := got["helper-task-agent-sync"]; ok {
		t.Fatal("a task that last succeeded is reported")
	}

	// A later successful start clears the start failure.
	status.StartedAt = now.Add(time.Minute)
	status.Tasks = nil
	data, _ = json.Marshal(status)
	if err := os.WriteFile(HelperStatusPath(dir), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if left := CollectNotices(dir); len(left) != 0 {
		t.Fatalf("notices after the helper started: %+v", left)
	}
}
