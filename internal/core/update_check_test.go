// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/internal/version"
)

// httpManifestFetcher fetches a manifest from a test server with the caller's
// context — standing in for update.FetchOnce, whose public-IP policy refuses
// loopback test servers.
func httpManifestFetcher(ctx context.Context, url string) (update.Manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return update.Manifest{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return update.Manifest{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return update.Manifest{}, fmt.Errorf("status %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return update.Manifest{}, err
	}
	var m update.Manifest
	return m, json.Unmarshal(body, &m)
}

func withManifestFetchers(t *testing.T, once, retry func(context.Context, string) (update.Manifest, error)) {
	t.Helper()
	origOnce, origRetry, origVersion := fetchManifestOnce, fetchManifestRetry, version.Version
	fetchManifestOnce, fetchManifestRetry = once, retry
	version.Version = "2.0.0"
	t.Cleanup(func() {
		fetchManifestOnce, fetchManifestRetry, version.Version = origOnce, origRetry, origVersion
	})
}

func readUpdateCache(t *testing.T, dir string) homebrewHintCache {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "data", "update-available.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c homebrewHintCache
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func writeStaleUpdateCache(t *testing.T, dir, latest string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Old on-disk format (no attempted_at), last success long ago.
	data := fmt.Sprintf(`{"checked_at":%q,"latest":%q}`, time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339Nano), latest)
	if err := os.WriteFile(filepath.Join(dir, "data", "update-available.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestUpdateAvailableHangingManifestIsBoundedAndBackedOff: a manifest server that
// never answers used to cost every CLI command the full fetch timeout. Now the
// CLI path gives up within its short budget, records the failed attempt (keeping
// the last known Latest), and the next command does not try again.
func TestUpdateAvailableHangingManifestIsBoundedAndBackedOff(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	withManifestFetchers(t, httpManifestFetcher, httpManifestFetcher)

	dir := t.TempDir()
	writeStaleUpdateCache(t, dir, "2.1.0")

	start := time.Now()
	got := UpdateAvailable(dir, srv.URL, "stable", update.PolicyNotifyOnly)
	elapsed := time.Since(start)
	if got != "2.1.0" {
		t.Fatalf("UpdateAvailable = %q, want the last known 2.1.0", got)
	}
	if elapsed > cliUpdateCheckBudget+time.Second {
		t.Fatalf("hanging manifest blocked the CLI for %s (budget %s)", elapsed, cliUpdateCheckBudget)
	}
	if hits.Load() != 1 {
		t.Fatalf("manifest hits = %d, want exactly 1 (single attempt)", hits.Load())
	}
	c := readUpdateCache(t, dir)
	if c.Failures != 1 || c.AttemptedAt.IsZero() || c.LastError == "" || c.Latest != "2.1.0" {
		t.Fatalf("failed attempt not recorded correctly: %+v", c)
	}

	// The failure is cached: the next commands return instantly, no refetch.
	for i := 0; i < 3; i++ {
		start = time.Now()
		if got := UpdateAvailable(dir, srv.URL, "stable", update.PolicyNotifyOnly); got != "2.1.0" {
			t.Fatalf("UpdateAvailable = %q", got)
		}
		if d := time.Since(start); d > 200*time.Millisecond {
			t.Fatalf("command after a recorded failure still waited %s", d)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("manifest refetched within the backoff interval (hits=%d)", hits.Load())
	}
}

// TestUpdateAvailableFailingThenRecovering: a 5xx is recorded as a failure; once
// the interval has passed and the manifest answers, the cache is refreshed and
// the failure counters reset.
func TestUpdateAvailableFailingThenRecovering(t *testing.T) {
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"channels":{"stable":{"version":"2.2.0"}}}`))
	}))
	t.Cleanup(srv.Close)
	withManifestFetchers(t, httpManifestFetcher, httpManifestFetcher)

	dir := t.TempDir()
	writeStaleUpdateCache(t, dir, "")
	if got := UpdateAvailable(dir, srv.URL, "stable", update.PolicyNotifyOnly); got != "" {
		t.Fatalf("UpdateAvailable = %q, want \"\"", got)
	}
	if c := readUpdateCache(t, dir); c.Failures != 1 {
		t.Fatalf("failure not recorded: %+v", c)
	}

	// Age the attempt past the interval; the next call fetches and succeeds.
	c := readUpdateCache(t, dir)
	c.AttemptedAt = time.Now().Add(-updateCheckInterval - time.Minute)
	writeUpdateCache(filepath.Join(dir, "data", "update-available.json"), c)
	healthy.Store(true)
	if got := UpdateAvailable(dir, srv.URL, "stable", update.PolicyNotifyOnly); got != "2.2.0" {
		t.Fatalf("UpdateAvailable after recovery = %q, want 2.2.0", got)
	}
	c = readUpdateCache(t, dir)
	if c.Failures != 0 || c.LastError != "" || c.Latest != "2.2.0" || time.Since(c.CheckedAt) > time.Minute {
		t.Fatalf("successful refresh not recorded: %+v", c)
	}
}

// TestUpdateCacheCompatibleWithOldBinaries: the cache written now still decodes
// into the old two-field shape with the same meaning.
func TestUpdateCacheCompatibleWithOldBinaries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "update-available.json")
	checked := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeUpdateCache(path, homebrewHintCache{CheckedAt: checked, Latest: "3.0.0", AttemptedAt: time.Now().UTC(), Failures: 2, LastError: "x"})
	var old struct {
		CheckedAt time.Time `json:"checked_at"`
		Latest    string    `json:"latest"`
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	if !old.CheckedAt.Equal(checked) || old.Latest != "3.0.0" {
		t.Fatalf("old reader sees %+v", old)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache perms = %v, %v; want 0600", info.Mode().Perm(), err)
	}
}

// TestDaemonUpdateCheckRefreshesEachInterval: the daemon's checker refreshes when
// the last success is about one interval old, even if a CLI recorded a recent
// failed attempt, and uses the retrying fetcher.
func TestDaemonUpdateCheckRefreshesEachInterval(t *testing.T) {
	var once, retry atomic.Int32
	ok := func(counter *atomic.Int32) func(context.Context, string) (update.Manifest, error) {
		return func(context.Context, string) (update.Manifest, error) {
			counter.Add(1)
			return update.Manifest{Channels: map[string]update.ChannelInfo{"stable": {Version: "2.3.0"}}}, nil
		}
	}
	withManifestFetchers(t, ok(&once), ok(&retry))
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "update-available.json")
	writeUpdateCache(path, homebrewHintCache{CheckedAt: time.Now().Add(-updateCheckInterval + 30*time.Second), AttemptedAt: time.Now(), Failures: 1})
	if got := updateAvailable(dir, "u", "stable", update.PolicyNotifyOnly, true); got != "2.3.0" {
		t.Fatalf("daemon check = %q", got)
	}
	if retry.Load() != 1 || once.Load() != 0 {
		t.Fatalf("daemon used once=%d retry=%d fetchers, want retry only", once.Load(), retry.Load())
	}
	// Fresh success: neither path refetches.
	_ = updateAvailable(dir, "u", "stable", update.PolicyNotifyOnly, true)
	_ = UpdateAvailable(dir, "u", "stable", update.PolicyNotifyOnly)
	if retry.Load() != 1 || once.Load() != 0 {
		t.Fatalf("refetched a fresh cache: once=%d retry=%d", once.Load(), retry.Load())
	}
}
