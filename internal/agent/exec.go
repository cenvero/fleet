// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package agent

import (
	"context"
	"os/exec"
	"syscall"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

// defaultExecTimeout bounds how long a single shell.exec may run. An
// authenticated controller could otherwise launch a command that never exits
// (e.g. `sleep infinity`, a fork bomb, or a hung network read) and pin agent
// resources indefinitely. ExecPayload has no timeout field today, so this is the
// effective limit; if a Timeout is ever added to the payload, honor it (capped)
// before falling back to this default.
const defaultExecTimeout = 600 * time.Second

// maxExecOutputBytes caps how much stdout (and, separately, stderr) is buffered
// in memory for one command. A command that streams gigabytes to stdout must not
// be able to exhaust the agent's memory; output past the cap is dropped and a
// marker is appended so the caller knows truncation happened.
const maxExecOutputBytes = 16 << 20 // 16 MiB

// truncationMarker is appended to a capped stream so the controller can tell the
// output was cut rather than ending naturally.
const truncationMarker = "\n[output truncated: exceeded 16 MiB limit]"

// cappedBuffer is an io.Writer that retains at most max bytes. Once full it
// discards further writes (but keeps counting via truncated) so a runaway
// command cannot grow the agent's heap without bound. It is written to only from
// the single goroutine exec.Cmd uses per stream, so it needs no locking.
type cappedBuffer struct {
	max       int
	buf       []byte
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := c.max - len(c.buf); remaining > 0 {
		take := len(p)
		if take > remaining {
			take = remaining
		}
		c.buf = append(c.buf, p[:take]...)
	}
	if len(c.buf) >= c.max && len(p) > 0 {
		c.truncated = true
	}
	// Always report the full length written: returning short would make exec.Cmd
	// treat it as a write error (io.ErrShortWrite) and tear the command down.
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	if c.truncated {
		return string(c.buf) + truncationMarker
	}
	return string(c.buf)
}

func runShellExec(ctx context.Context, payload proto.ExecPayload) (proto.ExecResult, error) {
	// Derive a timeout-bounded context so a runaway command is killed. We manage
	// the kill ourselves (process group) rather than relying on
	// CommandContext's single-process kill, which would leave the child's own
	// children (e.g. a backgrounded `sleep`) orphaned and still holding resources.
	runCtx, cancel := context.WithTimeout(ctx, defaultExecTimeout)
	defer cancel()

	cmd := exec.Command("/bin/sh", "-c", payload.Command) //nolint:gosec
	// Setpgid puts the shell and everything it spawns in a new process group so we
	// can signal the whole tree at once on timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout := &cappedBuffer{max: maxExecOutputBytes}
	stderr := &cappedBuffer{max: maxExecOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return proto.ExecResult{}, err
	}

	// Watchdog: on timeout (or caller cancellation) kill the whole process group.
	// SIGKILL to -pgid reaches every descendant, defeating attempts to survive by
	// double-forking. waitDone stops the watchdog once the command exits normally.
	waitDone := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			if cmd.Process != nil {
				// Negative pid => signal the entire process group.
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case <-waitDone:
		}
	}()

	err := cmd.Wait()
	close(waitDone)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return proto.ExecResult{}, err
		}
	}
	return proto.ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, nil
}
