// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/core"
)

// TestRequireApprovalStagesExecOptions: `exec --require-approval` records the
// exec options the command must run with once approved (secrets only as
// references), instead of just the bare command.
func TestRequireApprovalStagesExecOptions(t *testing.T) {
	dir, fake := setupExecFanout(t, map[string]fakeExecBehavior{"srv-01": {}}, nil)
	if err := core.NewSecretStore(dir).Set("api_key", "s3cr3t-value"); err != nil {
		t.Fatal(err)
	}

	res := runExecFleet(t, dir, "exec", "srv-01", "--require-approval",
		"--timeout", "45s", "--retry", "2", "--backoff", "3s", "--guard", "--confirm",
		"--on-fail", "echo rollback", "--idempotency-key", "deploy-1",
		"--secret", "API_KEY=@api_key", "--", "./deploy.sh --fast")
	if res.err != nil {
		t.Fatalf("stage: %v (stderr=%q)", res.err, res.stderr)
	}
	if fake.calls.Load() != 0 {
		t.Fatalf("a staged command must not run; agent saw %d call(s)", fake.calls.Load())
	}
	list, err := core.NewApprovalStore(dir).List()
	if err != nil || len(list) != 1 {
		t.Fatalf("approvals = %v, %v; want exactly one", list, err)
	}
	got := list[0]
	if got.Status != core.ApprovalPending || got.Server != "srv-01" || got.Command != "./deploy.sh --fast" {
		t.Fatalf("staged approval = %+v", got)
	}
	want := &core.ApprovalExec{
		Timeout: "45s", Retry: 2, Backoff: "3s", Guard: true, Confirm: true,
		OnFail: "echo rollback", IdempotencyKey: "deploy-1", Secrets: []string{"API_KEY=@api_key"},
	}
	if !reflect.DeepEqual(got.Exec, want) {
		t.Fatalf("staged exec options = %+v, want %+v", got.Exec, want)
	}
	data, err := core.NewApprovalStore(dir).List()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range data {
		for _, s := range a.Exec.Secrets {
			if strings.Contains(s, "s3cr3t-value") {
				t.Fatalf("a secret VALUE was persisted in the approval queue: %q", s)
			}
		}
	}
}

// TestRequireApprovalRefusesLiteralSecret: a literal --secret value would be
// persisted in approvals.json, so staging it is refused before anything runs.
func TestRequireApprovalRefusesLiteralSecret(t *testing.T) {
	dir, fake := setupExecFanout(t, map[string]fakeExecBehavior{"srv-01": {}}, nil)
	res := runExecFleet(t, dir, "exec", "srv-01", "--require-approval", "--secret", "TOKEN=plaintext", "--", "true")
	if res.err == nil || !strings.Contains(res.err.Error(), "VAR=@name") {
		t.Fatalf("err = %v, want a refusal pointing at VAR=@name", res.err)
	}
	if list, _ := core.NewApprovalStore(dir).List(); len(list) != 0 {
		t.Fatalf("nothing may be staged with a literal secret, got %+v", list)
	}
	if fake.calls.Load() != 0 {
		t.Fatalf("agent saw %d call(s)", fake.calls.Load())
	}
}

// TestApprovedExecArgs pins the argv an approval runs with: the staged options,
// --propagate-exit for an exact recorded exit code, and the server and command
// after `--` so neither can ever be parsed as a flag. No token ever appears on
// the argv (it travels in FLEET_TOKEN).
func TestApprovedExecArgs(t *testing.T) {
	a := core.Approval{
		ID: "abc", Server: "web-01", Command: "--help; rm -rf ./cache",
		Exec: &core.ApprovalExec{Timeout: "30s", Retry: 1, Backoff: "5s", GuardWarn: true, Confirm: true,
			OnFail: "echo undo", IdempotencyKey: "k", Secrets: []string{"A=@a", "B=@b"}},
	}
	got := approvedExecArgs("/cfg", a, true)
	want := []string{"--config-dir", "/cfg", "exec", "--propagate-exit", "--json",
		"--timeout", "30s", "--retry", "1", "--backoff", "5s", "--guard-warn", "--confirm",
		"--on-fail", "echo undo", "--idempotency-key", "k", "--secret", "A=@a", "--secret", "B=@b",
		"--", "web-01", "--help; rm -rf ./cache"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv =\n  %q\nwant\n  %q", got, want)
	}

	// A legacy approval (staged by an older fleet, no options) runs with defaults.
	legacy := approvedExecArgs("", core.Approval{Server: "db-01", Command: "uptime"}, false)
	wantLegacy := []string{"exec", "--propagate-exit", "--", "db-01", "uptime"}
	if !reflect.DeepEqual(legacy, wantLegacy) {
		t.Fatalf("legacy argv = %q, want %q", legacy, wantLegacy)
	}

	env := approvedExecEnv([]string{"PATH=/bin", "FLEET_TOKEN=old"}, "tok-123")
	if !reflect.DeepEqual(env, []string{"PATH=/bin", "FLEET_TOKEN=tok-123"}) {
		t.Fatalf("child env = %q; the --token must replace FLEET_TOKEN", env)
	}
	if got := approvedExecEnv([]string{"PATH=/bin", "FLEET_TOKEN=inherited"}, ""); !reflect.DeepEqual(got, []string{"PATH=/bin", "FLEET_TOKEN=inherited"}) {
		t.Fatalf("without --token the environment must pass through, got %q", got)
	}
}

// TestRequireApprovalRefusesFlagLikeServer: `exec --require-approval -- --all
// uptime` must not stage a request whose "server" is --all.
func TestRequireApprovalRefusesFlagLikeServer(t *testing.T) {
	dir, fake := setupExecFanout(t, map[string]fakeExecBehavior{"srv-01": {}}, nil)
	res := runExecFleet(t, dir, "exec", "--require-approval", "--", "--all", "uptime")
	if res.err == nil {
		t.Fatalf("staging server --all must fail; stdout=%q stderr=%q", res.stdout, res.stderr)
	}
	if list, _ := core.NewApprovalStore(dir).List(); len(list) != 0 {
		t.Fatalf("nothing may be staged, got %+v", list)
	}
	if fake.calls.Load() != 0 {
		t.Fatalf("agent saw %d call(s)", fake.calls.Load())
	}
}

// TestApproveRunsAndRecordsOutcome drives the real binary: `fleet approve`
// approves the request, runs it through `fleet exec`, and records the outcome
// exactly once. The target server does not exist, so the run fails — which is
// what must be recorded (and a second approve must be refused).
func TestApproveRunsAndRecordsOutcome(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess approval test skipped in -short mode")
	}
	bin := buildFleetBinary(t)
	dir := initConfigDir(t, bin)

	store := core.NewApprovalStore(dir)
	id, err := store.StageExec("no-such-server", "uptime", time.Hour, &core.ApprovalExec{Timeout: "5s"}, "tester")
	if err != nil {
		t.Fatal(err)
	}

	// Without a terminal and without --yes nothing is approved: the full
	// request is shown and the approval stays pending.
	noYes := exec.Command(bin, "--config-dir", dir, "approve", id)
	noYes.Env = append(noYes.Environ(), "FLEET_TOKEN=")
	out, runErr := noYes.CombinedOutput()
	if runErr == nil || !strings.Contains(string(out), "--yes") || !strings.Contains(string(out), "--timeout 5s") {
		t.Fatalf("approve without --yes must show the options and refuse; err=%v out=%q", runErr, out)
	}
	if got, _ := store.Get(id); got.Status != core.ApprovalPending {
		t.Fatalf("status after a refused approve = %s, want pending", got.Status)
	}

	cmd := exec.Command(bin, "--config-dir", dir, "approve", "--yes", id)
	cmd.Env = append(cmd.Environ(), "FLEET_TOKEN=")
	out, runErr = cmd.CombinedOutput()
	if runErr == nil {
		t.Fatalf("approve of a command that cannot run must exit non-zero; out=%q", out)
	}
	if !strings.Contains(string(out), "running it now") {
		t.Fatalf("approve did not run the command; out=%q", out)
	}

	got, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != core.ApprovalFailed || got.ExecutedAt == nil || got.ExitCode == nil || *got.ExitCode == 0 || got.ApprovedAt == nil {
		t.Fatalf("recorded approval = %+v, want failed with executed_at/exit_code/approved_at", got)
	}

	again := exec.Command(bin, "--config-dir", dir, "approve", "--yes", id)
	again.Env = append(again.Environ(), "FLEET_TOKEN=")
	if out, err := again.CombinedOutput(); err == nil || !strings.Contains(string(out), "not pending") {
		t.Fatalf("a second approve must be refused; err=%v out=%q", err, out)
	}
}

// TestScopedTokenCannotApprove: the approval queue is a human sign-off gate, so a
// scoped token — even one allowed destructive commands and the approve command —
// cannot approve (and so run) a staged command. The explicit denial runs before
// the generic RBAC v1 backstop, so it holds even if that allowlist ever grows.
func TestScopedTokenCannotApprove(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess RBAC test skipped in -short mode")
	}
	bin := buildFleetBinary(t)
	dir := initConfigDir(t, bin)

	approvals := core.NewApprovalStore(dir)
	id, err := approvals.Stage("web-01", "reboot", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := core.NewTokenStore(dir).Create(core.Token{
		Name:               "agent",
		AllowCommands:      []string{"approve", "approvals"},
		DestructiveAllowed: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "--config-dir", dir, "--token", scoped.ID, "approve", id)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "scoped token cannot run 'approve'") {
		t.Fatalf("scoped approve: err=%v out=%q, want the scoped-token denial", err, out)
	}
	if got, _ := approvals.Get(id); got.Status != core.ApprovalPending {
		t.Fatalf("approval must stay pending after a denied approve, got %s", got.Status)
	}
}

// TestApproveRefusesLegacyFlagLikeServer: an approval written by an older fleet
// (no server validation at staging) whose server is "--all" must be refused at
// approve time rather than handed to `fleet exec`.
func TestApproveRefusesLegacyFlagLikeServer(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess approval test skipped in -short mode")
	}
	bin := buildFleetBinary(t)
	dir := initConfigDir(t, bin)
	legacy := `[{"id":"0123456789abcdef","server":"--all","command":"uptime","status":"pending",` +
		`"requested":"2026-01-01T00:00:00Z","expires":"2999-01-01T00:00:00Z"}]`
	if err := os.WriteFile(core.ApprovalsPath(dir), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--config-dir", dir, "approve", "--yes", "0123456789abcdef")
	cmd.Env = append(cmd.Environ(), "FLEET_TOKEN=")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "invalid server name") {
		t.Fatalf("approving a legacy --all approval: err=%v out=%q, want a refusal", err, out)
	}
	got, gerr := core.NewApprovalStore(dir).Get("0123456789abcdef")
	if gerr != nil || got.Status != core.ApprovalPending {
		t.Fatalf("legacy approval = %+v, %v; must stay pending (never approved or run)", got, gerr)
	}
}
