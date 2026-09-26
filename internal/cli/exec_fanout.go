// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

// execDefaultParallel is how many servers `fleet exec --all/--group` runs on at
// once unless --parallel says otherwise. It stays below the daemon's
// control-socket limit (32) so reverse-mode fan-outs are never dropped.
const execDefaultParallel = 16

// Status values for execJSON targets where the command did not run.
const (
	execStatusBlocked = "blocked" // refused by cmd-policy / --guard / confirm (see "error")
	execStatusStaged  = "staged"  // --require-approval staged it (see "approval_id")
	execStatusDryRun  = "dry-run" // --dry-run (see "command")
	execStatusCached  = "cached"  // --idempotency-key replayed an earlier result
)

// agentDefaultExecTimeout mirrors the agent's own cap on a single shell.exec
// (internal/agent defaultExecTimeout), used to recognise that limit being hit by
// older agents that do not report timed_out themselves.
const agentDefaultExecTimeout = 600 * time.Second

// execTimeoutGrace bounds how close to the --timeout deadline a bare exit -1 or a
// transport error must land to be classed as that deadline firing. The agent
// kills the command at the same deadline (sent to it truncated to milliseconds)
// and its reply can win the race against the controller's own timer.
const execTimeoutGrace = time.Second

// exitProcess terminates the process with a remote exit code (--propagate-exit).
// A variable so tests can observe the code instead of exiting.
var exitProcess = os.Exit

// execWriters are the streams one exec target writes to: out/err for the remote
// output and headers, note for human notes (stdout normally, stderr in --json
// mode so stdout stays pure JSON).
type execWriters struct {
	out, err, note io.Writer
}

// execCapture records one target's writes (stdout and stderr, in order) so a
// target executed concurrently can be replayed verbatim, in target order, once
// every earlier target is done. Only the target's own goroutine writes to it.
type execCapture struct {
	chunks []execChunk
}

type execChunk struct {
	toErr bool
	data  []byte
}

type execCaptureWriter struct {
	c     *execCapture
	toErr bool
}

func (w execCaptureWriter) Write(p []byte) (int, error) {
	if n := len(w.c.chunks); n > 0 && w.c.chunks[n-1].toErr == w.toErr {
		w.c.chunks[n-1].data = append(w.c.chunks[n-1].data, p...)
	} else {
		w.c.chunks = append(w.c.chunks, execChunk{toErr: w.toErr, data: append([]byte(nil), p...)})
	}
	return len(p), nil
}

func (c *execCapture) writers(jsonMode bool) execWriters {
	return execWriters{
		out:  execCaptureWriter{c: c},
		err:  execCaptureWriter{c: c, toErr: true},
		note: execCaptureWriter{c: c, toErr: jsonMode},
	}
}

func (c *execCapture) replay(out, errw io.Writer) {
	for _, chunk := range c.chunks {
		if chunk.toErr {
			_, _ = errw.Write(chunk.data)
		} else {
			_, _ = out.Write(chunk.data)
		}
	}
}

// fanoutExitStatus applies single-server exec's exit rules to every target of
// `exec --all/--group`, which used to exit 0 no matter what happened:
//
//   - with --propagate-exit, the first non-zero REMOTE exit code in target order
//     (a command that ran and exited non-zero) is returned as code;
//   - otherwise, in human mode, any failed target — blocked by policy,
//     unreachable, timed out, or a non-zero exit — yields an error (exit 1);
//   - in --json mode (which never errors on a remote failure, as for one server)
//     only a policy/guard/confirm block yields an error.
func fanoutExitStatus(results []execJSON, skipped []bool, errs []error, asJSON, propagate bool) (int, error) {
	if propagate {
		for i, r := range results {
			if !skipped[i] && r.AgentError == "" && !r.TimedOut && r.ExitCode != 0 {
				return r.ExitCode, nil
			}
		}
	}
	var failed []string
	for i, r := range results {
		switch {
		case errs[i] != nil:
			failed = append(failed, r.Server)
		case asJSON || skipped[i]:
		case r.AgentError != "" || r.TimedOut || r.ExitCode != 0:
			failed = append(failed, r.Server)
		}
	}
	if len(failed) == 0 {
		return 0, nil
	}
	verb := "failed"
	if asJSON {
		verb = "was blocked"
	}
	return 0, fmt.Errorf("command %s on %d of %d server(s): %s", verb, len(failed), len(results), strings.Join(failed, ", "))
}

// execTimedOut reports whether an exec bounded by --timeout ended because of that
// deadline, whichever side noticed first: the controller's context, the agent's
// own deadline kill (reported as timed_out by current agents, or as a bare
// signal exit -1 by older ones), or the transport giving up at the deadline (a
// reverse call's i/o timeout). An operator interrupt is never a timeout.
func execTimedOut(execCtx, parent context.Context, timeout time.Duration, deadline, returned time.Time, r proto.ExecResult, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return true
	}
	if parent.Err() != nil {
		return false
	}
	// Scale the window down for short timeouts so a fast genuine failure (e.g. a
	// refused connection) is never mistaken for the deadline.
	grace := execTimeoutGrace
	if timeout/10 < grace {
		grace = timeout / 10
	}
	nearDeadline := !returned.Before(deadline.Add(-grace))
	if err == nil {
		return r.TimedOut || (r.ExitCode == -1 && nearDeadline)
	}
	return nearDeadline
}

// agentDefaultLimitHit reports whether an exec without --timeout was killed by
// the agent's own default command limit.
func agentDefaultLimitHit(r proto.ExecResult, elapsed time.Duration) bool {
	return r.TimedOut || (r.ExitCode == -1 && elapsed >= agentDefaultExecTimeout-execTimeoutGrace)
}

// printExecHuman renders a single exec result in human mode, mirroring the
// original --all output format. printHeader adds the "=== server [status] ==="
// banner (used for multi-server output); single-server output omits it.
func printExecHuman(out, errw io.Writer, j execJSON, printHeader bool) {
	if printHeader {
		switch {
		case j.AgentError != "":
			fmt.Fprintf(out, "=== %s [%s] ===\n%s\n", j.Server, classifyAgentErrorStr(j.AgentError), j.AgentError)
			return
		case j.TimedOut:
			fmt.Fprintf(out, "=== %s [timed out] ===\n", j.Server)
		default:
			fmt.Fprintf(out, "=== %s [exit %d] ===\n", j.Server, j.ExitCode)
		}
	}
	if j.Stdout != "" {
		fmt.Fprint(out, j.Stdout)
	}
	if j.Stderr != "" {
		fmt.Fprint(errw, j.Stderr)
	}
}

// redactedError replaces an error's text (e.g. with secret values scrubbed)
// while keeping it unwrappable, so errors.Is(err, context.DeadlineExceeded)
// and friends still work.
type redactedError struct {
	msg string
	err error
}

func (e redactedError) Error() string { return e.msg }
func (e redactedError) Unwrap() error { return e.err }
