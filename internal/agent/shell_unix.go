// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package agent

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
)

// SSH wire-format structs for channel requests (RFC 4254).
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

// serveShell handles a fleet-shell channel exactly like OpenSSH sshd does:
//
//   - "pty-req"       → allocate PTY, record terminal type and size
//   - "env"           → accept per-session env vars (like AcceptEnv in sshd_config)
//   - "shell"         → start a login shell (argv[0] = "-bash") with SSH_TTY set
//   - "exec"          → run a command through the user's own shell
//   - "window-change" → resize the PTY while the shell is running
func serveShell(channel ssh.Channel, requests <-chan *ssh.Request, sessionID string) {
	defer channel.Close()

	var (
		termType        = "xterm-256color"
		cols     uint32 = 80
		rows     uint32 = 24
		hasPTY   bool
		extraEnv []string // from "env" channel requests
	)

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
			replyReq(req, true)

		case "env":
			// Accept env vars from the client — same as AcceptEnv in sshd_config.
			// Block loader/dynamic-linker and shell-init variables that could be
			// used for privilege escalation even inside an authenticated session.
			var p struct{ Name, Value string }
			if err := ssh.Unmarshal(req.Payload, &p); err == nil && p.Name != "" && !isDangerousEnvVar(p.Name) {
				extraEnv = append(extraEnv, p.Name+"="+p.Value)
			}
			replyReq(req, true)

		case "shell", "exec":
			shellPath := userShell()
			env := buildEnv(termType, shellPath, extraEnv)

			if req.Type == "exec" {
				// Non-persistent: exec requests run a one-shot command.
				var execReq struct{ Command string }
				_ = ssh.Unmarshal(req.Payload, &execReq)
				cmd := exec.Command(shellPath, "-c", execReq.Command) //nolint:gosec
				cmd.Env = env
				replyReq(req, true)
				if hasPTY {
					runWithPTY(channel, requests, cmd, cols, rows)
				} else {
					runDirect(channel, requests, cmd)
				}
				return
			}

			// Interactive shell — use the persistent session store.
			// If a session already exists (e.g. after a network drop), attach to it.
			// If not, create a new one. The session survives disconnects for up to
			// sessionIdleTimeout (10 min) before the shell is sent SIGHUP.
			replyReq(req, true)
			runPersistentShell(channel, requests, shellPath, env, cols, rows, sessionID)
			return

		default:
			replyReq(req, false)
		}
	}
}

// buildEnv builds the session environment identically to OpenSSH:
// a clean PATH, locale, TERM, SHELL, user identity from /etc/passwd,
// and MAIL. The controller's own environment is never passed through.
func buildEnv(termType, shellPath string, extra []string) []string {
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8",
		"TERM=" + termType,
		"SHELL=" + shellPath,
	}
	if u, err := user.Current(); err == nil {
		env = append(env,
			"USER="+u.Username,
			"LOGNAME="+u.Username,
			"HOME="+u.HomeDir,
			fmt.Sprintf("MAIL=/var/mail/%s", u.Username),
		)
	}
	return append(env, extra...)
}

// runPersistentShell gets or creates a persistent shell session and attaches
// the current channel to it. On network drops the shell keeps running and the
// replay buffer accumulates output. On reconnect the buffer is sent first so
// the user sees what happened while disconnected.
func runPersistentShell(channel ssh.Channel, requests <-chan *ssh.Request, shellPath string, env []string, cols, rows uint32, sessionID string) {
	session, isNew, err := globalStore.getOrCreate(sessionID, func() (*persistentSession, error) {
		ptm, pts, err := pty.Open()
		if err != nil {
			return nil, err
		}
		_ = pty.Setsize(ptm, ptyWinsize(rows, cols))

		cmd := exec.Command(shellPath)
		cmd.Args = []string{"-" + filepath.Base(shellPath)}
		cmd.Env = append(env, "SSH_TTY="+pts.Name())
		cmd.Stdin = pts
		cmd.Stdout = pts
		cmd.Stderr = pts
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setsid:  true,
			Setctty: true,
			Ctty:    1,
		}
		if err := cmd.Start(); err != nil {
			_ = pts.Close()
			_ = ptm.Close()
			return nil, err
		}
		_ = pts.Close()

		// Reap the process once it exits (keeps the PTY output loop from leaking).
		go func() {
			_ = cmd.Wait()
		}()

		return &persistentSession{
			ptm:    ptm,
			cmd:    cmd,
			replay: &replayBuffer{},
			done:   make(chan struct{}),
		}, nil
	})
	if err != nil {
		sendExitStatus(channel, 1)
		return
	}

	_ = isNew // both paths use the same attach flow

	// Wire the channel to the (new or existing) session.
	// detached is closed when the channel's input goroutine exits (network drop
	// or clean disconnect). Using the returned signal avoids a second competing
	// reader on the same channel — which would steal bytes including Ctrl+C.
	detached := session.attach(channel, cols, rows, globalStore, sessionID)

	// Handle window-change requests while this channel is connected.
	// This reads from `requests`, not from `channel`, so no race.
	go func() {
		for req := range requests {
			if req.Type == "window-change" {
				var p windowChangePayload
				if err := ssh.Unmarshal(req.Payload, &p); err == nil {
					_ = pty.Setsize(session.ptm, ptyWinsize(p.Rows, p.Columns))
				}
			}
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		}
	}()

	// Block until the shell exits or the channel disconnects.
	select {
	case <-session.done:
		// Shell exited cleanly (user typed exit / process died).
		sendExitStatus(channel, 0)
		_ = channel.CloseWrite()
	case <-detached:
		// Channel closed — network drop or deliberate disconnect.
		// Shell keeps running; idle timer started inside detach().
	}
}

// runWithPTY allocates a PTY pair, starts the shell with the slave as its
// controlling terminal (new session, Setsid+Setctty), sets SSH_TTY, and
// proxies I/O. Shutdown sequence mirrors OpenSSH sshd:
//
//  1. Shell exits → close ptm (unblocks I/O goroutines)
//  2. Send exit-status channel request
//  3. CloseWrite (EOF) → controller closes the channel
//  4. Wait for I/O goroutines to drain
func runWithPTY(channel ssh.Channel, requests <-chan *ssh.Request, cmd *exec.Cmd, cols, rows uint32) {
	ptm, pts, err := pty.Open()
	if err != nil {
		sendExitStatus(channel, 1)
		return
	}
	_ = pty.Setsize(ptm, ptyWinsize(rows, cols))

	// SSH_TTY = slave PTY device path (e.g. /dev/pts/3).
	// bash, zsh, and tools like script(1) use this to detect a real terminal.
	cmd.Env = append(cmd.Env, "SSH_TTY="+pts.Name())
	cmd.Stdin = pts
	cmd.Stdout = pts
	cmd.Stderr = pts
	// Setsid: new session (detach from agent's controlling terminal)
	// Setctty + Ctty:1: make the slave PTY the controlling terminal
	// Ctty:1 = stdout fd in child — matches what creack/pty uses internally
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    1,
	}

	if err := cmd.Start(); err != nil {
		_ = pts.Close()
		_ = ptm.Close()
		sendExitStatus(channel, 1)
		return
	}
	_ = pts.Close() // parent closes slave after fork — shell holds its own ref

	// Handle window-change requests while the shell is running.
	go func() {
		for req := range requests {
			if req.Type == "window-change" {
				var p windowChangePayload
				if err := ssh.Unmarshal(req.Payload, &p); err == nil {
					_ = pty.Setsize(ptm, ptyWinsize(p.Rows, p.Columns))
				}
			}
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(ptm, channel) }() // controller → shell stdin
	go func() { defer wg.Done(); _, _ = io.Copy(channel, ptm) }() // shell stdout/stderr → controller

	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}

	// Step 1: close ptm — causes io.Copy(channel, ptm) to get EIO and return,
	// and causes io.Copy(ptm, channel) to fail on the next Write attempt.
	_ = ptm.Close()

	// Step 2: send exit-status so the controller knows the exit code.
	sendExitStatus(channel, exitCode)

	// Step 3: CloseWrite signals EOF to the controller's stdout reader.
	// The controller then calls channel.Close() which finally unblocks
	// io.Copy(ptm, channel) that may be stuck in channel.Read().
	_ = channel.CloseWrite()

	// Step 4: wait for both I/O goroutines to finish cleanly.
	wg.Wait()
}

// runDirect handles sessions without a PTY — non-interactive commands,
// piped scripts, etc. stdin/stdout/stderr connect directly to the channel.
func runDirect(channel ssh.Channel, requests <-chan *ssh.Request, cmd *exec.Cmd) {
	cmd.Stdin = channel
	cmd.Stdout = channel
	cmd.Stderr = channel.Stderr()
	if err := cmd.Start(); err != nil {
		sendExitStatus(channel, 1)
		return
	}
	// Drain any stray channel requests (e.g. env sent after shell start).
	go func() {
		for req := range requests {
			replyReq(req, false)
		}
	}()

	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	sendExitStatus(channel, exitCode)
	_ = channel.CloseWrite()
}

func sendExitStatus(channel ssh.Channel, code int) {
	payload := ssh.Marshal(exitStatusPayload{Status: sshExitStatusValue(code)})
	_, _ = channel.SendRequest("exit-status", false, payload)
}

func replyReq(req *ssh.Request, ok bool) {
	if req.WantReply {
		_ = req.Reply(ok, nil)
	}
}

// userShell returns the login shell for the current user from /etc/passwd.
// Falls back through bash → zsh → sh if the entry is missing.
func userShell() string {
	if u, err := user.Current(); err == nil {
		if sh := shellFromPasswd(u.Username); sh != "" {
			return sh
		}
	}
	for _, sh := range []string{"/bin/bash", "/bin/zsh", "/bin/sh"} {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	return "/bin/sh"
}

// isDangerousEnvVar returns true for environment variable names that could be
// exploited to hijack the shell or the dynamic linker even when the connection
// is fully authenticated. These are blocked regardless of the client request.
//
// Blocked categories:
//   - Dynamic-linker preload/path vars  (LD_*, DYLD_*) → code injection via shared library
//   - Shell initialisation vars (BASH_ENV, ENV, ZDOTDIR) → arbitrary code on shell start
//   - Interpreter startup vars → code injection in Python/Ruby/Node/Java/Perl sessions
//   - HOME / SHELL → would cause wrong .bashrc / wrong shell to load
func isDangerousEnvVar(name string) bool {
	upper := strings.ToUpper(name)
	// Prefix-based blocks
	for _, prefix := range []string{"LD_", "DYLD_"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	// Exact-name blocks
	switch upper {
	case "BASH_ENV", "ENV", "ZDOTDIR",
		"PROMPT_COMMAND", "PS1", "PS2", "PS4",
		"PYTHONSTARTUP", "PYTHONPATH",
		"RUBYOPT", "RUBYLIB",
		"PERL5OPT", "PERL5LIB",
		"NODE_OPTIONS", "NODE_PATH",
		"JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS",
		"HOME", "SHELL", "USER", "LOGNAME", "PATH":
		return true
	}
	return false
}

func shellFromPasswd(username string) string {
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return ""
	}
	prefix := username + ":"
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.SplitN(line, ":", 7)
		if len(fields) == 7 {
			if sh := strings.TrimSpace(fields[6]); sh != "" {
				return sh
			}
		}
	}
	return ""
}
