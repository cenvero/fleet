// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestBackupSkipsOutputInsideConfigDir(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		t.Fatalf("MkdirAll(configDir) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("alias = \"fleet\"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	outputPath := filepath.Join(configDir, "self.tar.gz")
	result, err := Backup(configDir, BackupOptions{OutputPath: outputPath})
	if err != nil {
		t.Fatalf("Backup() error = %v", err)
	}
	if result.OutputPath != outputPath {
		t.Fatalf("expected output path %q, got %q", outputPath, result.OutputPath)
	}

	names := readTarGzNames(t, outputPath)
	if slices.Contains(names, "self.tar.gz") {
		t.Fatalf("backup archive included itself: %#v", names)
	}
	if !slices.Contains(names, "config.toml") {
		t.Fatalf("backup archive omitted config.toml: %#v", names)
	}
}

func readTarGzNames(t *testing.T, path string) []string {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open(%s) error = %v", path, err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip.NewReader() error = %v", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatalf("tar.Next() error = %v", err)
		}
		names = append(names, hdr.Name)
	}
}
