// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build unix

package agent

import (
	"os"
	"syscall"
)

// oNonBlock keeps opening a leftover upload sidecar from blocking if a local
// user swapped a FIFO in at its name. It has no effect on regular files.
const oNonBlock = syscall.O_NONBLOCK

// privateToAgent reports whether info (from fstat of an open descriptor)
// describes a regular file only this agent can have written: owned by the
// agent's effective uid, not hard-linked anywhere else, and with no permission
// bits beyond 0600. Anything else may have been planted or be shared with
// another local user, so it must never be trusted.
func privateToAgent(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() {
		return false
	}
	return int64(st.Uid) == int64(os.Geteuid()) && st.Nlink == 1 && info.Mode().Perm()&^0o600 == 0
}

// sameInode reports whether two FileInfos describe the same file.
func sameInode(a, b os.FileInfo) bool { return os.SameFile(a, b) }

// exclusivelyOwned is the check made just before an upload is installed: the
// temp file is still owned by the agent and has no second name through which
// another local user could keep reaching the installed file. Its permission
// bits are not checked because finalize has already applied the final mode.
func exclusivelyOwned(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() {
		return false
	}
	return int64(st.Uid) == int64(os.Geteuid()) && st.Nlink == 1
}
