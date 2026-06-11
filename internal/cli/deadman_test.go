// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

// recordingExec is a fake guardExec that records the (server, command) pairs it
// is asked to run and returns a canned result.
type recordingExec struct {
	calls  []execCall
	result proto.ExecResult
	err    error
}

type execCall struct {
	server  string
	command string
}

func (r *recordingExec) ExecCommand(server, command string) (proto.ExecResult, error) {
	r.calls = append(r.calls, execCall{server: server, command: command})
	return r.result, r.err
}

func TestArmGuardBuildsAndRuns(t *testing.T) {
	fake := &recordingExec{result: proto.ExecResult{Stdout: "ok", ExitCode: 0}}
	res, err := armGuard(fake, "web-01", "web-01-1", "ufw enable", "ufw disable", 120)
	if err != nil {
		t.Fatalf("armGuard: %v", err)
	}
	if res.Stdout != "ok" {
		t.Fatalf("expected canned result, got %+v", res)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("expected exactly 1 exec call, got %d", len(fake.calls))
	}
	call := fake.calls[0]
	if call.server != "web-01" {
		t.Fatalf("expected server web-01, got %q", call.server)
	}
	cmd := call.command
	// The risky command must run, the revert script must be written, and a
	// detached revert must be scheduled with the derived delay and unit.
	for _, want := range []string{
		"/run/fleet-guard-web-01-1.sh",
		"systemd-run --on-active=",
		"--unit='fleet-guard-web-01-1'",
		"setsid sh -c",
		"'ufw enable'", // risky command, shell-quoted
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("arm command missing %q\n---\n%s", want, cmd)
		}
	}
	// The delay (120s) must appear quoted for systemd-run.
	if !strings.Contains(cmd, "'120'") {
		t.Errorf("expected quoted 120s delay in arm command:\n%s", cmd)
	}
}

func TestArmGuardRejectsZeroDelay(t *testing.T) {
	fake := &recordingExec{}
	if _, err := armGuard(fake, "web-01", "web-01-1", "ufw enable", "ufw disable", 0); err == nil {
		t.Fatal("expected error for non-positive delay")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("exec should not run when building fails, got %d calls", len(fake.calls))
	}
}

func TestArmGuardRejectsBadID(t *testing.T) {
	fake := &recordingExec{}
	if _, err := armGuard(fake, "web-01", "bad id with spaces", "x", "y", 10); err == nil {
		t.Fatal("expected error for invalid guard id")
	}
}

func TestConfirmGuardTouchesSentinelAndStopsTimer(t *testing.T) {
	fake := &recordingExec{}
	if _, err := confirmGuard(fake, "web-01", "web-01-2"); err != nil {
		t.Fatalf("confirmGuard: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("expected 1 exec call, got %d", len(fake.calls))
	}
	cmd := fake.calls[0].command
	for _, want := range []string{
		"touch '/run/fleet-guard-web-01-2.ok'",
		"systemctl stop 'fleet-guard-web-01-2.timer'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("confirm command missing %q\n---\n%s", want, cmd)
		}
	}
}

func TestRevertGuardRunsScriptWithFallback(t *testing.T) {
	fake := &recordingExec{}
	if _, err := revertGuard(fake, "web-01", "web-01-3", "ufw disable"); err != nil {
		t.Fatalf("revertGuard: %v", err)
	}
	cmd := fake.calls[0].command
	for _, want := range []string{
		"touch '/run/fleet-guard-web-01-3.ok'",
		"sh '/run/fleet-guard-web-01-3.sh'",
		"sh -c 'ufw disable'", // stored fallback, shell-quoted
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("revert command missing %q\n---\n%s", want, cmd)
		}
	}
}

func TestRevertGuardWithoutFallbackRunsScriptOnly(t *testing.T) {
	fake := &recordingExec{}
	if _, err := revertGuard(fake, "web-01", "web-01-4", ""); err != nil {
		t.Fatalf("revertGuard: %v", err)
	}
	cmd := fake.calls[0].command
	if !strings.Contains(cmd, "sh '/run/fleet-guard-web-01-4.sh'") {
		t.Errorf("expected script run, got:\n%s", cmd)
	}
	if strings.Contains(cmd, "sh -c") {
		t.Errorf("did not expect a fallback branch with no stored revert cmd:\n%s", cmd)
	}
}

// TestGuardQuotingResistsInjection ensures the risky command is passed as quoted
// data on the `sh -c` line (never breaking out into the arm shell), and that the
// revert script is written through a quoted heredoc so its body is inert at write
// time. The heredoc body is intentionally verbatim — it is the operator's own
// revert command, executed later as the undo action — so we assert the *boundary*
// is safe (escaped sh -c, quoted heredoc delimiter), not that the body is escaped.
func TestGuardQuotingResistsInjection(t *testing.T) {
	fake := &recordingExec{}
	evil := "x'; rm -rf / #"
	if _, err := armGuard(fake, "web-01", "web-01-5", evil, evil, 30); err != nil {
		t.Fatalf("armGuard: %v", err)
	}
	cmd := fake.calls[0].command
	// The risky command on the sh -c line must be shell-quoted, so the injection
	// payload appears in its escaped form, never as a bare break-out.
	if !strings.Contains(cmd, `sh -c 'x'"'"'; rm -rf / #'`) {
		t.Errorf("risky command not safely quoted on sh -c line:\n%s", cmd)
	}
	// The heredoc that writes the revert script must use a QUOTED delimiter, which
	// disables all expansion of the body at write time.
	if !strings.Contains(cmd, "<<'FLEET_GUARD_EOF'") {
		t.Errorf("expected quoted heredoc delimiter to inert the body:\n%s", cmd)
	}
}

// TestArmGuardRejectsDelimiterCollision proves a revert command containing a line
// equal to the heredoc delimiter is rejected, keeping the heredoc unbreakable.
func TestArmGuardRejectsDelimiterCollision(t *testing.T) {
	fake := &recordingExec{}
	if _, err := armGuard(fake, "web-01", "web-01-7", "x", "echo hi\nFLEET_GUARD_EOF\nrm -rf /", 30); err == nil {
		t.Fatal("expected rejection of a revert script containing the heredoc delimiter")
	}
}

func TestArmGuardPropagatesExecError(t *testing.T) {
	fake := &recordingExec{err: fmt.Errorf("boom")}
	if _, err := armGuard(fake, "web-01", "web-01-6", "x", "y", 10); err == nil {
		t.Fatal("expected exec error to propagate")
	}
}
