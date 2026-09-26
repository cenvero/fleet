// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package logs

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryAuditFileLock takes a non-blocking exclusive flock on file. flock locks
// belong to the open file description, so two descriptors of the same file —
// in different processes or in the same one — exclude each other.
func tryAuditFileLock(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return false, err
}
