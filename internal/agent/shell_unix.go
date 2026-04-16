// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package agent

import (
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
)

// SSH wire-format structs for PTY and window-change channel requests (RFC 4254).
type ptyRequestPayload struct {
	Term     string
	Columns  uint32
	Rows     uint32
	Width    uint32
	Height   uint32
	Modelist string
}

type windowChangePayload struct {
	Columns uint32
	Rows    uint32
	Width   uint32
	Height  uint32
}

type exitStatusPayload struct {
	Status uint32
}

// serveShell handles a fleet-shell channel: allocates a PTY, starts the
// user's login shell, and proxies I/O between the channel and the PTY.
// Window resize requests are handled dynamically while the shell is running.
func serveShell(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()

	var (
		termType = "xterm-256color"
		cols     uint32 = 80
		rows     uint32 = 24
		hasPTY   bool
	)

	// First pass: collect pty-req before the shell request arrives.
	for req := range requests {
		switch req.Type {
		case "pty-req":
			var p ptyRequestPayload
			if err := ssh.Unmarshal(req.Payload, &p); err == nil {
				if p.Term != "" {
					termType = p.Term
				}
				if p.Columns > 0 {
					cols = p.Columns
				}
				if p.Rows > 0 {
					rows = p.Rows
				}
			}
			hasPTY = true
			if req.WantReply {
				_ = req.Reply(true, nil)
			}

		case "shell", "exec":
			shell := findShell()
			var cmd *exec.Cmd
			if req.Type == "exec" {
				var execReq struct{ Command string }
				_ = ssh.Unmarshal(req.Payload, &execReq)
				cmd = exec.Command("/bin/sh", "-c", execReq.Command) //nolint:gosec
			} else {
				cmd = exec.Command(shell, "-l")
			}
			// Use a clean server-side environment — do NOT inherit os.Environ()
		// which would be the controller machine's env (wrong HOME, PATH, etc.).
		cmd.Env = []string{
			"TERM=" + termType,
			"SHELL=" + cmd.Path,
			"LANG=en_US.UTF-8",
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		}
		// Inherit HOME and USER from the agent process (runs as the deploy user/root).
		for _, key := range []string{"HOME", "USER", "LOGNAME"} {
			if val := os.Getenv(key); val != "" {
				cmd.Env = append(cmd.Env, key+"="+val)
			}
		}

			if hasPTY {
				runShellWithPTY(channel, requests, cmd, cols, rows)
			} else {
				runShellDirect(channel, requests, cmd)
			}
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
			return

		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

func runShellWithPTY(channel ssh.Channel, requests <-chan *ssh.Request, cmd *exec.Cmd, cols, rows uint32) {
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	})
	if err != nil {
		sendExitStatus(channel, 1)
		return
	}
	defer ptmx.Close()

	// Handle window-change requests while the shell is running.
	go func() {
		for req := range requests {
			if req.Type == "window-change" {
				var p windowChangePayload
				if err := ssh.Unmarshal(req.Payload, &p); err == nil {
					_ = pty.Setsize(ptmx, &pty.Winsize{
						Rows: uint16(p.Rows),
						Cols: uint16(p.Columns),
					})
				}
			}
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(ptmx, channel) // controller → shell stdin
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(channel, ptmx) // shell stdout/stderr → controller
	}()

	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	wg.Wait()
	sendExitStatus(channel, exitCode)
}

func runShellDirect(channel ssh.Channel, requests <-chan *ssh.Request, cmd *exec.Cmd) {
	cmd.Stdin = channel
	cmd.Stdout = channel
	cmd.Stderr = channel.Stderr()
	if err := cmd.Start(); err != nil {
		sendExitStatus(channel, 1)
		return
	}
	go func() {
		for req := range requests {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}()
	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	sendExitStatus(channel, exitCode)
}

func sendExitStatus(channel ssh.Channel, code int) {
	payload := ssh.Marshal(exitStatusPayload{Status: uint32(code)})
	_, _ = channel.SendRequest("exit-status", false, payload)
}

func findShell() string {
	for _, sh := range []string{"/bin/bash", "/bin/zsh", "/bin/sh"} {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	return "/bin/sh"
}
