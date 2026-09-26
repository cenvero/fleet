// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build windows

package agent

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

const (
	defaultExecTimeoutWindows = 10 * time.Minute
	maxExecOutputBytesWindows = 16 << 20
)

type windowsCappedBuffer struct {
	bytes.Buffer
	truncated bool
}

func (b *windowsCappedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := maxExecOutputBytesWindows - b.Len()
	if remaining <= 0 {
		b.truncated = true
		return original, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, err := b.Buffer.Write(p)
	return original, err
}

func (b *windowsCappedBuffer) String() string {
	out := b.Buffer.String()
	if b.truncated {
		out += "\n[output truncated: exceeded 16 MiB limit]"
	}
	return out
}

func runShellExec(ctx context.Context, payload proto.ExecPayload) (proto.ExecResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, defaultExecTimeoutWindows)
	defer cancel()

	cmd := exec.Command("cmd.exe", "/C", payload.Command) // #nosec G204 -- authenticated shell.exec RPC intentionally executes the operator command
	var stdout, stderr windowsCappedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return proto.ExecResult{}, err
	}

	waitDone := make(chan struct{})
	var killed atomic.Bool
	go func() {
		select {
		case <-runCtx.Done():
			if cmd.Process != nil {
				killed.Store(true)
				// /T terminates descendants as well as the command shell. Ignore the
				// helper's output and then kill the root as a final fallback.
				_ = exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run() //nolint:gosec
				_ = cmd.Process.Kill()
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
	// taskkill /F leaves exit code 1, so a deadline kill is recognised from the
	// watchdog having fired on an expired deadline rather than from the code.
	timedOut := killed.Load() && errors.Is(runCtx.Err(), context.DeadlineExceeded)
	return proto.ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode, TimedOut: timedOut}, nil
}
