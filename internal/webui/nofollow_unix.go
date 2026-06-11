// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build unix

package webui

import "syscall"

// oNoFollow makes a local write/upload open refuse to follow a symlinked final
// component (fails with ELOOP), so a symlink at the destination can't redirect a
// clobber outside the intended path. A no-op on platforms without O_NOFOLLOW.
const oNoFollow = syscall.O_NOFOLLOW
