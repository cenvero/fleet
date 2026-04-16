// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package agent

import (
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
//   - "pty-req" → allocate PTY, record terminal type and size
//   - "env"     → accept per-session env vars (like AcceptEnv in sshd_config)
//   - "shell"   → start a login shell (argv[0] = "-bash") with SSH_TTY set
//   - "exec"    → run a command through the user's shell
//   - "window-change" → resize the PTY while the shell is running
func serveShell(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()

	var (
		termType = "xterm-256color"
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
			// Accept env vars set by the client before the shell starts.
			// Real sshd honours AcceptEnv; we accept all of them since the
			// connection is already fully authenticated.
			var p struct{ Name, Value string }
			if err := ssh.Unmarshal(req.Payload, &p); err == nil && p.Name != "" {
				extraEnv = append(extraEnv, p.Name+"="+p.Value)
			}
			replyReq(req, true)

		case "shell", "exec":
			shell := userShell()
			env := buildEnv(termType, shell, extraEnv)

			var cmd *exec.Cmd
			if req.Type == "exec" {
				// fleet exec / ssh host cmd — run through the user's own shell
				var execReq struct{ Command string }
				_ = ssh.Unmarshal(req.Payload, &execReq)
				cmd = exec.Command(shell, "-c", execReq.Command) //nolint:gosec
				cmd.Args[0] = shell
			} else {
				// Login shell: argv[0] starts with "-" to signal login to bash/zsh/etc.
				// This is what OpenSSH does — NOT "bash -l".
				cmd = exec.Command(shell)
				cmd.Args = []string{"-" + filepath.Base(shell)}
			}
			cmd.Env = env

			replyReq(req, true)
			if hasPTY {
				runWithPTY(channel, requests, cmd, cols, rows)
			} else {
				runDirect(channel, requests, cmd)
			}
			return

		default:
			replyReq(req, false)
		}
	}
}

// buildEnv constructs a clean, server-side environment identical to what
// OpenSSH sets for a session: user info from /etc/passwd, a sane PATH,
// LANG, TERM, SHELL, and any extra vars from "env" channel requests.
// The controller's environment is never passed through.
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
		)
	}
	return append(env, extra...)
}

// runWithPTY allocates a PTY pair, starts the shell with the slave as its
// controlling terminal, sets SSH_TTY, and proxies I/O. This is the path
// taken for interactive sessions (pty-req was sent).
func runWithPTY(channel ssh.Channel, requests <-chan *ssh.Request, cmd *exec.Cmd, cols, rows uint32) {
	ptm, pts, err := pty.Open()
	if err != nil {
		sendExitStatus(channel, 1)
		return
	}
	_ = pty.Setsize(ptm, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})

	// SSH_TTY = slave PTY path (e.g. /dev/pts/3) — bash uses this for
	// "is a terminal" checks, and tools like script(1) look for it.
	cmd.Env = append(cmd.Env, "SSH_TTY="+pts.Name())
	cmd.Stdin = pts
	cmd.Stdout = pts
	cmd.Stderr = pts
	// New session + controlling terminal — identical to what sshd does.
	// Ctty:1 matches what creack/pty uses internally (stdout fd in child).
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
	_ = pts.Close() // parent doesn't need the slave after fork
	defer ptm.Close()

	// Process window-change requests while the shell is running.
	go func() {
		for req := range requests {
			if req.Type == "window-change" {
				var p windowChangePayload
				if err := ssh.Unmarshal(req.Payload, &p); err == nil {
					_ = pty.Setsize(ptm, &pty.Winsize{
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
	go func() { defer wg.Done(); _, _ = io.Copy(ptm, channel) }()
	go func() { defer wg.Done(); _, _ = io.Copy(channel, ptm) }()

	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	wg.Wait()
	sendExitStatus(channel, exitCode)
}

// runDirect handles sessions without a PTY (e.g. fleet exec, scp, rsync).
func runDirect(channel ssh.Channel, requests <-chan *ssh.Request, cmd *exec.Cmd) {
	cmd.Stdin = channel
	cmd.Stdout = channel
	cmd.Stderr = channel.Stderr()
	if err := cmd.Start(); err != nil {
		sendExitStatus(channel, 1)
		return
	}
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
}

func sendExitStatus(channel ssh.Channel, code int) {
	payload := ssh.Marshal(exitStatusPayload{Status: uint32(code)})
	_, _ = channel.SendRequest("exit-status", false, payload)
}

func replyReq(req *ssh.Request, ok bool) {
	if req.WantReply {
		_ = req.Reply(ok, nil)
	}
}

// userShell returns the login shell for the current user by reading /etc/passwd.
// Falls back to /bin/bash or /bin/sh if the entry is missing or unreadable.
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
			sh := strings.TrimSpace(fields[6])
			if sh != "" {
				return sh
			}
		}
	}
	return ""
}
