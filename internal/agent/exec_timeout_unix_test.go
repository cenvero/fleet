// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package agent

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
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

// execHelperEnv makes the test binary act as a helper child for the tests
// below instead of running tests (see TestExecHelperProcess).
const execHelperEnv = "FLEET_AGENT_EXEC_TEST_HELPER"

// TestExecHelperProcess is not a test. Started by a shell.exec with
// execHelperEnv set, it leaves the command's process group with setsid(2), so
// the deadline kill of that group cannot reach it, prints its PID, and holds
// stdout open far past any deadline used here.
func TestExecHelperProcess(t *testing.T) {
	if os.Getenv(execHelperEnv) != "setsid-hold-stdout" {
		return
	}
	if _, err := syscall.Setsid(); err != nil {
		fmt.Fprintln(os.Stderr, "setsid:", err)
		os.Exit(2)
	}
	fmt.Println(os.Getpid())
	time.Sleep(20 * time.Second)
	os.Exit(0)
}

// killPrintedPID kills the process whose PID is the first line of stdout: a
// leftover the command started, which the exec deliberately left behind.
func killPrintedPID(t *testing.T, stdout string) {
	t.Helper()
	line, _, _ := strings.Cut(stdout, "\n")
	if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && pid > 1 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// TestRunShellExecBackgroundChildHoldingStdout: a command that backgrounds a
// child which keeps stdout open (`sleep 30 &`) returns soon after the shell
// exits, with the shell's own status. It used to block until the deadline (by
// default 10 minutes), holding one of the exec slots.
func TestRunShellExecBackgroundChildHoldingStdout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	res, err := runShellExec(ctx, proto.ExecPayload{Command: "sleep 30 & echo $!"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	killPrintedPID(t, res.Stdout)
	if elapsed > execWaitDelay+3*time.Second {
		t.Fatalf("returned after %s: waited on the background child instead of the shell", elapsed)
	}
	if res.ExitCode != 0 || res.TimedOut {
		t.Fatalf("result = %+v, want exit 0 and not timed out", res)
	}
	if strings.TrimSpace(res.Stdout) == "" {
		t.Fatal("output written before the shell exited was lost")
	}
}

// TestRunShellExecDeadlineWhileBackgroundChildHoldsStdout: when the deadline
// fires while the reply is still waiting on a background child's output, the
// deadline is what ended the command, so it is reported as timed out even
// though the shell itself exited 0. It used to say exit 0, not timed out.
func TestRunShellExecDeadlineWhileBackgroundChildHoldsStdout(t *testing.T) {
	t.Parallel()
	if execWaitDelay <= 300*time.Millisecond {
		t.Skip("needs a deadline shorter than execWaitDelay")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, err := runShellExec(ctx, proto.ExecPayload{Command: "sleep 30 & echo $!"})
	if err != nil {
		t.Fatal(err)
	}
	killPrintedPID(t, res.Stdout)
	if !res.TimedOut {
		t.Fatalf("result = %+v, want timed out", res)
	}
}

// TestRunShellExecDeadlineWithEscapedChild: a child that left the process
// group (so the deadline kill cannot reach it) and kept stdout open must not
// hold the reply past the deadline. Wait used to block until the child exited.
func TestRunShellExecDeadlineWithEscapedChild(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const deadline = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	// The trailing echo keeps the shell from exec-ing the helper in place:
	// the helper must be the shell's child, not the process group leader, for
	// setsid to succeed.
	command := execHelperEnv + "=setsid-hold-stdout " + shellQuote(exe) + " -test.run='^TestExecHelperProcess$'; echo unreachable"
	start := time.Now()
	res, err := runShellExec(ctx, proto.ExecPayload{Command: command})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	killPrintedPID(t, res.Stdout)
	if elapsed > deadline+execWaitDelay+3*time.Second {
		t.Fatalf("returned %s after start with a %s deadline: held by the escaped child", elapsed, deadline)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("result = %+v, want timed out with exit -1", res)
	}
	if _, err := strconv.Atoi(strings.TrimSpace(res.Stdout)); err != nil {
		t.Fatalf("helper did not report its PID before the deadline (stdout %q, stderr %q); the escape was not exercised", res.Stdout, res.Stderr)
	}
}
