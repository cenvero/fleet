// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

// TestCappedBufferTruncates proves the per-stream memory cap: writes past the
// limit are dropped and the truncation marker is appended, so a command that
// floods stdout/stderr cannot grow the agent's heap without bound.
func TestCappedBufferTruncates(t *testing.T) {
	t.Parallel()
	c := &cappedBuffer{max: 8}
	// Write more than the cap across multiple calls.
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte(" world, this is way over")); err != nil {
		t.Fatal(err)
	}
	got := c.String()
	if !strings.HasPrefix(got, "hello wo") {
		t.Fatalf("expected retained bytes capped at 8 (%q), got %q", "hello wo", got)
	}
	if !strings.Contains(got, "output truncated") {
		t.Fatalf("expected a truncation marker, got %q", got)
	}
	// The retained data (excluding the marker) must never exceed max bytes.
	retained := strings.TrimSuffix(got, truncationMarker)
	if len(retained) != 8 {
		t.Fatalf("retained %d bytes, want exactly cap=8", len(retained))
	}
}

// TestCappedBufferUnderLimit confirms output below the cap is returned verbatim
// with no marker.
func TestCappedBufferUnderLimit(t *testing.T) {
	t.Parallel()
	c := &cappedBuffer{max: maxExecOutputBytes}
	if _, err := c.Write([]byte("short output")); err != nil {
		t.Fatal(err)
	}
	if got := c.String(); got != "short output" {
		t.Fatalf("expected verbatim output, got %q", got)
	}
}

// TestRunShellExecBasic confirms the hardened exec path still runs a normal
// command and captures stdout/exit code.
func TestRunShellExecBasic(t *testing.T) {
	t.Parallel()
	res, err := runShellExec(context.Background(), proto.ExecPayload{Command: "printf hi; exit 3"})
	if err != nil {
		t.Fatalf("runShellExec: %v", err)
	}
	if res.Stdout != "hi" {
		t.Fatalf("stdout = %q, want %q", res.Stdout, "hi")
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", res.ExitCode)
	}
}

// TestRunShellExecKilledOnCancel proves the watchdog tears down a running
// command (and its process group) when the caller's context is cancelled,
// rather than blocking until the default timeout.
func TestRunShellExecKilledOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the command must be killed promptly
	res, err := runShellExec(ctx, proto.ExecPayload{Command: "sleep 30"})
	if err != nil {
		t.Fatalf("runShellExec should return after kill, got err %v", err)
	}
	// A killed process exits non-zero (negative exit codes surface as -1 here).
	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit for a killed command, got 0")
	}
}
