// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package agent

import (
	"math"

	"github.com/creack/pty"
)

// PTY winsizes are uint16 on Unix kernels, so clamp oversized client values
// rather than letting them wrap during conversion.
func ptyWinsize(rows, cols uint32) *pty.Winsize {
	return &pty.Winsize{
		Rows: clampPTYDimension(rows),
		Cols: clampPTYDimension(cols),
	}
}

func clampPTYDimension(size uint32) uint16 {
	if size > math.MaxUint16 {
		return uint16(math.MaxUint16)
	}
	return uint16(size)
}

// SSH exit-status is an unsigned 32-bit integer. Negative process exit codes
// represent "no numeric exit status" on Unix, so send the conventional 255.
func sshExitStatusValue(code int) uint32 {
	if code < 0 {
		return 255
	}
	if uint64(code) > math.MaxUint32 {
		return uint32(math.MaxUint32)
	}
	return uint32(code)
}
