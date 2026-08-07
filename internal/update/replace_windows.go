// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build windows

package update

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func replaceFilePlatform(stagedPath, targetPath string) error {
	oldPath := targetPath + ".old"
	_ = os.Remove(oldPath)
	targetExisted := true
	if err := moveFileWriteThrough(targetPath, oldPath, true); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		targetExisted = false
	}
	if err := moveFileWriteThrough(stagedPath, targetPath, true); err != nil {
		if targetExisted {
			if restoreErr := moveFileWriteThrough(oldPath, targetPath, true); restoreErr != nil {
				return errors.Join(err, fmt.Errorf("restore original executable after replacement failure: %w", restoreErr))
			}
		}
		return err
	}
	if targetExisted {
		_ = os.Remove(oldPath)
	}
	return nil
}

func renameReplacePlatform(sourcePath, targetPath string) error {
	return moveFileWriteThrough(sourcePath, targetPath, true)
}

func moveFileWriteThrough(sourcePath, targetPath string, replace bool) error {
	source, err := windows.UTF16PtrFromString(sourcePath)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		return err
	}
	flags := uint32(windows.MOVEFILE_WRITE_THROUGH)
	if replace {
		flags |= windows.MOVEFILE_REPLACE_EXISTING
	}
	return windows.MoveFileEx(source, target, flags)
}

// MoveFileEx with MOVEFILE_WRITE_THROUGH flushes the rename itself. Windows
// does not provide POSIX directory fsync semantics through os.File.
func syncParentDir(string) error { return nil }
