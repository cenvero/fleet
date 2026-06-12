// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build unix

package core

import "syscall"

// oNoFollow makes an OpenFile refuse to follow a symlinked final component
// (the open fails with ELOOP). When extracting an archive into an
// operator-chosen destination, a pre-planted symlink at a member's target path
// would otherwise be followed and the member bytes written through it, outside
// the destination directory. Opening with O_NOFOLLOW closes that hole.
const oNoFollow = syscall.O_NOFOLLOW
