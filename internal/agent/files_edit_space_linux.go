// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build linux

package agent

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// reserveFileSpace allocates n bytes of disk space after offset off in f
// without changing its size, so an in-place write that grows the file cannot
// run out of space halfway. Filesystems without fallocate skip the
// reservation.
func reserveFileSpace(f *os.File, off, n int64) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) {
		ferr = unix.Fallocate(int(fd), unix.FALLOC_FL_KEEP_SIZE, off, n) // #nosec G115 -- a file descriptor fits in int
	}); err != nil {
		return err
	}
	if errors.Is(ferr, unix.EOPNOTSUPP) || errors.Is(ferr, unix.ENOSYS) {
		return nil
	}
	return ferr
}
