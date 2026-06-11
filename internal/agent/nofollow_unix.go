// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build unix

package agent

import "syscall"

// oNoFollow makes an OpenFile refuse to follow a symlinked final component
// (fails with ELOOP), closing the TOCTOU window between validateWriteTarget's
// Lstat check and the actual open where a local process could swap the target
// for a symlink pointing outside --file-root.
const oNoFollow = syscall.O_NOFOLLOW
