// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

// TestRunShellExecMarksDeadlineKill: a command killed because its deadline
// expired is reported as timed_out (exit -1 alone was indistinguishable from any
// other signal death, so controllers raced to guess).
func TestRunShellExecMarksDeadlineKill(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := runShellExec(ctx, proto.ExecPayload{Command: "sleep 5"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("deadline kill = %+v, want timed_out with exit -1", res)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("command outlived its deadline: %s", time.Since(start))
	}
}

// TestRunShellExecCancelIsNotTimeout: a caller cancellation (not a deadline)
// kills the command but is not a timeout; a normal exit never is.
func TestRunShellExecCancelIsNotTimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	res, err := runShellExec(ctx, proto.ExecPayload{Command: "sleep 5"})
	if err != nil {
		t.Fatal(err)
	}
	if res.TimedOut {
		t.Fatalf("cancellation reported as timeout: %+v", res)
	}

	res, err = runShellExec(context.Background(), proto.ExecPayload{Command: "exit 2"})
	if err != nil || res.TimedOut || res.ExitCode != 2 {
		t.Fatalf("normal exit = %+v, %v", res, err)
	}
	// A command that kills itself with a signal is not a timeout either.
	res, err = runShellExec(context.Background(), proto.ExecPayload{Command: "kill -9 $$"})
	if err != nil || res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("self-kill = %+v, %v", res, err)
	}
}
