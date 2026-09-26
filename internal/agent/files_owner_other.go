// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !unix

package agent

import "os"

const oNonBlock = 0

// privateToAgent cannot establish who owns a file or how many names it has on
// this platform, so a leftover upload sidecar is never trusted: an upload
// always starts in a fresh temp file (resume within one agent process still
// works through the in-memory upload state).
func privateToAgent(os.FileInfo) bool { return false }

// sameInode is not checked here: file identities are not guaranteed on every
// filesystem these platforms mount, and a false negative would block every
// upload from being installed.
func sameInode(os.FileInfo, os.FileInfo) bool { return true }

// exclusivelyOwned has no ownership or link count to inspect here; the temp
// file was created exclusively by this process, so it is accepted.
func exclusivelyOwned(info os.FileInfo) bool { return info.Mode().IsRegular() }
