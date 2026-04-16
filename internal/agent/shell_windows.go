// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build windows

package agent

import "golang.org/x/crypto/ssh"

// serveShell is not supported on Windows — the fleet agent targets Linux/Unix.
func serveShell(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for req := range requests {
		if req.WantReply {
			_ = req.Reply(false, nil)
		}
	}
}
