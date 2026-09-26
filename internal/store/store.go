// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// Store is one workload's database. A Store from Open is connected (and its
// schema migrated) up front; one from OpenLazy connects on first use, so a CLI
// command that never touches the database never pays for opening it.
type Store struct {
	mu       sync.Mutex // guards db/sqlDB/closed during a lazy connect or Close
	cfg      DatabaseConfig
	db       *gorm.DB
	sqlDB    *sql.DB
	workload Workload
	backend  Backend
	closed   bool
}

// ErrStoreClosed is returned by operations on a Store after Close.
var ErrStoreClosed = errors.New("store is closed")

// schemaVersion identifies the table/index layout Init produces. Bump it
// whenever a model's GORM tags change or migrate() learns something new; a
// database whose recorded version is at least this skips AutoMigrate on open
// (versions: 1 = implicit, before versioning; 2 = composite
// metric_snapshots(server, timestamp) index + version table).
const schemaVersion = 2

// schemaVersionRow records, per workload, the schemaVersion the database was
// last migrated to. Additive table: older binaries ignore it (and keep running
// their own AutoMigrate, which never removes anything).
type schemaVersionRow struct {
	Workload  string    `gorm:"column:workload;primaryKey;size:64"`
	Version   int       `gorm:"column:version;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null;autoUpdateTime"`
}

func (schemaVersionRow) TableName() string { return "fleet_schema_versions" }

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

// metricsSnapshotRow is one stored sample. Besides the original single-column
// indexes it carries a composite (server, timestamp) index for per-server time
// range reads (latest sample, sparklines); the timestamp index serves the
// retention prune.
type metricsSnapshotRow struct {
	ID        uint64    `gorm:"column:id;primaryKey;autoIncrement"`
	Server    string    `gorm:"column:server;not null;size:255;index;index:idx_metric_snapshots_server_ts,priority:1"`
	Timestamp time.Time `gorm:"column:timestamp;not null;index;index:idx_metric_snapshots_server_ts,priority:2"`
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

// Open connects to the workload's database and brings its schema up to date
// before returning, so configuration and connection errors surface here.
func Open(cfg DatabaseConfig, workload Workload) (*Store, error) {
	s, err := OpenLazy(cfg, workload)
	if err != nil {
		return nil, err
	}
	if _, err := s.conn(); err != nil {
		return nil, err
	}
	return s, nil
}

// OpenLazy validates the configuration and returns a Store that connects and
// migrates on first use (and retries on the next use if that fails). Close is
// cheap and safe on a Store that never connected.
func OpenLazy(cfg DatabaseConfig, workload Workload) (*Store, error) {
	cfg = WithDefaults(cfg, "")
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	switch workload {
	case WorkloadState, WorkloadMetrics, WorkloadEvents:
	default:
		return nil, fmt.Errorf("unsupported store workload %q", workload)
	}
	return &Store{cfg: cfg, workload: workload, backend: cfg.Backend}, nil
}

// conn returns the connected handle, connecting and migrating first if needed.
func (s *Store) conn() (*gorm.DB, error) {
	if s == nil {
		return nil, ErrStoreClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrStoreClosed
	}
	if s.db != nil {
		return s.db, nil
	}
	db, sqlDB, err := openManagedDatabase(s.cfg, s.workload)
	if err != nil {
		return nil, err
	}
	if err := initSchema(db, s.workload); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	s.db, s.sqlDB = db, sqlDB
	return db, nil
}

func openManagedDatabase(cfg DatabaseConfig, workload Workload) (*gorm.DB, *sql.DB, error) {
	var dialector gorm.Dialector
	switch cfg.Backend {
	case BackendSQLite:
		path := cfg.PathFor(workload)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, nil, fmt.Errorf("create database directory: %w", err)
		}
		// Pre-create the db file (and its WAL/SHM/journal sidecars) at 0600
		// BEFORE the driver opens them. The SQLite driver otherwise creates
		// these with umask-default perms (often world-readable) and we only
		// tighten them with restrictSQLitePerms() AFTER open — leaving a brief
		// window where secrets/state could be read. Opening an existing 0600
		// file (O_CREATE without O_EXCL) means the driver never creates it with
		// the loose default, closing that window portably (syscall.Umask is
		// Unix-only; this works on Windows too). An empty/zero-length WAL/SHM is
		// valid and SQLite re-initializes them; a stale -journal is harmless.
		if err := precreateSQLiteFiles(path); err != nil {
			return nil, nil, err
		}
		dialector = sqlite.Open(sqliteDSN(path))
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
		// The driver creates the db (and WAL/SHM sidecars) with world-readable
		// default perms; these may hold secrets/state, so restrict to 0600.
		if err := restrictSQLitePerms(cfg.PathFor(workload)); err != nil {
			return nil, nil, err
		}
	}

	return db, sqlDB, nil
}

// sqliteDSN is the driver DSN for a database file. It asks the driver to run
// PRAGMA synchronous=NORMAL on EVERY pooled connection (the pragma is
// per-connection; a one-off Exec would only reach one of them). With WAL (set
// below, persistent in the file) NORMAL is the documented safe setting: a crash
// or power loss can lose the last commits but never corrupts the database, and
// commits stop paying an fsync each. A path that itself contains '?' cannot
// carry DSN parameters, so it is used verbatim (default synchronous=FULL).
func sqliteDSN(path string) string {
	if strings.Contains(path, "?") {
		return path
	}
	return path + "?_pragma=synchronous(NORMAL)"
}

// sqliteFileSuffixes are the on-disk sidecars SQLite may create alongside the
// main database file. Pre-creating and re-tightening all of them keeps perms
// owner-only regardless of which journal mode is active.
var sqliteFileSuffixes = []string{"", "-wal", "-shm", "-journal"}

// precreateSQLiteFiles creates the SQLite database file and its WAL/SHM/journal
// sidecars at 0600 BEFORE the driver opens the database, so the driver opens
// pre-existing owner-only files instead of creating them with umask-default
// (typically world-readable) perms. This closes the brief readable window that
// restrictSQLitePerms() alone leaves (it only tightens AFTER open). O_CREATE
// (not O_EXCL) so re-opening an existing database is a no-op; an existing file
// that is already 0600 is left as-is. We do not loosen perms on a file that
// already exists with tighter bits.
func precreateSQLiteFiles(path string) error {
	for _, suffix := range sqliteFileSuffixes {
		p := path + suffix
		f, err := os.OpenFile(p, os.O_RDONLY|os.O_CREATE, 0o600) // #nosec G304 -- path derived from validated config
		if err != nil {
			return fmt.Errorf("pre-create sqlite file %s: %w", p, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("pre-create sqlite file %s: %w", p, err)
		}
		// Defend against a pre-existing file created earlier under a loose umask
		// (e.g. by an older fleet version): tighten it before the driver opens.
		if err := os.Chmod(p, 0o600); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("restrict sqlite file permissions: %w", err)
		}
	}
	return nil
}

// restrictSQLitePerms tightens the SQLite database file and its WAL/SHM sidecars
// to 0600 (owner-only), since the driver creates them with umask-default perms
// that are typically world-readable. Missing sidecars are ignored.
func restrictSQLitePerms(path string) error {
	for _, suffix := range sqliteFileSuffixes {
		if err := os.Chmod(path+suffix, 0o600); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("restrict sqlite file permissions: %w", err)
		}
	}
	return nil
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

// SnapshotSQLite writes a transactionally consistent copy of a live SQLite
// store to destination. VACUUM INTO runs through SQLite itself, so committed
// pages that have not yet left the WAL are included; copying the main .db file
// directly is not safe in WAL mode. The destination must not already exist.
func (s *Store) SnapshotSQLite(ctx context.Context, destination string) error {
	if s == nil {
		return fmt.Errorf("snapshot sqlite store: store is closed")
	}
	if _, err := s.conn(); err != nil {
		if errors.Is(err, ErrStoreClosed) {
			return fmt.Errorf("snapshot sqlite store: store is closed")
		}
		return fmt.Errorf("snapshot sqlite store: %w", err)
	}
	s.mu.Lock()
	sqlDB := s.sqlDB
	s.mu.Unlock()
	if s.backend != BackendSQLite {
		return fmt.Errorf("snapshot sqlite store: backend %q is not sqlite", s.backend)
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("snapshot sqlite store: destination already exists")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("snapshot sqlite store: inspect destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("snapshot sqlite store: create destination directory: %w", err)
	}

	// VACUUM INTO accepts a bound filename expression. Using a parameter avoids
	// constructing SQL from a filesystem path.
	if _, err := sqlDB.ExecContext(ctx, "VACUUM INTO ?", destination); err != nil {
		_ = os.Remove(destination)
		return fmt.Errorf("snapshot sqlite store: online backup: %w", err)
	}
	removeOnFailure := true
	defer func() {
		if removeOnFailure {
			_ = os.Remove(destination)
		}
	}()
	if err := os.Chmod(destination, 0o600); err != nil {
		return fmt.Errorf("snapshot sqlite store: secure destination: %w", err)
	}
	f, err := os.Open(destination) // #nosec G304 -- destination is the caller-provided private SQLite snapshot path
	if err != nil {
		return fmt.Errorf("snapshot sqlite store: open destination: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("snapshot sqlite store: sync destination: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("snapshot sqlite store: close destination: %w", err)
	}
	if err := verifySQLiteSnapshot(ctx, destination); err != nil {
		return err
	}
	removeOnFailure = false
	return nil
}

func verifySQLiteSnapshot(ctx context.Context, snapshotPath string) error {
	u := &url.URL{Scheme: "file", Path: snapshotPath}
	db, err := sql.Open("sqlite", u.String()+"?mode=ro&immutable=1")
	if err != nil {
		return fmt.Errorf("verify sqlite snapshot: open: %w", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("verify sqlite snapshot: integrity check: %w", err)
	}
	defer rows.Close()
	checked := false
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return fmt.Errorf("verify sqlite snapshot: read integrity result: %w", err)
		}
		checked = true
		if result != "ok" {
			return fmt.Errorf("verify sqlite snapshot: integrity check failed: %s", result)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("verify sqlite snapshot: integrity result: %w", err)
	}
	if !checked {
		return fmt.Errorf("verify sqlite snapshot: integrity check returned no result")
	}
	return nil
}

// Init connects if needed and makes sure the schema is current. Stores from
// Open and OpenLazy already do this on connect; it is kept for callers that
// want to force the migration step explicitly.
func (s *Store) Init() error {
	_, err := s.conn()
	return err
}

// initSchema brings a freshly opened database's schema up to date. When the
// database already records schemaVersion (or newer) for this workload the
// AutoMigrate pass — several catalog queries per table on every CLI start — is
// skipped entirely. Otherwise AutoMigrate runs as it always has (idempotent,
// additive) and the version is recorded afterwards, so a crash mid-migration
// just migrates again next time. Two processes may migrate an old database at
// the same moment; GORM's check-then-create is not atomic, so if the first pass
// fails and the schema is still not current, one more pass (a no-op for
// whatever the other process already created) settles it.
func initSchema(db *gorm.DB, workload Workload) error {
	if schemaCurrent(db, workload) {
		return nil
	}
	if err := migrateSchema(db, workload); err != nil {
		if schemaCurrent(db, workload) {
			return nil
		}
		if err := migrateSchema(db, workload); err != nil {
			return err
		}
	}
	return db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "workload"}},
		DoUpdates: clause.AssignmentColumns([]string{"version", "updated_at"}),
	}).Create(&schemaVersionRow{Workload: string(workload), Version: schemaVersion}).Error
}

func schemaCurrent(db *gorm.DB, workload Workload) bool {
	var row schemaVersionRow
	err := db.Where("workload = ?", string(workload)).Limit(1).Find(&row).Error
	return err == nil && row.Workload == string(workload) && row.Version >= schemaVersion
}

func migrateSchema(db *gorm.DB, workload Workload) error {
	switch workload {
	case WorkloadState:
		return db.AutoMigrate(&stateRow{}, &schemaVersionRow{})
	case WorkloadMetrics:
		return db.AutoMigrate(&metricsRow{}, &metricsSnapshotRow{}, &schemaVersionRow{})
	case WorkloadEvents:
		return db.AutoMigrate(&eventRow{}, &schemaVersionRow{})
	default:
		return fmt.Errorf("unsupported store workload %q", workload)
	}
}

// Close releases the connection pool (if one was ever opened). Any later use of
// the Store fails with ErrStoreClosed. Safe to call more than once.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.sqlDB == nil {
		return nil
	}
	return s.sqlDB.Close()
}

func (s *Store) PutState(key, value string) error {
	switch s.workload {
	case WorkloadState, WorkloadMetrics:
	default:
		return fmt.Errorf("put state is unsupported for workload %q", s.workload)
	}
	db, err := s.conn()
	if err != nil {
		return err
	}
	return putStateRow(db, s.workload, key, value)
}

// putStateRow upserts one key/value row of a state-like workload.
func putStateRow(db *gorm.DB, workload Workload, key, value string) error {
	upsert := clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
	}
	switch workload {
	case WorkloadState:
		row := stateRow{Key: key, Value: value}
		return db.Clauses(upsert).Create(&row).Error
	case WorkloadMetrics:
		row := metricsRow{Key: key, Value: value}
		return db.Clauses(upsert).Create(&row).Error
	default:
		return fmt.Errorf("put state is unsupported for workload %q", workload)
	}
}

func (s *Store) GetState(key string) (string, error) {
	switch s.workload {
	case WorkloadState, WorkloadMetrics:
	default:
		return "", fmt.Errorf("get state is unsupported for workload %q", s.workload)
	}
	db, err := s.conn()
	if err != nil {
		return "", err
	}
	if s.workload == WorkloadState {
		var row stateRow
		if err := db.Where("key = ?", key).Take(&row).Error; err != nil {
			return "", err
		}
		return row.Value, nil
	}
	var row metricsRow
	if err := db.Where("key = ?", key).Take(&row).Error; err != nil {
		return "", err
	}
	return row.Value, nil
}

func (s *Store) AppendEvent(timestamp time.Time, category, payload string) error {
	if s.workload != WorkloadEvents {
		return fmt.Errorf("append event is unsupported for workload %q", s.workload)
	}
	db, err := s.conn()
	if err != nil {
		return err
	}
	return db.Create(&eventRow{
		Timestamp: timestamp.UTC(),
		Category:  category,
		Payload:   payload,
	}).Error
}

func (s *Store) ListStateEntries() ([]StateEntry, error) {
	switch s.workload {
	case WorkloadState, WorkloadMetrics:
	default:
		return nil, fmt.Errorf("list state entries is unsupported for workload %q", s.workload)
	}
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	if s.workload == WorkloadState {
		var rows []stateRow
		if err := db.Order("key asc").Find(&rows).Error; err != nil {
			return nil, err
		}
		entries := make([]StateEntry, 0, len(rows))
		for _, row := range rows {
			entries = append(entries, StateEntry{Key: row.Key, Value: row.Value})
		}
		return entries, nil
	}
	var rows []metricsRow
	if err := db.Order("key asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	entries := make([]StateEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, StateEntry{Key: row.Key, Value: row.Value})
	}
	return entries, nil
}

func (s *Store) ReplaceStateEntries(entries []StateEntry) error {
	switch s.workload {
	case WorkloadState, WorkloadMetrics:
	default:
		return fmt.Errorf("replace state entries is unsupported for workload %q", s.workload)
	}
	db, err := s.conn()
	if err != nil {
		return err
	}
	if s.workload == WorkloadState {
		return db.Transaction(func(tx *gorm.DB) error {
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
	}
	return db.Transaction(func(tx *gorm.DB) error {
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
}

func (s *Store) ListEvents() ([]EventEntry, error) {
	if s.workload != WorkloadEvents {
		return nil, fmt.Errorf("list events is unsupported for workload %q", s.workload)
	}
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	var rows []eventRow
	if err := db.Order("timestamp asc").Order("id asc").Find(&rows).Error; err != nil {
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
	db, err := s.conn()
	if err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
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
	db, err := s.conn()
	if err != nil {
		return err
	}
	return db.Create(&metricsSnapshotRow{
		Server:    server,
		Timestamp: timestamp.UTC(),
		Payload:   payload,
	}).Error
}

// RecordMetricSnapshot stores one sample in a single transaction: it upserts
// the latest-value state row (key) and appends the history row. Doing both in
// one commit halves the commits (and, in rollback-journal setups, the fsyncs)
// per sample compared with PutState + AppendMetricSnapshot, and a reader never
// sees one without the other.
func (s *Store) RecordMetricSnapshot(key, server string, timestamp time.Time, payload string) error {
	if s.workload != WorkloadMetrics {
		return fmt.Errorf("record metric snapshot is unsupported for workload %q", s.workload)
	}
	db, err := s.conn()
	if err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := putStateRow(tx, WorkloadMetrics, key, payload); err != nil {
			return err
		}
		return tx.Create(&metricsSnapshotRow{
			Server:    server,
			Timestamp: timestamp.UTC(),
			Payload:   payload,
		}).Error
	})
}

// metricPruneBatch bounds how many rows one prune statement deletes, so a large
// backlog is removed in short transactions that never hold the write lock long.
const metricPruneBatch = 5000

// PruneMetricSnapshots deletes metric_snapshots rows with a timestamp before
// cutoff, in bounded batches (using the timestamp index), and returns how many
// rows were removed. It works on every supported backend.
func (s *Store) PruneMetricSnapshots(ctx context.Context, cutoff time.Time) (int64, error) {
	if s.workload != WorkloadMetrics {
		return 0, fmt.Errorf("prune metric snapshots is unsupported for workload %q", s.workload)
	}
	db, err := s.conn()
	if err != nil {
		return 0, err
	}
	var statement string
	switch s.backend {
	case BackendMySQL, BackendMariaDB:
		// MySQL cannot LIMIT an IN (subquery) but supports DELETE ... LIMIT.
		statement = "DELETE FROM `metric_snapshots` WHERE `timestamp` < ? LIMIT ?"
	default:
		// SQLite (without the optional DELETE ... LIMIT build flag) and Postgres.
		statement = `DELETE FROM "metric_snapshots" WHERE "id" IN (SELECT "id" FROM "metric_snapshots" WHERE "timestamp" < ? LIMIT ?)`
	}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res := db.WithContext(ctx).Exec(statement, cutoff.UTC(), metricPruneBatch)
		if res.Error != nil {
			return total, res.Error
		}
		total += res.RowsAffected
		if res.RowsAffected < metricPruneBatch {
			return total, nil
		}
		// Yield between batches so pollers and CLI writers get the lock.
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (s *Store) LatestMetricSnapshot(server string) (MetricSnapshotEntry, error) {
	if s.workload != WorkloadMetrics {
		return MetricSnapshotEntry{}, fmt.Errorf("latest metric snapshot is unsupported for workload %q", s.workload)
	}
	db, err := s.conn()
	if err != nil {
		return MetricSnapshotEntry{}, err
	}
	var row metricsSnapshotRow
	if err := db.Where("server = ?", server).Order("timestamp desc").Order("id desc").Take(&row).Error; err != nil {
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
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	query := db.Order("timestamp desc").Order("id desc")
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
