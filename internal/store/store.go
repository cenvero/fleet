// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type Backend string

const (
	BackendSQLite   Backend = "sqlite"
	BackendPostgres Backend = "postgres"
	BackendMySQL    Backend = "mysql"
	BackendMariaDB  Backend = "mariadb"
)

type Workload string

const (
	WorkloadState   Workload = "state"
	WorkloadMetrics Workload = "metrics"
	WorkloadEvents  Workload = "events"
)

type DatabaseConfig struct {
	Backend  Backend          `toml:"backend" json:"backend"`
	SQLite   SQLiteConfig     `toml:"sqlite" json:"sqlite"`
	Postgres SQLBackendConfig `toml:"postgres" json:"postgres"`
	MySQL    SQLBackendConfig `toml:"mysql" json:"mysql"`
	MariaDB  SQLBackendConfig `toml:"mariadb" json:"mariadb"`
}

type SQLiteConfig struct {
	StatePath   string `toml:"state_path" json:"state_path"`
	MetricsPath string `toml:"metrics_path" json:"metrics_path"`
	EventsPath  string `toml:"events_path" json:"events_path"`
}

type SQLBackendConfig struct {
	DSN             string `toml:"dsn" json:"dsn"`
	MaxOpenConns    int    `toml:"max_open_conns" json:"max_open_conns"`
	MaxIdleConns    int    `toml:"max_idle_conns" json:"max_idle_conns"`
	ConnMaxLifetime string `toml:"conn_max_lifetime" json:"conn_max_lifetime"`
}

type Store struct {
	db       *gorm.DB
	sqlDB    *sql.DB
	workload Workload
}

type StateEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type EventEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Category  string    `json:"category"`
	Payload   string    `json:"payload"`
}

type MetricSnapshotEntry struct {
	Server    string    `json:"server"`
	Timestamp time.Time `json:"timestamp"`
	Payload   string    `json:"payload"`
}

type stateRow struct {
	Key       string    `gorm:"column:key;primaryKey;size:255"`
	Value     string    `gorm:"column:value;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null;autoUpdateTime"`
}

func (stateRow) TableName() string { return "controller_state" }

type metricsRow struct {
	Key       string    `gorm:"column:key;primaryKey;size:255"`
	Value     string    `gorm:"column:value;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null;autoUpdateTime"`
}

func (metricsRow) TableName() string { return "metrics_state" }

type metricsSnapshotRow struct {
	ID        uint64    `gorm:"column:id;primaryKey;autoIncrement"`
	Server    string    `gorm:"column:server;not null;size:255;index"`
	Timestamp time.Time `gorm:"column:timestamp;not null;index"`
	Payload   string    `gorm:"column:payload;type:text;not null"`
}

func (metricsSnapshotRow) TableName() string { return "metric_snapshots" }

type eventRow struct {
	ID        uint64    `gorm:"column:id;primaryKey;autoIncrement"`
	Timestamp time.Time `gorm:"column:timestamp;not null;index"`
	Category  string    `gorm:"column:category;not null;size:128;index"`
	Payload   string    `gorm:"column:payload;type:text;not null"`
}

func (eventRow) TableName() string { return "events" }

func DefaultDatabaseConfig(configDir string) DatabaseConfig {
	sqlDefaults := SQLBackendConfig{
		MaxOpenConns:    10,
		MaxIdleConns:    5,
		ConnMaxLifetime: "30m",
	}
	return DatabaseConfig{
		Backend: BackendSQLite,
		SQLite: SQLiteConfig{
			StatePath:   filepath.Join(configDir, "data", "state.db"),
			MetricsPath: filepath.Join(configDir, "data", "metrics.db"),
			EventsPath:  filepath.Join(configDir, "data", "events.db"),
		},
		Postgres: sqlDefaults,
		MySQL:    sqlDefaults,
		MariaDB:  sqlDefaults,
	}
}

func WithDefaults(cfg DatabaseConfig, configDir string) DatabaseConfig {
	defaults := DefaultDatabaseConfig(configDir)
	if cfg.Backend == "" {
		cfg.Backend = defaults.Backend
	}
	if cfg.SQLite.StatePath == "" {
		cfg.SQLite.StatePath = defaults.SQLite.StatePath
	}
	if cfg.SQLite.MetricsPath == "" {
		cfg.SQLite.MetricsPath = defaults.SQLite.MetricsPath
	}
	if cfg.SQLite.EventsPath == "" {
		cfg.SQLite.EventsPath = defaults.SQLite.EventsPath
	}
	cfg.Postgres = withSQLDefaults(cfg.Postgres, defaults.Postgres)
	cfg.MySQL = withSQLDefaults(cfg.MySQL, defaults.MySQL)
	cfg.MariaDB = withSQLDefaults(cfg.MariaDB, defaults.MariaDB)
	return cfg
}

func withSQLDefaults(cfg SQLBackendConfig, defaults SQLBackendConfig) SQLBackendConfig {
	if cfg.MaxOpenConns == 0 {
		cfg.MaxOpenConns = defaults.MaxOpenConns
	}
	if cfg.MaxIdleConns == 0 {
		cfg.MaxIdleConns = defaults.MaxIdleConns
	}
	if cfg.ConnMaxLifetime == "" {
		cfg.ConnMaxLifetime = defaults.ConnMaxLifetime
	}
	return cfg
}

func (c DatabaseConfig) Validate() error {
	switch c.Backend {
	case BackendSQLite:
		if c.SQLite.StatePath == "" {
			return fmt.Errorf("sqlite state path is required")
		}
		if c.SQLite.MetricsPath == "" {
			return fmt.Errorf("sqlite metrics path is required")
		}
		if c.SQLite.EventsPath == "" {
			return fmt.Errorf("sqlite events path is required")
		}
	case BackendPostgres:
		if c.Postgres.DSN == "" {
			return fmt.Errorf("postgres dsn is required")
		}
	case BackendMySQL:
		if c.MySQL.DSN == "" {
			return fmt.Errorf("mysql dsn is required")
		}
	case BackendMariaDB:
		if c.MariaDB.DSN == "" {
			return fmt.Errorf("mariadb dsn is required")
		}
	default:
		return fmt.Errorf("unsupported database backend %q", c.Backend)
	}
	return nil
}

func (c DatabaseConfig) PathFor(workload Workload) string {
	switch workload {
	case WorkloadState:
		return c.SQLite.StatePath
	case WorkloadMetrics:
		return c.SQLite.MetricsPath
	case WorkloadEvents:
		return c.SQLite.EventsPath
	default:
		return ""
	}
}

func Open(cfg DatabaseConfig, workload Workload) (*Store, error) {
	cfg = WithDefaults(cfg, "")
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	db, sqlDB, err := openManagedDatabase(cfg, workload)
	if err != nil {
		return nil, err
	}

	s := &Store{
		db:       db,
		sqlDB:    sqlDB,
		workload: workload,
	}
	if err := s.Init(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return s, nil
}

func openManagedDatabase(cfg DatabaseConfig, workload Workload) (*gorm.DB, *sql.DB, error) {
	var dialector gorm.Dialector
	switch cfg.Backend {
	case BackendSQLite:
		path := cfg.PathFor(workload)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, nil, fmt.Errorf("create database directory: %w", err)
		}
		dialector = sqlite.Open(path)
	case BackendPostgres:
		dialector = postgres.Open(cfg.Postgres.DSN)
	case BackendMySQL:
		dialector = mysql.Open(cfg.MySQL.DSN)
	case BackendMariaDB:
		dialector = mysql.Open(cfg.MariaDB.DSN)
	default:
		return nil, nil, fmt.Errorf("unsupported database backend %q", cfg.Backend)
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		PrepareStmt: true,
		Logger:      logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("open %s database for %s: %w", cfg.Backend, workload, err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, nil, fmt.Errorf("unwrap %s database handle: %w", cfg.Backend, err)
	}
	configurePool(sqlDB, cfg.sqlSettings())

	if cfg.Backend == BackendSQLite {
		if err := db.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
			return nil, nil, fmt.Errorf("enable sqlite foreign keys: %w", err)
		}
		if err := db.Exec("PRAGMA journal_mode = WAL").Error; err != nil {
			return nil, nil, fmt.Errorf("enable sqlite WAL mode: %w", err)
		}
	}

	return db, sqlDB, nil
}

func configurePool(sqlDB *sql.DB, cfg SQLBackendConfig) {
	if sqlDB == nil {
		return
	}
	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime != "" {
		if d, err := time.ParseDuration(cfg.ConnMaxLifetime); err == nil {
			sqlDB.SetConnMaxLifetime(d)
		}
	}
}

func (c DatabaseConfig) sqlSettings() SQLBackendConfig {
	switch c.Backend {
	case BackendPostgres:
		return c.Postgres
	case BackendMySQL:
		return c.MySQL
	case BackendMariaDB:
		return c.MariaDB
	default:
		return SQLBackendConfig{}
	}
}

func (s *Store) Init() error {
	switch s.workload {
	case WorkloadState:
		return s.db.AutoMigrate(&stateRow{})
	case WorkloadMetrics:
		return s.db.AutoMigrate(&metricsRow{}, &metricsSnapshotRow{})
	case WorkloadEvents:
		return s.db.AutoMigrate(&eventRow{})
	default:
		return fmt.Errorf("unsupported store workload %q", s.workload)
	}
}

func (s *Store) Close() error {
	if s == nil || s.sqlDB == nil {
		return nil
	}
	return s.sqlDB.Close()
}

func (s *Store) PutState(key, value string) error {
	switch s.workload {
	case WorkloadState:
		row := stateRow{Key: key, Value: value}
		return s.db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
		}).Create(&row).Error
	case WorkloadMetrics:
		row := metricsRow{Key: key, Value: value}
		return s.db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
		}).Create(&row).Error
	default:
		return fmt.Errorf("put state is unsupported for workload %q", s.workload)
	}
}

func (s *Store) GetState(key string) (string, error) {
	switch s.workload {
	case WorkloadState:
		var row stateRow
		if err := s.db.Where("key = ?", key).Take(&row).Error; err != nil {
			return "", err
		}
		return row.Value, nil
	case WorkloadMetrics:
		var row metricsRow
		if err := s.db.Where("key = ?", key).Take(&row).Error; err != nil {
			return "", err
		}
		return row.Value, nil
	default:
		return "", fmt.Errorf("get state is unsupported for workload %q", s.workload)
	}
}

func (s *Store) AppendEvent(timestamp time.Time, category, payload string) error {
	if s.workload != WorkloadEvents {
		return fmt.Errorf("append event is unsupported for workload %q", s.workload)
	}
	return s.db.Create(&eventRow{
		Timestamp: timestamp.UTC(),
		Category:  category,
		Payload:   payload,
	}).Error
}

func (s *Store) ListStateEntries() ([]StateEntry, error) {
	switch s.workload {
	case WorkloadState:
		var rows []stateRow
		if err := s.db.Order("key asc").Find(&rows).Error; err != nil {
			return nil, err
		}
		entries := make([]StateEntry, 0, len(rows))
		for _, row := range rows {
			entries = append(entries, StateEntry{Key: row.Key, Value: row.Value})
		}
		return entries, nil
	case WorkloadMetrics:
		var rows []metricsRow
		if err := s.db.Order("key asc").Find(&rows).Error; err != nil {
			return nil, err
		}
		entries := make([]StateEntry, 0, len(rows))
		for _, row := range rows {
			entries = append(entries, StateEntry{Key: row.Key, Value: row.Value})
		}
		return entries, nil
	default:
		return nil, fmt.Errorf("list state entries is unsupported for workload %q", s.workload)
	}
}

func (s *Store) ReplaceStateEntries(entries []StateEntry) error {
	switch s.workload {
	case WorkloadState:
		return s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&stateRow{}).Error; err != nil {
				return err
			}
			if len(entries) == 0 {
				return nil
			}
			rows := make([]stateRow, 0, len(entries))
			for _, entry := range entries {
				rows = append(rows, stateRow{Key: entry.Key, Value: entry.Value})
			}
			return tx.CreateInBatches(rows, 200).Error
		})
	case WorkloadMetrics:
		return s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&metricsRow{}).Error; err != nil {
				return err
			}
			if len(entries) == 0 {
				return nil
			}
			rows := make([]metricsRow, 0, len(entries))
			for _, entry := range entries {
				rows = append(rows, metricsRow{Key: entry.Key, Value: entry.Value})
			}
			return tx.CreateInBatches(rows, 200).Error
		})
	default:
		return fmt.Errorf("replace state entries is unsupported for workload %q", s.workload)
	}
}

func (s *Store) ListEvents() ([]EventEntry, error) {
	if s.workload != WorkloadEvents {
		return nil, fmt.Errorf("list events is unsupported for workload %q", s.workload)
	}
	var rows []eventRow
	if err := s.db.Order("timestamp asc").Order("id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	entries := make([]EventEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, EventEntry{
			Timestamp: row.Timestamp,
			Category:  row.Category,
			Payload:   row.Payload,
		})
	}
	return entries, nil
}

func (s *Store) ReplaceEvents(entries []EventEntry) error {
	if s.workload != WorkloadEvents {
		return fmt.Errorf("replace events is unsupported for workload %q", s.workload)
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&eventRow{}).Error; err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		rows := make([]eventRow, 0, len(entries))
		for _, entry := range entries {
			rows = append(rows, eventRow{
				Timestamp: entry.Timestamp.UTC(),
				Category:  entry.Category,
				Payload:   entry.Payload,
			})
		}
		return tx.CreateInBatches(rows, 200).Error
	})
}

func (s *Store) AppendMetricSnapshot(server string, timestamp time.Time, payload string) error {
	if s.workload != WorkloadMetrics {
		return fmt.Errorf("append metric snapshot is unsupported for workload %q", s.workload)
	}
	return s.db.Create(&metricsSnapshotRow{
		Server:    server,
		Timestamp: timestamp.UTC(),
		Payload:   payload,
	}).Error
}

func (s *Store) LatestMetricSnapshot(server string) (MetricSnapshotEntry, error) {
	if s.workload != WorkloadMetrics {
		return MetricSnapshotEntry{}, fmt.Errorf("latest metric snapshot is unsupported for workload %q", s.workload)
	}
	var row metricsSnapshotRow
	if err := s.db.Where("server = ?", server).Order("timestamp desc").Order("id desc").Take(&row).Error; err != nil {
		return MetricSnapshotEntry{}, err
	}
	return MetricSnapshotEntry{
		Server:    row.Server,
		Timestamp: row.Timestamp,
		Payload:   row.Payload,
	}, nil
}

func (s *Store) ListMetricSnapshots(server string, limit int) ([]MetricSnapshotEntry, error) {
	if s.workload != WorkloadMetrics {
		return nil, fmt.Errorf("list metric snapshots is unsupported for workload %q", s.workload)
	}
	query := s.db.Order("timestamp desc").Order("id desc")
	if server != "" {
		query = query.Where("server = ?", server)
	}
	if limit > 0 {
		query = query.Limit(limit)
	}
	var rows []metricsSnapshotRow
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	entries := make([]MetricSnapshotEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, MetricSnapshotEntry{
			Server:    row.Server,
			Timestamp: row.Timestamp,
			Payload:   row.Payload,
		})
	}
	return entries, nil
}
