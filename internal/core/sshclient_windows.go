// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build windows

package core

import (
	"os"

	"golang.org/x/crypto/ssh"
)

func stdinFd() int {
	return int(os.Stdin.Fd()) //#nosec G115 -- fd fits in int on all supported platforms
}

// watchWindowResize is a no-op on Windows — SIGWINCH does not exist.
func watchWindowResize(_ ssh.Channel, _ int, done <-chan struct{}) {
	<-done
}
