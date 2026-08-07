// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	advisoryLockRetry   = 10 * time.Millisecond
	advisoryLockTimeout = 10 * time.Second
)

// withAdvisoryFileLock serializes fn across processes using an advisory lock
// owned by an open file descriptor/handle. Closing that descriptor releases the
// lock, and the kernel also closes it automatically if the process exits or is
// killed. The sidecar file is intentionally retained: unlinking a lock file can
// let a new process lock a different inode while an existing holder still owns
// the old one.
//
// Acquisition is bounded and fails closed; fn is never called unless the lock
// was acquired successfully.
func withAdvisoryFileLock(path string, fn func() error) (retErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- callers provide fixed store sidecar paths
	if err != nil {
		return fmt.Errorf("open advisory lock %s: %w", path, err)
	}
	defer func() {
		if closeErr := file.Close(); retErr == nil && closeErr != nil {
			retErr = fmt.Errorf("release advisory lock %s: %w", path, closeErr)
		}
	}()

	deadline := time.Now().Add(advisoryLockTimeout)
	for {
		acquired, lockErr := tryAdvisoryFileLock(file)
		if lockErr != nil {
			return fmt.Errorf("acquire advisory lock %s: %w", path, lockErr)
		}
		if acquired {
			return fn()
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("acquire advisory lock: timed out waiting for %s", path)
		}
		time.Sleep(advisoryLockRetry)
	}
}
