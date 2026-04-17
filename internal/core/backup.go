// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BackupOptions controls how the config-dir backup is created.
type BackupOptions struct {
	// OutputPath is where the .tar.gz will be written.
	// If empty, a timestamped file is placed in the current directory.
	OutputPath string
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
// backup — temp files, lock files, and compiled DB journals.
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

// Backup creates a gzipped tar archive of the entire config directory.
// All server records, keys, audit logs, and (SQLite) database files are included.
// Ephemeral files (lock files, WAL journals, in-progress temp files) are excluded.
func Backup(configDir string, opts BackupOptions) (BackupResult, error) {
	configDir = filepath.Clean(configDir)
	now := time.Now().UTC()

	outputPath := opts.OutputPath
	if outputPath == "" {
		outputPath = fmt.Sprintf("fleet-backup-%s.tar.gz", now.Format("20060102-150405"))
	}

	f, err := os.Create(outputPath)
	if err != nil {
		return BackupResult{}, fmt.Errorf("create backup file: %w", err)
	}
	// No defer f.Close() here — all exit paths call f.Close() explicitly below
	// in the correct order (tw → gz → f) so that the gzip/tar trailers are
	// flushed before the underlying file is closed. A defer would double-close f
	// on the success path and silently swallow the error on the other paths.

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	var filesCount int
	var sizeBytes int64

	err = filepath.Walk(configDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(configDir, path)
		if err != nil {
			return err
		}

		// Always use forward slashes in tar archives (portable).
		archiveName := filepath.ToSlash(rel)

		if info.IsDir() {
			if archiveName == "." {
				return nil
			}
			hdr := &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     archiveName + "/",
				Mode:     int64(info.Mode()),
				ModTime:  info.ModTime(),
			}
			return tw.WriteHeader(hdr)
		}

		if skipBackupPattern(path) {
			return nil
		}

		// Only follow regular files — skip sockets, pipes, devices.
		if !info.Mode().IsRegular() {
			return nil
		}

		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     archiveName,
			Mode:     int64(info.Mode()),
			Size:     info.Size(),
			ModTime:  info.ModTime(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}

		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()

		n, err := io.Copy(tw, src)
		if err != nil {
			return err
		}
		filesCount++
		sizeBytes += n
		return nil
	})

	if err != nil {
		_ = gz.Close()
		_ = f.Close()
		_ = os.Remove(outputPath)
		return BackupResult{}, fmt.Errorf("walk config directory: %w", err)
	}

	if err := tw.Close(); err != nil {
		_ = gz.Close()
		_ = f.Close()
		_ = os.Remove(outputPath)
		return BackupResult{}, fmt.Errorf("finalize tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		_ = f.Close()
		_ = os.Remove(outputPath)
		return BackupResult{}, fmt.Errorf("finalize gzip: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(outputPath)
		return BackupResult{}, fmt.Errorf("close backup file: %w", err)
	}

	stat, err := os.Stat(outputPath)
	if err != nil {
		return BackupResult{}, err
	}

	absOut, _ := filepath.Abs(outputPath)
	return BackupResult{
		OutputPath: absOut,
		ConfigDir:  configDir,
		FilesCount: filesCount,
		SizeBytes:  stat.Size(),
		CreatedAt:  now,
	}, nil
}
