// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// legacyMetricsSnapshotRow is metric_snapshots as released before schema
// versioning: single-column indexes only.
type legacyMetricsSnapshotRow struct {
	ID        uint64    `gorm:"column:id;primaryKey;autoIncrement"`
	Server    string    `gorm:"column:server;not null;size:255;index"`
	Timestamp time.Time `gorm:"column:timestamp;not null;index"`
	Payload   string    `gorm:"column:payload;type:text;not null"`
}

func (legacyMetricsSnapshotRow) TableName() string { return "metric_snapshots" }

// createLegacyMetricsDB writes a metrics database the way an older release did
// (its AutoMigrate, no version table), with one row in each table.
func createLegacyMetricsDB(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&metricsRow{}, &legacyMetricsSnapshotRow{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&metricsRow{Key: "latest.old", Value: "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&legacyMetricsSnapshotRow{Server: "old", Timestamp: time.Now().UTC(), Payload: "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	_ = sqlDB.Close()
}

func sqliteIndexExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	db, err := s.conn()
	if err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Raw("SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?", name).Scan(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count == 1
}

// TestOpenMigratesLegacyDatabase: an existing pre-versioning database gets the
// composite index and a version row, keeping its data.
func TestOpenMigratesLegacyDatabase(t *testing.T) {
	t.Parallel()
	cfg := DefaultDatabaseConfig(t.TempDir())
	createLegacyMetricsDB(t, cfg.SQLite.MetricsPath)

	st, err := Open(cfg, WorkloadMetrics)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if !sqliteIndexExists(t, st, "idx_metric_snapshots_server_ts") {
		t.Fatal("composite (server, timestamp) index not created on a legacy database")
	}
	db, _ := st.conn()
	if !schemaCurrent(db, WorkloadMetrics) {
		t.Fatal("schema version not recorded after migration")
	}
	if got, err := st.GetState("latest.old"); err != nil || got != "{}" {
		t.Fatalf("legacy state row lost: %q %v", got, err)
	}
	if rows, err := st.ListMetricSnapshots("old", 0); err != nil || len(rows) != 1 {
		t.Fatalf("legacy snapshot rows lost: %d %v", len(rows), err)
	}
}

// TestOpenSkipsAutoMigrateWhenVersionCurrent proves the fast path: once the
// version is recorded, opening does not run AutoMigrate (a dropped index is not
// recreated), and bumping past it is also treated as current.
func TestOpenSkipsAutoMigrateWhenVersionCurrent(t *testing.T) {
	t.Parallel()
	cfg := DefaultDatabaseConfig(t.TempDir())
	st, err := Open(cfg, WorkloadMetrics)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := st.conn()
	if err := db.Exec("DROP INDEX idx_metric_snapshots_server_ts").Error; err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	st, err = Open(cfg, WorkloadMetrics)
	if err != nil {
		t.Fatal(err)
	}
	if sqliteIndexExists(t, st, "idx_metric_snapshots_server_ts") {
		t.Fatal("AutoMigrate ran although the schema version was current")
	}
	// A lower recorded version triggers the migration again.
	db, _ = st.conn()
	if err := db.Exec("UPDATE fleet_schema_versions SET version = 1 WHERE workload = ?", string(WorkloadMetrics)).Error; err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	st, err = Open(cfg, WorkloadMetrics)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if !sqliteIndexExists(t, st, "idx_metric_snapshots_server_ts") {
		t.Fatal("outdated schema version did not trigger migration")
	}
}

// TestSchemaVersionIsPerWorkload: two workloads sharing one database file (a
// legal configuration) must each get their tables.
func TestSchemaVersionIsPerWorkload(t *testing.T) {
	t.Parallel()
	cfg := DefaultDatabaseConfig(t.TempDir())
	cfg.SQLite.MetricsPath = cfg.SQLite.StatePath
	state, err := Open(cfg, WorkloadState)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	metrics, err := Open(cfg, WorkloadMetrics)
	if err != nil {
		t.Fatal(err)
	}
	defer metrics.Close()
	if err := metrics.RecordMetricSnapshot("latest.a", "a", time.Now(), "{}"); err != nil {
		t.Fatalf("metrics tables missing on a shared file: %v", err)
	}
}

// TestConcurrentOpenOfLegacyDatabase: several processes' worth of opens racing
// on an unmigrated database must all succeed.
func TestConcurrentOpenOfLegacyDatabase(t *testing.T) {
	t.Parallel()
	cfg := DefaultDatabaseConfig(t.TempDir())
	createLegacyMetricsDB(t, cfg.SQLite.MetricsPath)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := Open(cfg, WorkloadMetrics)
			if err != nil {
				errs <- err
				return
			}
			errs <- st.RecordMetricSnapshot("latest.x", "x", time.Now(), "{}")
			_ = st.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent open/migrate failed: %v", err)
		}
	}
}

// TestOpenLazyDefersConnection: a lazily opened store touches no files until
// used, Close before use is a no-op, and use after Close fails.
func TestOpenLazyDefersConnection(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	cfg := DefaultDatabaseConfig(base)
	st, err := OpenLazy(cfg, WorkloadState)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.SQLite.StatePath); !os.IsNotExist(err) {
		t.Fatalf("lazy open created the database file (stat err %v)", err)
	}
	if err := st.PutState("k", "v"); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetState("k"); err != nil || got != "v" {
		t.Fatalf("GetState = %q %v", got, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := st.PutState("k", "w"); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("use after Close: %v, want ErrStoreClosed", err)
	}

	unused, err := OpenLazy(DefaultDatabaseConfig(t.TempDir()), WorkloadEvents)
	if err != nil {
		t.Fatal(err)
	}
	if err := unused.Close(); err != nil {
		t.Fatalf("Close of a never-used lazy store: %v", err)
	}
	if _, err := OpenLazy(cfg, Workload("bogus")); err == nil {
		t.Fatal("OpenLazy accepted an unknown workload")
	}
}

// TestSQLiteSynchronousNormalOnEveryConnection: synchronous=NORMAL is set per
// connection through the DSN, so every pooled connection has it.
func TestSQLiteSynchronousNormalOnEveryConnection(t *testing.T) {
	t.Parallel()
	st, err := Open(DefaultDatabaseConfig(t.TempDir()), WorkloadMetrics)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	conns := make([]interface{ Close() error }, 0, 3)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < 3; i++ {
		c, err := st.sqlDB.Conn(ctx) // three distinct pooled connections, held open
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		var mode int
		if err := c.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if mode != 1 {
			t.Fatalf("connection %d: synchronous = %d, want 1 (NORMAL)", i, mode)
		}
		var journal string
		if err := c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
			t.Fatal(err)
		}
		if journal != "wal" {
			t.Fatalf("connection %d: journal_mode = %q, want wal", i, journal)
		}
	}
}

func TestRecordMetricSnapshotWritesStateAndHistory(t *testing.T) {
	t.Parallel()
	st, err := Open(DefaultDatabaseConfig(t.TempDir()), WorkloadMetrics)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if err := st.RecordMetricSnapshot("latest.web", "web", now.Add(time.Duration(i)*time.Second), fmt.Sprintf(`{"i":%d}`, i)); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := st.GetState("latest.web"); err != nil || got != `{"i":2}` {
		t.Fatalf("latest state = %q %v", got, err)
	}
	latest, err := st.LatestMetricSnapshot("web")
	if err != nil || latest.Payload != `{"i":2}` {
		t.Fatalf("latest snapshot = %+v %v", latest, err)
	}
	if rows, _ := st.ListMetricSnapshots("web", 0); len(rows) != 3 {
		t.Fatalf("history rows = %d, want 3", len(rows))
	}
	state, err := Open(DefaultDatabaseConfig(t.TempDir()), WorkloadState)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err := state.RecordMetricSnapshot("k", "s", now, "{}"); err == nil {
		t.Fatal("RecordMetricSnapshot accepted a non-metrics workload")
	}
}

// TestPruneMetricSnapshots deletes only rows older than the cutoff, across more
// rows than one batch.
func TestPruneMetricSnapshots(t *testing.T) {
	t.Parallel()
	st, err := Open(DefaultDatabaseConfig(t.TempDir()), WorkloadMetrics)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	db, _ := st.conn()
	now := time.Now().UTC()
	old := make([]metricsSnapshotRow, 0, metricPruneBatch+700)
	for i := 0; i < metricPruneBatch+700; i++ {
		old = append(old, metricsSnapshotRow{Server: fmt.Sprintf("s%d", i%7), Timestamp: now.Add(-40*24*time.Hour + time.Duration(i)*time.Second), Payload: "{}"})
	}
	if err := db.CreateInBatches(old, 500).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		if err := st.RecordMetricSnapshot("latest.s", "s1", now.Add(-time.Duration(i)*time.Hour), "{}"); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := st.PruneMetricSnapshots(context.Background(), now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if removed != int64(len(old)) {
		t.Fatalf("pruned %d rows, want %d", removed, len(old))
	}
	rows, err := st.ListMetricSnapshots("", 0)
	if err != nil || len(rows) != 25 {
		t.Fatalf("remaining rows = %d (%v), want the 25 recent ones", len(rows), err)
	}
	if again, err := st.PruneMetricSnapshots(context.Background(), now.Add(-30*24*time.Hour)); err != nil || again != 0 {
		t.Fatalf("second prune = %d %v", again, err)
	}
}

// BenchmarkOpenExistingStore measures opening an up-to-date metrics store and
// running one query (connection + schema check), i.e. what a CLI command that
// touches the database pays.
func BenchmarkOpenExistingStore(b *testing.B) {
	cfg := DefaultDatabaseConfig(b.TempDir())
	st, err := Open(cfg, WorkloadMetrics)
	if err != nil {
		b.Fatal(err)
	}
	_ = st.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := Open(cfg, WorkloadMetrics)
		if err != nil {
			b.Fatal(err)
		}
		_ = st.Close()
	}
}
