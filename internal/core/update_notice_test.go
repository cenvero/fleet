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

	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/internal/version"
)

// seedUpdateCache writes a fresh (non-stale) update-available cache so
// UpdateAvailable returns its value without a network fetch.
func seedUpdateCache(t *testing.T, dir, latest string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(homebrewHintCache{CheckedAt: time.Now().UTC(), Latest: latest})
	if err := os.WriteFile(filepath.Join(dir, "data", "update-available.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateAvailableAndNotice(t *testing.T) {
	orig := version.Version
	version.Version = "2.0.0"
	defer func() { version.Version = orig }()

	dir := t.TempDir()
	seedUpdateCache(t, dir, "2.1.0")
	const manifestURL = "https://example.invalid/manifest.json"

	// A newer cached version on the configured channel is reported.
	if got := UpdateAvailable(dir, manifestURL, "stable", update.PolicyNotifyOnly); got != "2.1.0" {
		t.Fatalf("UpdateAvailable = %q, want 2.1.0", got)
	}
	// Disabled policy suppresses the check entirely.
	if got := UpdateAvailable(dir, manifestURL, "stable", update.PolicyDisabled); got != "" {
		t.Fatalf("disabled policy should suppress: %q", got)
	}

	// The notice carries the version + the correct upgrade command for this build.
	notice := UpdateNotice(dir, manifestURL, "stable", update.PolicyNotifyOnly, false)
	if !strings.Contains(notice, "2.1.0") || !strings.Contains(notice, UpgradeCommand()) {
		t.Fatalf("notice missing version/command: %q", notice)
	}
	if strings.Contains(notice, ansiYellow) {
		t.Fatalf("color=false must not include ANSI: %q", notice)
	}
	if c := UpdateNotice(dir, manifestURL, "stable", update.PolicyNotifyOnly, true); !strings.HasPrefix(c, ansiYellow) || !strings.HasSuffix(c, ansiReset) {
		t.Fatalf("color=true must wrap in yellow: %q", c)
	}

	// Up to date → no notice.
	version.Version = "2.1.0"
	if got := UpdateNotice(dir, manifestURL, "stable", update.PolicyNotifyOnly, false); got != "" {
		t.Fatalf("same version should produce no notice: %q", got)
	}
	// A dev build is never 'out of date'.
	version.Version = "dev"
	if got := UpdateAvailable(dir, manifestURL, "stable", update.PolicyNotifyOnly); got != "" {
		t.Fatalf("dev build should never report an update: %q", got)
	}
}

func TestUpgradeCommandNonEmpty(t *testing.T) {
	if cmd := UpgradeCommand(); cmd == "" || !(strings.Contains(cmd, "brew") || strings.Contains(cmd, "fleet update")) {
		t.Fatalf("unexpected upgrade command: %q", cmd)
	}
}
