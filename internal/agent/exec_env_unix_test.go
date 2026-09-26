// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

// TestRunShellExecEnvReachesWholeCommand: payload env is visible to every part
// of the command (a `VAR=v cmd` prefix only reached the first simple command).
func TestRunShellExecEnvReachesWholeCommand(t *testing.T) {
	t.Parallel()
	res, err := runShellExec(context.Background(), proto.ExecPayload{
		Command: `echo ${#TOK}; true; printenv TOK; sh -c 'echo "$TOK"'`,
		Env:     map[string]string{"TOK": `it's "q" $x`},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "11\nit's \"q\" $x\nit's \"q\" $x\n"
	if res.Stdout != want || res.ExitCode != 0 {
		t.Fatalf("stdout %q exit %d, want %q", res.Stdout, res.ExitCode, want)
	}
	if _, err := runShellExec(context.Background(), proto.ExecPayload{Command: "true", Env: map[string]string{"BAD-NAME": "s3cr3t"}}); err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("invalid name: err = %v (must fail without echoing the value)", err)
	}
	if !slices.Contains(DetectCapabilities(), proto.CapabilityExecEnv) {
		t.Fatal("agent does not advertise exec env support")
	}
}
