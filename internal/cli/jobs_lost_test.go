// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
)

// A job whose server was removed from the fleet can never report back: `job
// status` shows it lost, `job wait` ends on it with an error (exit 1) instead
// of polling forever, and `fleet jobs` lists it as lost.
func TestJobOnRemovedServerIsLost(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	launched := func(string, string) (string, int, error) { return "", 0, nil }
	rec, err := core.NewJobStore(configDir).Start(launched, "decommissioned", "sleep 600", "")
	if err != nil {
		t.Fatal(err)
	}

	out, err := runFleetIn(t, configDir, "job", "wait", "1", "--poll", "1ms")
	if err == nil || !strings.Contains(err.Error(), "was lost") || !strings.Contains(err.Error(), "removed from the fleet") {
		t.Fatalf("job wait on a lost job: err = %v, out = %q", err, out)
	}

	out, err = runFleetIn(t, configDir, "job", "status", "1")
	if err != nil || !strings.Contains(out, "lost") || !strings.Contains(out, "was lost: server decommissioned was removed") {
		t.Fatalf("job status: %q, %v", out, err)
	}

	out, err = runFleetIn(t, configDir, "jobs")
	if err != nil || !strings.Contains(out, "lost") {
		t.Fatalf("fleet jobs: %q, %v", out, err)
	}
	got, err := core.NewJobStore(configDir).Get(rec.ID)
	if err != nil || got.Status != core.JobLost {
		t.Fatalf("stored job: %+v, %v", got, err)
	}
}
