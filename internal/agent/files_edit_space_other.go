// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !linux

package agent

import "os"

// reserveFileSpace is a no-op where fallocate is not available; an in-place
// write that runs out of space writes the original back instead.
func reserveFileSpace(*os.File, int64, int64) error { return nil }
