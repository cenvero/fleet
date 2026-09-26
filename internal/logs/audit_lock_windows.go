// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build windows

package logs

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryAuditFileLock takes a non-blocking exclusive byte-range lock on the
// sidecar lock file. The lock lives on the sidecar (never the audit log itself),
// so Windows' mandatory range-lock semantics cannot block readers of the log.
func tryAuditFileLock(file *os.File) (bool, error) {
	overlapped := new(windows.Overlapped)
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		overlapped,
	)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}
