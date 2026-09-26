// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/pkg/proto"
)

// TestExecSecretReachesWholeCommand: --secret used to be sent as a `VAR='v'
// cmd` prefix, which only set the variable for the first simple command. It now
// travels as process environment (capable agents) or an export prefix (older
// POSIX agents), and the value never shows in output, dry-run or JSON.
func TestExecSecretReachesWholeCommand(t *testing.T) {
	const secret = "s3cr3t-Value-42"
	dir, _ := setupExecFanout(t, map[string]fakeExecBehavior{"new": {}, "old": {}}, nil)
	if err := core.NewSecretStore(dir).Set("tok", secret); err != nil {
		t.Fatal(err)
	}
	app, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := app.GetServer("new")
	if err != nil {
		t.Fatal(err)
	}
	rec.Capabilities = append(rec.Capabilities, proto.CapabilityExecEnv)
	if err := app.SaveServer(rec); err != nil {
		t.Fatal(err)
	}
	_ = app.Close()

	var mu sync.Mutex
	payloads := map[string]proto.ExecPayload{}
	openAppHook = func(a *core.App) {
		a.ReverseRPCContext = func(ctx context.Context, server string, env proto.Envelope) (proto.Envelope, error) {
			p, err := proto.DecodePayload[proto.ExecPayload](env.Payload)
			if err != nil {
				return proto.Envelope{}, err
			}
			mu.Lock()
			payloads[server] = p
			mu.Unlock()
			// Echo what the remote command would see, including the value.
			value := p.Env["TOK"]
			if value == "" && strings.HasPrefix(p.Command, "export TOK=") {
				value = secret
			}
			return proto.Envelope{Payload: proto.ExecResult{Stdout: fmt.Sprintf("%d %s\n", len(value), value)}}, nil
		}
	}

	r := runExecFleet(t, dir, "exec", "--all", "--secret", "TOK=@tok", "--", "echo ${#TOK}; printenv TOK")
	if r.err != nil {
		t.Fatalf("exec: %v\n%s", r.err, r.combined)
	}
	if strings.Contains(r.combined, secret) {
		t.Fatalf("secret value leaked into output:\n%s", r.combined)
	}
	if strings.Count(r.stdout, fmt.Sprintf("%d %s", len(secret), core.RedactPlaceholder)) != 2 {
		t.Fatalf("remote command did not see the secret on both servers:\n%s", r.stdout)
	}
	mu.Lock()
	newP, oldP := payloads["new"], payloads["old"]
	mu.Unlock()
	if newP.Command != "echo ${#TOK}; printenv TOK" || newP.Env["TOK"] != secret {
		t.Fatalf("capable agent should get the value as env, not on the command line: %+v", newP)
	}
	if !strings.HasPrefix(oldP.Command, "export TOK=") || !strings.HasSuffix(oldP.Command, "; echo ${#TOK}; printenv TOK") || oldP.Env != nil {
		t.Fatalf("older agent should get an export prefix: %q", oldP.Command)
	}

	for _, args := range [][]string{
		{"exec", "--all", "--dry-run", "--secret", "TOK=@tok", "--", "printenv TOK"},
		{"exec", "--all", "--json", "--secret", "TOK=@tok", "--", "printenv TOK"},
	} {
		if r := runExecFleet(t, dir, args...); strings.Contains(r.combined, secret) {
			t.Fatalf("%v leaked the secret:\n%s", args, r.combined)
		}
	}
}
