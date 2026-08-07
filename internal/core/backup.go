// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/store"
)

// BackupOptions controls how the config-dir backup is created.
type BackupOptions struct {
	// OutputPath is where the .tar.gz will be written.
	// If empty, a timestamped file is placed in the current directory.
	OutputPath string

	// SQLiteStores allows an already-open App to provide its live stores. Any
	// missing workload is opened for the duration of the backup. This keeps the
	// public Backup entrypoint useful while ensuring all three SQLite workloads
	// are snapshotted through SQLite rather than copied behind its WAL.
	SQLiteStores map[store.Workload]*store.Store
}

// BackupResult describes a completed backup.
type BackupResult struct {
	OutputPath string    `json:"output_path"`
	ConfigDir  string    `json:"config_dir"`
	FilesCount int       `json:"files_count"`
	SizeBytes  int64     `json:"size_bytes"`
	CreatedAt  time.Time `json:"created_at"`
}

// skipBackupPattern returns true for files that should never be included in a
// backup — temp files, lock files, and SQLite journals/sidecars.
func skipBackupPattern(name string) bool {
	base := filepath.Base(name)
	switch {
	case strings.HasSuffix(base, ".new"),
		strings.HasSuffix(base, ".tmp"),
		strings.HasSuffix(base, ".lock"),
		strings.HasSuffix(base, "-journal"),
		strings.HasSuffix(base, "-wal"),
		strings.HasSuffix(base, "-shm"),
		strings.HasPrefix(base, ".authorized_keys."):
		return true
	}
	return false
}

// Backup creates a gzipped tar archive of the entire config directory. SQLite
// files are first copied with SQLite's online VACUUM INTO mechanism, which
// produces a consistent standalone database even when committed pages exist
// only in WAL. The final archive is created owner-only, exclusively, and
// without following a symlink at the output path.
func Backup(configDir string, opts BackupOptions) (result BackupResult, retErr error) {
	configDir = filepath.Clean(configDir)
	now := time.Now().UTC()
	outputPath := opts.OutputPath
	if outputPath == "" {
		outputPath = fmt.Sprintf("fleet-backup-%s.tar.gz", now.Format("20060102-150405"))
	}
	absOutputPath, err := filepath.Abs(outputPath)
	if err != nil {
		return BackupResult{}, fmt.Errorf("resolve backup output path: %w", err)
	}

	// O_EXCL refuses all existing targets (including symlinks), while
	// O_NOFOLLOW closes the final-component symlink race on supported systems.
	f, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600) // #nosec G304 -- operator-selected backup destination
	if err != nil {
		return BackupResult{}, fmt.Errorf("create backup file exclusively: %w", err)
	}
	keepOutput := false
	defer func() {
		_ = f.Close()
		if !keepOutput {
			_ = os.Remove(outputPath)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return BackupResult{}, fmt.Errorf("secure backup file: %w", err)
	}

	snapshots, cleanupSnapshots, err := prepareSQLiteSnapshots(context.Background(), configDir, opts.SQLiteStores)
	if err != nil {
		return BackupResult{}, err
	}
	defer cleanupSnapshots()

	sourceRoot, err := openVerifiedLocalDir(configDir, 0o700)
	if err != nil {
		return BackupResult{}, fmt.Errorf("open config directory for backup: %w", err)
	}
	defer sourceRoot.Close()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	var filesCount int

	walkErr := filepath.Walk(configDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(configDir, path)
		if err != nil {
			return err
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if filepath.Clean(absPath) == filepath.Clean(absOutputPath) {
			return nil
		}
		archiveName := filepath.ToSlash(rel)
		if info.IsDir() {
			if archiveName == "." {
				return nil
			}
			return tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     archiveName + "/",
				Mode:     int64(info.Mode().Perm()),
				ModTime:  info.ModTime(),
			})
		}
		if skipBackupPattern(path) {
			return nil
		}
		// filepath.Walk uses lstat semantics. Skipping every non-regular entry
		// means backup never follows config-tree symlinks, sockets, or devices.
		if !info.Mode().IsRegular() {
			return nil
		}

		var src *os.File
		if snapshot, ok := snapshots[filepath.Clean(absPath)]; ok {
			src, err = os.Open(snapshot) // #nosec G304 -- generated owner-only SQLite snapshot in private staging
			if err != nil {
				return fmt.Errorf("open sqlite snapshot: %w", err)
			}
			info, err = src.Stat()
			if err != nil {
				_ = src.Close()
				return fmt.Errorf("stat sqlite snapshot: %w", err)
			}
		} else {
			src, err = sourceRoot.OpenFile(rel, os.O_RDONLY|oNoFollow, 0)
			if err != nil {
				return fmt.Errorf("open config entry %q: %w", rel, err)
			}
			opened, statErr := src.Stat()
			if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
				_ = src.Close()
				if statErr != nil {
					return statErr
				}
				return fmt.Errorf("config entry %q changed while opening", rel)
			}
			info = opened
		}
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     archiveName,
			Mode:     int64(info.Mode().Perm()),
			Size:     info.Size(),
			ModTime:  info.ModTime(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = src.Close()
			return err
		}
		_, copyErr := io.Copy(tw, src)
		closeErr := src.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		filesCount++
		return nil
	})
	if walkErr != nil {
		_ = tw.Close()
		_ = gz.Close()
		return BackupResult{}, fmt.Errorf("walk config directory: %w", walkErr)
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return BackupResult{}, fmt.Errorf("finalize tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return BackupResult{}, fmt.Errorf("finalize gzip: %w", err)
	}
	if err := f.Sync(); err != nil {
		return BackupResult{}, fmt.Errorf("sync backup file: %w", err)
	}
	if err := f.Close(); err != nil {
		return BackupResult{}, fmt.Errorf("close backup file: %w", err)
	}
	if err := syncDirectory(filepath.Dir(absOutputPath)); err != nil {
		return BackupResult{}, fmt.Errorf("sync backup output directory: %w", err)
	}
	stat, err := os.Stat(outputPath)
	if err != nil {
		return BackupResult{}, fmt.Errorf("stat backup file: %w", err)
	}
	keepOutput = true
	return BackupResult{
		OutputPath: absOutputPath,
		ConfigDir:  configDir,
		FilesCount: filesCount,
		SizeBytes:  stat.Size(),
		CreatedAt:  now,
	}, nil
}

func prepareSQLiteSnapshots(ctx context.Context, configDir string, supplied map[store.Workload]*store.Store) (map[string]string, func(), error) {
	cfgPath := ConfigPath(configDir)
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		return map[string]string{}, func() {}, nil
	} else if err != nil {
		return nil, func() {}, fmt.Errorf("inspect config for backup: %w", err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return nil, func() {}, fmt.Errorf("load config for backup: %w", err)
	}
	if cfg.Database.Backend != store.BackendSQLite {
		return map[string]string{}, func() {}, nil
	}

	stage, err := os.MkdirTemp("", "fleet-sqlite-backup-*")
	if err != nil {
		return nil, func() {}, fmt.Errorf("create sqlite backup staging directory: %w", err)
	}
	if err := os.Chmod(stage, 0o700); err != nil { // #nosec G302 -- directory is private but requires the owner execute bit for traversal
		_ = os.RemoveAll(stage)
		return nil, func() {}, fmt.Errorf("secure sqlite backup staging directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(stage) }

	workloads := []store.Workload{store.WorkloadState, store.WorkloadMetrics, store.WorkloadEvents}
	snapshots := make(map[string]string, len(workloads))
	opened := make([]*store.Store, 0, len(workloads))
	defer func() {
		for _, st := range opened {
			_ = st.Close()
		}
	}()
	for _, workload := range workloads {
		sourcePath, err := filepath.Abs(cfg.Database.PathFor(workload))
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("resolve %s sqlite path: %w", workload, err)
		}
		if _, duplicate := snapshots[filepath.Clean(sourcePath)]; duplicate {
			cleanup()
			return nil, func() {}, fmt.Errorf("sqlite workloads must use distinct database paths: %s", sourcePath)
		}
		st := supplied[workload]
		if st == nil {
			st, err = store.Open(cfg.Database, workload)
			if err != nil {
				cleanup()
				return nil, func() {}, fmt.Errorf("open %s store for backup: %w", workload, err)
			}
			opened = append(opened, st)
		}
		destination := filepath.Join(stage, string(workload)+".db")
		if err := st.SnapshotSQLite(ctx, destination); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("snapshot %s store: %w", workload, err)
		}
		snapshots[filepath.Clean(sourcePath)] = destination
	}
	return snapshots, cleanup, nil
}
