// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package update

import (
	"os"
	"path/filepath"
)

func replaceFilePlatform(stagedPath, targetPath string) error {
	if err := os.Rename(stagedPath, targetPath); err != nil {
		return err
	}
	return syncParentDir(targetPath)
}

func renameReplacePlatform(sourcePath, targetPath string) error {
	return os.Rename(sourcePath, targetPath)
}

func syncParentDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
