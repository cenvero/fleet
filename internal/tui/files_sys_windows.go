// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build windows

package tui

import (
	"os"

	"golang.org/x/sys/windows"
)

// localDiskFree reports the bytes available to the caller on the volume
// holding path.
func localDiskFree(path string) (int64, bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, false
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, false
	}
	if avail > 1<<62 {
		return 0, false
	}
	return int64(avail), true
}

// fileOwner is not shown on Windows (ownership is an ACL, not a uid/gid pair).
func fileOwner(os.FileInfo) string { return "" }
