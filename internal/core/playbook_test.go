// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"strings"
	"testing"
)

// fakeExec records every (server, command) call in order and returns a result
// chosen by the supplied responder. It is the test double for ExecFn.
type fakeExec struct {
	calls   []string
	respond func(server, command string) (int, string, string, error)
}

func (f *fakeExec) fn() ExecFn {
	return func(server, command string) (int, string, string, error) {
		f.calls = append(f.calls, command)
		if f.respond != nil {
			return f.respond(server, command)
		}
		return 0, "", "", nil
	}
}

func samplePlaybook() Playbook {
	return Playbook{
		Name: "setup",
		Steps: []PlaybookStep{
			{Name: "step1", Apply: "apply1", Rollback: "rollback1"},
			{Name: "step2", Apply: "apply2", Rollback: "rollback2"},
			{Name: "step3", Apply: "apply3", Rollback: "rollback3"},
		},
	}
}

// statusFor returns the recorded status of a named step on the first server.
func statusFor(t *testing.T, res PlaybookResult, server, step string) string {
	t.Helper()
	for _, sr := range res.Servers {
		if sr.Server != server {
			continue
		}
		for _, s := range sr.Steps {
			if s.Name == step {
				return s.Status
			}
		}
	}
	t.Fatalf("step %q for server %q not found", step, server)
	return ""
}

// TestRollbackRunsAppliedStepsInReverse: when a later step fails and
// OnFailRollback is set, the rollback commands of the previously-applied steps
// run in REVERSE order, and those steps are recorded as rolledback.
func TestRollbackRunsAppliedStepsInReverse(t *testing.T) {
	fe := &fakeExec{
		respond: func(_, command string) (int, string, string, error) {
			if command == "apply3" {
				return 1, "", "boom", nil // step3 fails
			}
			return 0, "", "", nil
		},
	}

	res := RunPlaybook(fe.fn(), samplePlaybook(), []string{"web-01"}, RunOptions{OnFailRollback: true})

	if !res.Failed() {
		t.Fatalf("expected the playbook to fail")
	}

	// Expected call order: apply1, apply2, apply3 (fails),
	// then rollbacks in reverse of applied (step2, step1).
	want := []string{"apply1", "apply2", "apply3", "rollback2", "rollback1"}
	if strings.Join(fe.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("call order = %v, want %v", fe.calls, want)
	}

	if got := statusFor(t, res, "web-01", "step1"); got != StepRolledBack {
		t.Errorf("step1 status = %q, want %q", got, StepRolledBack)
	}
	if got := statusFor(t, res, "web-01", "step2"); got != StepRolledBack {
		t.Errorf("step2 status = %q, want %q", got, StepRolledBack)
	}
	if got := statusFor(t, res, "web-01", "step3"); got != StepFailed {
		t.Errorf("step3 status = %q, want %q", got, StepFailed)
	}
}

// TestRollbackSkippedWhenNoRollbackCommand: an applied step with an empty
// rollback command is NOT claimed as rolled back; it is recorded as
// rollback-skipped and no undo command runs for it.
func TestRollbackSkippedWhenNoRollbackCommand(t *testing.T) {
	pb := Playbook{
		Name: "partial-rollback",
		Steps: []PlaybookStep{
			{Name: "step1", Apply: "apply1"},                        // no rollback
			{Name: "step2", Apply: "apply2", Rollback: "rollback2"}, // has rollback
			{Name: "step3", Apply: "apply3", Rollback: "rollback3"},
		},
	}
	fe := &fakeExec{
		respond: func(_, command string) (int, string, string, error) {
			if command == "apply3" {
				return 1, "", "boom", nil // step3 fails
			}
			return 0, "", "", nil
		},
	}

	res := RunPlaybook(fe.fn(), pb, []string{"web-01"}, RunOptions{OnFailRollback: true})

	// Only step2 has a rollback command; step1 must not run any undo.
	want := []string{"apply1", "apply2", "apply3", "rollback2"}
	if strings.Join(fe.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("call order = %v, want %v", fe.calls, want)
	}

	if got := statusFor(t, res, "web-01", "step1"); got != StepRollbackSkipped {
		t.Errorf("step1 status = %q, want %q", got, StepRollbackSkipped)
	}
	if got := statusFor(t, res, "web-01", "step2"); got != StepRolledBack {
		t.Errorf("step2 status = %q, want %q", got, StepRolledBack)
	}
}

// TestRollbackPreservesApplyFailureOutput: when a later step fails and rollback
// runs, the failed step keeps its original apply ExitCode/Stderr, and a
// rolled-back step keeps its apply result while its rollback outcome is recorded
// separately in StepResult.Rollback.
func TestRollbackPreservesApplyFailureOutput(t *testing.T) {
	fe := &fakeExec{
		respond: func(_, command string) (int, string, string, error) {
			switch command {
			case "apply1":
				return 0, "applied-one", "", nil
			case "apply3":
				return 3, "", "apply3-broke", nil // step3 fails
			case "rollback1":
				return 9, "", "undo-failed", nil // rollback itself fails
			default:
				return 0, "", "", nil
			}
		},
	}

	res := RunPlaybook(fe.fn(), samplePlaybook(), []string{"web-01"}, RunOptions{OnFailRollback: true})

	// The failed step must preserve WHY apply failed.
	var failed, rolled StepResult
	for _, sr := range res.Servers {
		for _, s := range sr.Steps {
			if s.Name == "step3" {
				failed = s
			}
			if s.Name == "step1" {
				rolled = s
			}
		}
	}

	if failed.Status != StepFailed {
		t.Fatalf("step3 status = %q, want %q", failed.Status, StepFailed)
	}
	if failed.ExitCode != 3 || strings.TrimSpace(failed.Stderr) != "apply3-broke" {
		t.Errorf("step3 apply result not preserved: exit=%d stderr=%q", failed.ExitCode, failed.Stderr)
	}

	// step1's apply output must survive the rollback overwrite bug.
	if rolled.Status != StepRolledBack {
		t.Fatalf("step1 status = %q, want %q", rolled.Status, StepRolledBack)
	}
	if rolled.Stdout != "applied-one" {
		t.Errorf("step1 apply stdout = %q, want %q (rollback must not overwrite it)", rolled.Stdout, "applied-one")
	}
	if rolled.Rollback == nil {
		t.Fatalf("step1 RollbackResult missing")
	}
	if rolled.Rollback.ExitCode != 9 || strings.TrimSpace(rolled.Rollback.Stderr) != "undo-failed" {
		t.Errorf("step1 rollback result = %+v, want exit 9 / stderr undo-failed", rolled.Rollback)
	}
}

// TestNoRollbackWhenDisabled: a failed step with OnFailRollback off stops the
// server but runs no rollback commands.
func TestNoRollbackWhenDisabled(t *testing.T) {
	fe := &fakeExec{
		respond: func(_, command string) (int, string, string, error) {
			if command == "apply2" {
				return 7, "", "nope", nil
			}
			return 0, "", "", nil
		},
	}

	res := RunPlaybook(fe.fn(), samplePlaybook(), []string{"web-01"}, RunOptions{OnFailRollback: false})

	want := []string{"apply1", "apply2"}
	if strings.Join(fe.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("call order = %v, want %v (no rollback expected)", fe.calls, want)
	}
	if got := statusFor(t, res, "web-01", "step1"); got != StepApplied {
		t.Errorf("step1 status = %q, want %q", got, StepApplied)
	}
	if got := statusFor(t, res, "web-01", "step3"); got != StepSkipped {
		t.Errorf("step3 status = %q, want %q", got, StepSkipped)
	}
}

// TestCheckExitZeroSkipsApply: a check that exits 0 marks the step satisfied
// and never runs apply.
func TestCheckExitZeroSkipsApply(t *testing.T) {
	pb := Playbook{
		Name: "idempotent",
		Steps: []PlaybookStep{
			{Name: "installed", Check: "check1", Apply: "apply1"},
			{Name: "second", Apply: "apply2"},
		},
	}
	fe := &fakeExec{
		respond: func(_, command string) (int, string, string, error) {
			if command == "check1" {
				return 0, "", "", nil // already satisfied
			}
			return 0, "", "", nil
		},
	}

	res := RunPlaybook(fe.fn(), pb, []string{"web-01"}, RunOptions{})

	// apply1 must NOT have run; check1 then apply2 only.
	want := []string{"check1", "apply2"}
	if strings.Join(fe.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("call order = %v, want %v", fe.calls, want)
	}
	if got := statusFor(t, res, "web-01", "installed"); got != StepSatisfied {
		t.Errorf("installed status = %q, want %q", got, StepSatisfied)
	}
	if got := statusFor(t, res, "web-01", "second"); got != StepApplied {
		t.Errorf("second status = %q, want %q", got, StepApplied)
	}
}

// TestCheckNonZeroRunsApply: a failing check causes apply to run.
func TestCheckNonZeroRunsApply(t *testing.T) {
	pb := Playbook{
		Name: "needs-apply",
		Steps: []PlaybookStep{
			{Name: "installed", Check: "check1", Apply: "apply1"},
		},
	}
	fe := &fakeExec{
		respond: func(_, command string) (int, string, string, error) {
			if command == "check1" {
				return 1, "", "", nil // not satisfied
			}
			return 0, "", "", nil
		},
	}

	res := RunPlaybook(fe.fn(), pb, []string{"web-01"}, RunOptions{})

	want := []string{"check1", "apply1"}
	if strings.Join(fe.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("call order = %v, want %v", fe.calls, want)
	}
	if got := statusFor(t, res, "web-01", "installed"); got != StepApplied {
		t.Errorf("installed status = %q, want %q", got, StepApplied)
	}
}

// TestDryRunRunsNothing: dry-run produces a plan but never calls exec.
func TestDryRunRunsNothing(t *testing.T) {
	fe := &fakeExec{
		respond: func(_, _ string) (int, string, string, error) {
			t.Fatalf("exec must not be called during a dry run")
			return 0, "", "", nil
		},
	}

	res := RunPlaybook(fe.fn(), samplePlaybook(), []string{"web-01", "web-02"}, RunOptions{DryRun: true})

	if len(fe.calls) != 0 {
		t.Fatalf("expected zero exec calls, got %v", fe.calls)
	}
	if !res.DryRun {
		t.Errorf("result.DryRun = false, want true")
	}
	if len(res.Servers) != 2 {
		t.Fatalf("expected 2 server results, got %d", len(res.Servers))
	}
	for _, sr := range res.Servers {
		for _, s := range sr.Steps {
			if s.Status != StepSkipped {
				t.Errorf("dry-run step %q status = %q, want %q", s.Name, s.Status, StepSkipped)
			}
		}
	}
}

// TestTransportErrorIsFailure: an ExecFn error (not just non-zero exit) marks
// the step failed and triggers rollback when enabled.
func TestTransportErrorIsFailure(t *testing.T) {
	fe := &fakeExec{
		respond: func(_, command string) (int, string, string, error) {
			if command == "apply2" {
				return 0, "", "", errDial
			}
			return 0, "", "", nil
		},
	}

	res := RunPlaybook(fe.fn(), samplePlaybook(), []string{"web-01"}, RunOptions{OnFailRollback: true})

	if !res.Failed() {
		t.Fatalf("expected failure on transport error")
	}
	want := []string{"apply1", "apply2", "rollback1"}
	if strings.Join(fe.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("call order = %v, want %v", fe.calls, want)
	}
	if got := statusFor(t, res, "web-01", "step2"); got != StepFailed {
		t.Errorf("step2 status = %q, want %q", got, StepFailed)
	}
}

// errDial is a sentinel transport error for TestTransportErrorIsFailure.
var errDial = &dialError{}

type dialError struct{}

func (*dialError) Error() string { return "dial failed" }

// TestParsePlaybookValidation covers required-field checks.
func TestParsePlaybookValidation(t *testing.T) {
	cases := map[string]string{
		"missing name":  "steps:\n  - name: a\n    apply: x\n",
		"no steps":      "name: p\n",
		"step no name":  "name: p\nsteps:\n  - apply: x\n",
		"step no apply": "name: p\nsteps:\n  - name: a\n",
	}
	for label, doc := range cases {
		if _, err := ParsePlaybook([]byte(doc)); err == nil {
			t.Errorf("%s: expected a validation error", label)
		}
	}

	good := "name: p\nhosts: role=plesk\nsteps:\n  - name: a\n    check: c\n    apply: x\n    rollback: r\n"
	pb, err := ParsePlaybook([]byte(good))
	if err != nil {
		t.Fatalf("valid playbook rejected: %v", err)
	}
	if pb.Hosts != "role=plesk" || len(pb.Steps) != 1 || pb.Steps[0].Rollback != "r" {
		t.Fatalf("parsed playbook unexpected: %+v", pb)
	}
}

// TestResolveTargetsPrecedence: explicit server beats group beats hosts; an
// unresolvable target is an error.
func TestResolveTargetsPrecedence(t *testing.T) {
	pb := Playbook{Name: "p", Hosts: "role=db", Steps: []PlaybookStep{{Name: "a", Apply: "x"}}}

	// Explicit server wins, no tag store needed.
	got, err := ResolveTargets(pb, "web-01", "role=web", []string{"web-01"}, nil)
	if err != nil || len(got) != 1 || got[0] != "web-01" {
		t.Fatalf("explicit target = %v, err = %v", got, err)
	}

	// No target at all => error.
	empty := Playbook{Name: "p", Steps: []PlaybookStep{{Name: "a", Apply: "x"}}}
	if _, err := ResolveTargets(empty, "", "", nil, nil); err == nil {
		t.Errorf("expected error when no target can be resolved")
	}

	// An explicit server that is not a known server => clear early error.
	_, err = ResolveTargets(pb, "web-99", "", []string{"web-01", "web-02"}, nil)
	if err == nil {
		t.Fatalf("expected an error for an unknown explicit server")
	}
	if !strings.Contains(err.Error(), "unknown server") || !strings.Contains(err.Error(), "web-99") {
		t.Errorf("error = %v, want it to name the unknown server", err)
	}
}
