// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

func TestExecPayloadWithEnv(t *testing.T) {
	t.Parallel()
	const value = `s3 'cr"et $HOME`
	env := map[string]string{"TOK": value}

	// Current agents: env travels in the payload, the command line is untouched.
	p, err := execPayloadWithEnv(ServerRecord{Name: "a", Capabilities: []string{proto.CapabilityExecEnv}}, "a; printenv TOK", env)
	if err != nil || p.Command != "a; printenv TOK" || p.Env["TOK"] != value {
		t.Fatalf("capable agent payload = %+v, %v", p, err)
	}

	// Older POSIX agents: an export prefix that reaches the whole command.
	p, err = execPayloadWithEnv(ServerRecord{Name: "b", Observed: ServerObservation{OS: "linux"}}, `echo ${#TOK}; true; printenv TOK`, env)
	if err != nil || p.Env != nil || !strings.HasPrefix(p.Command, "export TOK=") {
		t.Fatalf("fallback payload = %+v, %v", p, err)
	}
	if runtime.GOOS != "windows" {
		out, err := exec.Command("/bin/sh", "-c", p.Command).Output() // #nosec G204 -- test runs its own fixed command
		if err != nil {
			t.Fatal(err)
		}
		if want := "15\n" + value + "\n"; string(out) != want {
			t.Fatalf("fallback ran as %q, want %q", out, want)
		}
	}

	// Older Windows agents: refused, naming the variable but never the value.
	_, err = execPayloadWithEnv(ServerRecord{Name: "w", Observed: ServerObservation{OS: "windows"}}, "echo %TOK%", env)
	if err == nil || !strings.Contains(err.Error(), "TOK") || strings.Contains(err.Error(), "cr\"et") {
		t.Fatalf("windows fallback err = %v", err)
	}
	if _, err := execPayloadWithEnv(ServerRecord{}, "x", map[string]string{"A B": value}); err == nil || strings.Contains(err.Error(), "cr\"et") {
		t.Fatalf("bad name err = %v", err)
	}
}
