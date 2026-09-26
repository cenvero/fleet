// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"strings"
	"testing"
)

// TestHealthGroupMatchingNothingIsAnError is the regression for `fleet health
// --group <typo>` printing "no servers to check" and exiting 0.
func TestHealthGroupMatchingNothingIsAnError(t *testing.T) {
	dir, fake := setupExecFanout(t, map[string]fakeExecBehavior{"web-1": {}},
		map[string]map[string]string{"web-1": {"role": "web"}})

	for _, args := range [][]string{
		{"health", "--group", "role=nomatch"},
		{"health", "--group", "role=nomatch", "--json"},
	} {
		r := runExecFleet(t, dir, args...)
		if r.err == nil || !strings.Contains(r.err.Error(), `no servers match --group "role=nomatch"`) {
			t.Fatalf("%v: err=%v out=%q", args, r.err, r.combined)
		}
		if r.stdout != "" {
			t.Fatalf("%v printed a report for no servers:\n%s", args, r.stdout)
		}
	}
	if n := fake.calls.Load(); n != 0 {
		t.Fatalf("an empty selection probed %d servers", n)
	}

	r := runExecFleet(t, dir, "health", "--group", "role=web", "--json")
	if r.err != nil || !strings.Contains(r.stdout, `"server": "web-1"`) {
		t.Fatalf("matching --group: err=%v out=%q", r.err, r.combined)
	}
}

// An empty fleet has nothing to check, which is not an error.
func TestHealthWithNoServersIsNotAnError(t *testing.T) {
	dir, _ := setupExecFanout(t, map[string]fakeExecBehavior{}, nil)
	r := runExecFleet(t, dir, "health")
	if r.err != nil || strings.TrimSpace(r.stdout) != "no servers to check" {
		t.Fatalf("health on an empty fleet: err=%v out=%q", r.err, r.combined)
	}
}
