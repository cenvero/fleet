// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package core

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func stdinFd() int {
	return int(os.Stdin.Fd()) //#nosec G115 -- fd fits in int on all supported platforms
}

// watchWindowResize listens for SIGWINCH and sends window-change channel
// requests to the fleet agent so the remote PTY stays in sync with the
// local terminal size.
func watchWindowResize(channel ssh.Channel, fd int, done <-chan struct{}) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)
	for {
		select {
		case <-done:
			return
		case <-sigCh:
			w, h, err := term.GetSize(fd)
			if err == nil {
				payload := ssh.Marshal(windowChangePayload{
					Columns: uint32(w),
					Rows:    uint32(h),
				})
				_, _ = channel.SendRequest("window-change", false, payload)
			}
		}
	}
}
