// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package logs

import (
	"fmt"
	"os"
	"time"
)

const (
	auditLockRetry   = 5 * time.Millisecond
	auditLockTimeout = 10 * time.Second
)

// withAuditFileLock serializes fn across processes with an advisory lock held on
// an open descriptor of the sidecar at path (flock on Unix, LockFileEx on
// Windows). Closing the descriptor releases the lock, and the kernel releases it
// if the process dies, so a crashed writer can never wedge the log. The sidecar
// is intentionally never removed: unlinking it would let a new process lock a
// different inode while an existing holder still owns the old one.
//
// Acquisition is bounded and fails closed: fn only runs once the lock is held.
func withAuditFileLock(path string, fn func() error) (retErr error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- fixed sidecar next to the audit log
	if err != nil {
		return fmt.Errorf("open audit lock: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); retErr == nil && closeErr != nil {
			retErr = fmt.Errorf("release audit lock: %w", closeErr)
		}
	}()

	deadline := time.Now().Add(auditLockTimeout)
	for {
		acquired, lockErr := tryAuditFileLock(file)
		if lockErr != nil {
			return fmt.Errorf("acquire audit lock: %w", lockErr)
		}
		if acquired {
			return fn()
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("acquire audit lock: timed out waiting for %s", path)
		}
		time.Sleep(auditLockRetry)
	}
}
