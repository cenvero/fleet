// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !linux && !darwin && !windows

package tui

import "os"

// localDiskFree is unavailable on this platform.
func localDiskFree(string) (int64, bool) { return 0, false }

// fileOwner is unavailable on this platform.
func fileOwner(os.FileInfo) string { return "" }
