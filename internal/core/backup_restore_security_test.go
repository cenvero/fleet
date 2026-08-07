// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/store"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
)

func TestBackupOutputIsOwnerOnlyAndExclusive(t *testing.T) {
	configDir := newBackupTestConfig(t)
	output := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := Backup(configDir, BackupOptions{OutputPath: output}); err != nil {
		t.Fatalf("Backup() error = %v", err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("backup permissions = %o, want 0600", got)
	}
	if _, err := Backup(configDir, BackupOptions{OutputPath: output}); err == nil {
		t.Fatal("second Backup() unexpectedly overwrote existing output")
	}
}

func TestBackupRefusesOutputSymlink(t *testing.T) {
	configDir := newBackupTestConfig(t)
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "backup.tar.gz")
	if err := os.Symlink(victim, output); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Backup(configDir, BackupOptions{OutputPath: output}); err == nil {
		t.Fatal("Backup() unexpectedly followed output symlink")
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "unchanged" {
		t.Fatalf("symlink target changed to %q", got)
	}
}

func TestBackupRemovesPartialOutputOnSnapshotFailure(t *testing.T) {
	configDir := t.TempDir()
	if err := os.WriteFile(ConfigPath(configDir), []byte("not = [valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "partial.tar.gz")
	if _, err := Backup(configDir, BackupOptions{OutputPath: output}); err == nil {
		t.Fatal("Backup() expected invalid-config error")
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("partial output remains: %v", err)
	}
}

func TestBackupSQLiteWALCommittedDataRoundTrip(t *testing.T) {
	configDir := newBackupTestConfig(t)
	cfg, err := LoadConfig(ConfigPath(configDir))
	if err != nil {
		t.Fatal(err)
	}
	stores := make(map[store.Workload]*store.Store)
	for _, workload := range []store.Workload{store.WorkloadState, store.WorkloadMetrics, store.WorkloadEvents} {
		st, err := store.Open(cfg.Database, workload)
		if err != nil {
			t.Fatalf("Open(%s): %v", workload, err)
		}
		stores[workload] = st
		defer st.Close()
	}
	if err := stores[store.WorkloadState].PutState("wal-state", "present"); err != nil {
		t.Fatal(err)
	}
	if err := stores[store.WorkloadMetrics].PutState("wal-metrics", "present"); err != nil {
		t.Fatal(err)
	}
	if err := stores[store.WorkloadEvents].AppendEvent(time.Now(), "wal-event", `{"present":true}`); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(t.TempDir(), "wal.tar.gz")
	if _, err := Backup(configDir, BackupOptions{OutputPath: archive, SQLiteStores: stores}); err != nil {
		t.Fatalf("Backup(): %v", err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := RestoreBackup(archive, restored); err != nil {
		t.Fatalf("RestoreBackup(): %v", err)
	}
	restoredCfg := store.DefaultDatabaseConfig(restored)
	stateDB, err := store.Open(restoredCfg, store.WorkloadState)
	if err != nil {
		t.Fatal(err)
	}
	defer stateDB.Close()
	if got, err := stateDB.GetState("wal-state"); err != nil || got != "present" {
		t.Fatalf("restored state = %q, %v", got, err)
	}
	metricsDB, err := store.Open(restoredCfg, store.WorkloadMetrics)
	if err != nil {
		t.Fatal(err)
	}
	defer metricsDB.Close()
	if got, err := metricsDB.GetState("wal-metrics"); err != nil || got != "present" {
		t.Fatalf("restored metrics = %q, %v", got, err)
	}
	eventsDB, err := store.Open(restoredCfg, store.WorkloadEvents)
	if err != nil {
		t.Fatal(err)
	}
	defer eventsDB.Close()
	events, err := eventsDB.ListEvents()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		found = found || event.Category == "wal-event"
	}
	if !found {
		t.Fatal("WAL-backed event missing from restored snapshot")
	}
}

func TestRestoreRejectsExcessiveDataAfterTarTerminator(t *testing.T) {
	archive := makeTestArchive(t, []testArchiveMember{{name: "config.toml", body: []byte("new")}})
	var extra bytes.Buffer
	gz := gzip.NewWriter(&extra)
	if _, err := gz.Write(make([]byte, maxRestoreTrailerSize+1)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive = append(archive, extra.Bytes()...)
	archivePath := filepath.Join(t.TempDir(), "excessive-tail.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	live := makeLiveConfig(t)
	if err := RestoreBackup(archivePath, live); err == nil {
		t.Fatal("RestoreBackup() accepted excessive post-tar decompressed data")
	}
	assertLiveMarker(t, live)
}
func TestRestoreRejectsLateCorruptionWithoutChangingLiveConfig(t *testing.T) {
	archive := makeTestArchive(t, []testArchiveMember{{name: "config.toml", body: []byte("new")}})
	archive[len(archive)-1] ^= 0xff // corrupt gzip trailer, after tar terminator
	archivePath := filepath.Join(t.TempDir(), "corrupt.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	live := makeLiveConfig(t)
	if err := RestoreBackup(archivePath, live); err == nil {
		t.Fatal("RestoreBackup() accepted late gzip corruption")
	}
	assertLiveMarker(t, live)
}

func TestRestoreRejectsSymlinkMemberAndDestination(t *testing.T) {
	t.Run("archive member", func(t *testing.T) {
		archive := makeTestArchive(t, []testArchiveMember{{name: "link", typeflag: tar.TypeSymlink, linkname: "../victim"}})
		path := filepath.Join(t.TempDir(), "symlink.tar.gz")
		if err := os.WriteFile(path, archive, 0o600); err != nil {
			t.Fatal(err)
		}
		live := makeLiveConfig(t)
		if err := RestoreBackup(path, live); err == nil {
			t.Fatal("RestoreBackup() accepted symlink member")
		}
		assertLiveMarker(t, live)
	})
	t.Run("live destination", func(t *testing.T) {
		dir := t.TempDir()
		victim := filepath.Join(dir, "victim")
		if err := os.Mkdir(victim, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(victim, "marker"), []byte("live"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "config")
		if err := os.Symlink(victim, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		archivePath := filepath.Join(dir, "valid.tar.gz")
		if err := os.WriteFile(archivePath, makeTestArchive(t, []testArchiveMember{{name: "config.toml", body: []byte("new")}}), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := RestoreBackup(archivePath, link); err == nil {
			t.Fatal("RestoreBackup() accepted symlink destination")
		}
		got, _ := os.ReadFile(filepath.Join(victim, "marker"))
		if string(got) != "live" {
			t.Fatalf("victim marker changed to %q", got)
		}
	})
}

func TestRestoreAggregateLimitLeavesLiveConfigUntouched(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "large.tar.gz")
	archive := makeTestArchive(t, []testArchiveMember{{name: "one", body: []byte("12")}, {name: "two", body: []byte("34")}})
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	live := makeLiveConfig(t)
	limits := restoreLimits{fileSize: 4, totalSize: 3, members: 10}
	if err := restoreBackupWithLimits(archivePath, live, limits, os.Rename); err == nil {
		t.Fatal("restore unexpectedly accepted aggregate beyond limit")
	}
	assertLiveMarker(t, live)
}

func TestRestoreCommitFailureRollsBackLiveConfig(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "valid.tar.gz")
	if err := os.WriteFile(archivePath, makeTestArchive(t, []testArchiveMember{{name: "config.toml", body: []byte("new")}}), 0o600); err != nil {
		t.Fatal(err)
	}
	live := makeLiveConfig(t)
	calls := 0
	rename := func(old, new string) error {
		calls++
		if calls == 2 {
			return errors.New("injected commit failure")
		}
		return os.Rename(old, new)
	}
	if err := restoreBackupWithLimits(archivePath, live, defaultRestoreLimits(), rename); err == nil {
		t.Fatal("restore unexpectedly succeeded")
	}
	if calls != 3 {
		t.Fatalf("rename calls = %d, want commit failure plus rollback", calls)
	}
	assertLiveMarker(t, live)
	if _, err := os.Stat(filepath.Join(live, "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("new config leaked into live tree: %v", err)
	}
}

func newBackupTestConfig(t *testing.T) string {
	t.Helper()
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeDirect,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize(): %v", err)
	}
	return configDir
}

type testArchiveMember struct {
	name     string
	body     []byte
	typeflag byte
	linkname string
}

func makeTestArchive(t *testing.T, members []testArchiveMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, member := range members {
		typeflag := member.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		hdr := &tar.Header{Name: member.name, Mode: 0o600, Typeflag: typeflag, Linkname: member.linkname}
		if typeflag == tar.TypeReg {
			hdr.Size = int64(len(member.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if len(member.body) > 0 {
			if _, err := tw.Write(member.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeLiveConfig(t *testing.T) string {
	t.Helper()
	live := filepath.Join(t.TempDir(), "live")
	if err := os.Mkdir(live, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "marker"), []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	return live
}

func assertLiveMarker(t *testing.T, live string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(live, "marker"))
	if err != nil {
		t.Fatalf("read live marker: %v", err)
	}
	if string(got) != "live" {
		t.Fatalf("live marker = %q, want live", got)
	}
}
