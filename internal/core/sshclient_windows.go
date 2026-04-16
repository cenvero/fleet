// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build windows

package core

import (
	"os"

	"golang.org/x/crypto/ssh"
)

func stdinFd() int {
	return int(os.Stdin.Fd()) //nolint:gosec
}

// watchWindowResize is a no-op on Windows — SIGWINCH does not exist.
func watchWindowResize(_ *ssh.Session, _ int, done <-chan struct{}) {
	<-done
}
