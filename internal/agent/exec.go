// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
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

// execWaitDelay bounds how long Wait keeps collecting a command's output after
// the shell has exited, whether on its own or killed at the deadline. A process
// the command started can keep stdout or stderr open: a plain `server &`, or a
// child that left the process group with setsid, which the deadline kill of
// that group cannot reach. Wait used to block until that process exited,
// holding the reply (well past the deadline) and one of the exec slots. After
// the delay the pipes are closed and the command is reported as it stands.
//
// The delay must stay well inside the 5 seconds a controller waits for the
// reply after it cancels a request, or the controller gives up the channel.
const execWaitDelay = 2 * time.Second

// maxExecOutputBytes caps how much stdout (and, separately, stderr) is buffered
// in memory for one command. A command that streams gigabytes to stdout must not
// be able to exhaust the agent's memory; output past the cap is dropped and
// truncationMarker is appended so the caller knows truncation happened. The
// reply carrying both streams has the same 16 MiB limit, so execResponse may
// trim them further.
const maxExecOutputBytes = 16 << 20 // 16 MiB

// maxConcurrentExecs bounds how many shell.exec commands may run at once across
// the whole agent. Each in-flight exec buffers up to maxExecOutputBytes for
// stdout AND stderr (≈32 MiB worst case), so without an aggregate cap the
// 128-channel limit lets a peer pin ≈4 GiB of heap. Capping concurrency to a
// small number bounds total exec output memory to maxConcurrentExecs * 2 *
// maxExecOutputBytes (≈256 MiB here). Excess execs queue on execSlots rather
// than failing — the command still runs, just after a slot frees — so existing
// behavior (every exec eventually runs and returns its output) is preserved.
const maxConcurrentExecs = 8

// execSlots is a counting semaphore: a token is acquired before an exec runs and
// released when it returns, bounding aggregate exec output buffers process-wide.
var execSlots = make(chan struct{}, maxConcurrentExecs)

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
	// Acquire a concurrency slot before allocating output buffers so aggregate
	// exec memory across all channels stays bounded (see execSlots). Prefer a
	// non-blocking acquire so an immediately-available slot proceeds straight to
	// the normal run path (preserving existing behavior, including the watchdog
	// killing a command whose context is already cancelled). Only when all slots
	// are in use do we wait — and there, bail out on cancellation so a cancelled
	// request doesn't queue behind a backlog.
	select {
	case execSlots <- struct{}{}:
		defer func() { <-execSlots }()
	default:
		select {
		case execSlots <- struct{}{}:
			defer func() { <-execSlots }()
		case <-ctx.Done():
			return proto.ExecResult{}, ctx.Err()
		}
	}

	// Derive a timeout-bounded context so a runaway command is killed. We manage
	// the kill ourselves (process group) rather than relying on
	// CommandContext's single-process kill, which would leave the child's own
	// children (e.g. a backgrounded `sleep`) orphaned and still holding resources.
	runCtx, cancel := context.WithTimeout(ctx, defaultExecTimeout)
	defer cancel()

	cmd := exec.Command("/bin/sh", "-c", payload.Command) // #nosec G204 -- authenticated shell.exec RPC intentionally executes the operator command
	// Setpgid puts the shell and everything it spawns in a new process group so we
	// can signal the whole tree at once on timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = execWaitDelay

	if len(payload.Env) > 0 {
		env, err := execEnvironment(payload.Env)
		if err != nil {
			return proto.ExecResult{}, err
		}
		cmd.Env = env
	}

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
	//
	// mu orders the kill against Wait returning, so the result knows whether the
	// kill happened while Wait was still pending.
	waitDone := make(chan struct{})
	var (
		mu          sync.Mutex
		finished    bool // Wait has returned
		killed      bool // the watchdog killed the group while Wait was pending
		shellExited bool // ...and the shell itself had already exited by then
	)
	go func() {
		select {
		case <-runCtx.Done():
			mu.Lock()
			defer mu.Unlock()
			if finished || cmd.Process == nil {
				return
			}
			// A shell that has exited and been reaped reports ErrProcessDone;
			// one still running, or exiting right now, does not.
			shellExited = errors.Is(cmd.Process.Signal(syscall.Signal(0)), os.ErrProcessDone)
			killed = true
			// Negative pid => signal the entire process group.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-waitDone:
		}
	}()

	err := cmd.Wait()
	mu.Lock()
	finished = true
	wasKilled, shellHadExited := killed, shellExited
	mu.Unlock()
	close(waitDone)

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		switch {
		case errors.As(err, &exitErr):
			exitCode = exitErr.ExitCode()
		case errors.Is(err, exec.ErrWaitDelay):
			// The shell exited 0, but something it started still held stdout or
			// stderr open after execWaitDelay, so the pipes were closed. The
			// shell's own result stands.
		default:
			return proto.ExecResult{}, err
		}
	}
	return proto.ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		TimedOut: execKilledByDeadline(runCtx, wasKilled, shellHadExited, exitCode),
	}, nil
}

// execKilledByDeadline reports whether the command was ended by its deadline
// (the request's, or defaultExecTimeout), as opposed to a caller cancellation
// or a command that finished on its own. The watchdog must have killed the
// process group while Wait was still pending, on an expired deadline, and
// either the shell died of that kill (exit -1) or it had already exited and the
// reply was still waiting on output a leftover process held open. A shell that
// exits on its own as the timer fires keeps its own exit status and is not a
// timeout.
func execKilledByDeadline(runCtx context.Context, killed, shellHadExited bool, exitCode int) bool {
	if !killed || !errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return false
	}
	return exitCode == -1 || shellHadExited
}
