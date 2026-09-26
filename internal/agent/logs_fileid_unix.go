// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build unix

package agent

import (
	"fmt"
	"os"
	"syscall"
)

// logFileID identifies a file by device and inode so a log cursor notices when
// the path now names a different file (rotation by rename, or replacement).
func logFileID(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}
