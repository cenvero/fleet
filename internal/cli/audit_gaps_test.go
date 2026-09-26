// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/pkg/proto"
)

// TestAuditCoversExecApprovalsSecretsTokensPolicies: exec, approval decisions,
// policy changes, secret and token changes wrote no audit entries. They now do,
// and no entry ever carries a secret value or a token's bearer id.
func TestAuditCoversExecApprovalsSecretsTokensPolicies(t *testing.T) {
	const secret = "Sup3r-Secret-Value"
	dir, _ := setupExecFanout(t, map[string]fakeExecBehavior{"web": {}}, nil)
	openAppHook = func(a *core.App) {
		a.ReverseRPCContext = func(ctx context.Context, server string, env proto.Envelope) (proto.Envelope, error) {
			if env.Action == proto.ActionFileDelete {
				return proto.Envelope{Error: &proto.Error{Code: "delete_failed", Message: "outside file root"}}, nil
			}
			return proto.Envelope{Payload: proto.ExecResult{ExitCode: 3}}, nil
		}
	}
	must := func(args ...string) fleetRun {
		t.Helper()
		r := runExecFleet(t, dir, args...)
		return r
	}
	must("secret", "set", "tok", "--value", secret)
	must("secret", "rotate", "tok")
	must("exec", "web", "--secret", "TOK=@tok", "--", "printenv TOK")
	staged := must("exec", "web", "--require-approval", "--", "uptime")
	id := regexp.MustCompile(`staged approval (\S+)`).FindStringSubmatch(staged.stdout)
	if id == nil {
		t.Fatalf("no approval staged: %q", staged.combined)
	}
	must("approvals", "reject", id[1])
	created := must("token", "create", "--name", "ci")
	bearer := strings.TrimSpace(strings.Split(strings.SplitN(created.stdout, "\n\n", 2)[1], "\n")[0])
	must("token", "revoke", bearer)
	must("cmd-policy", "set", "deny", "mkfs")
	must("policy", "set", "redact-pattern", secret)
	must("secret", "rm", "tok")
	must("file", "rm", "web", "/etc/x")

	entries, err := logs.NewAuditLog(filepath.Join(dir, "logs", "_audit.log")).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	byAction := map[string][]logs.AuditEntry{}
	for _, e := range entries {
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), secret) || (len(bearer) > 8 && strings.Contains(string(raw), bearer)) {
			t.Fatalf("audit entry leaks a secret or bearer token: %s", raw)
		}
		byAction[e.Action] = append(byAction[e.Action], e)
	}
	expect := func(action, target, detail string) {
		t.Helper()
		for _, e := range byAction[action] {
			if e.Target == target && strings.Contains(e.Details, detail) && e.Operator != "" {
				return
			}
		}
		t.Errorf("missing audit %s target=%q details~%q; have %+v", action, target, detail, byAction[action])
	}
	expect("exec.run", "web", `command="TOK=@tok printenv TOK" exit_code=3`)
	expect("approval.stage", "web", `id=`+id[1]+` command="uptime"`)
	expect("approval.reject", "web", `id=`+id[1])
	expect("secret.set", "tok", "")
	expect("secret.rotate", "tok", "length=40")
	expect("secret.remove", "tok", "")
	expect("token.create", "ci", "id="+bearer[:8])
	expect("token.revoke", "ci", "id="+bearer[:8])
	expect("cmd-policy.set", "controller", `kind=deny patterns=["mkfs"]`)
	expect("policy.set", "controller", "redact-patterns count=1")
	expect("file.delete.failed", "web", "/etc/x error=delete_failed")
}
