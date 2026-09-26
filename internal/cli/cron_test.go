// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"strings"
	"testing"
)

// TestCronReportsMissingCrontab is the regression for `fleet cron list`
// reporting "no managed scheduled jobs" on a server that has no crontab
// command at all (the read's exit 127 was masked).
func TestCronReportsMissingCrontab(t *testing.T) {
	dir, fake := setupExecFanout(t, map[string]fakeExecBehavior{"srv": {exit: 127}}, nil)

	r := runExecFleet(t, dir, "cron", "list", "srv")
	if r.err == nil || !strings.Contains(r.err.Error(), "crontab is not installed on srv") {
		t.Fatalf("cron list without crontab: err=%v out=%q", r.err, r.combined)
	}
	if strings.Contains(r.stdout, "no managed scheduled jobs") {
		t.Fatalf("missing crontab reported as an empty job list:\n%s", r.stdout)
	}

	for _, args := range [][]string{
		{"cron", "add", "srv", "--name", "backup", "--schedule", "0 3 * * *", "--cmd", "/opt/backup.sh"},
		{"cron", "rm", "srv", "--name", "backup"},
	} {
		before := fake.calls.Load()
		r := runExecFleet(t, dir, args...)
		if r.err == nil || !strings.Contains(r.err.Error(), "crontab is not installed on srv") {
			t.Fatalf("%v without crontab: err=%v", args, r.err)
		}
		if n := fake.calls.Load() - before; n != 1 {
			t.Fatalf("%v made %d remote calls; want only the read, no write", args, n)
		}
	}
}

func TestCronListStillTreatsNoCrontabAsEmpty(t *testing.T) {
	dir, _ := setupExecFanout(t, map[string]fakeExecBehavior{"srv": {}}, nil)
	r := runExecFleet(t, dir, "cron", "list", "srv")
	if r.err != nil || !strings.Contains(r.stdout, "no managed scheduled jobs on srv") {
		t.Fatalf("cron list with an empty crontab: err=%v out=%q", r.err, r.combined)
	}

	dir, _ = setupExecFanout(t, map[string]fakeExecBehavior{"srv": {
		stdout: "# >>> fleet:backup >>>\n0 3 * * * /opt/backup.sh\n# <<< fleet:backup <<<\n",
	}}, nil)
	r = runExecFleet(t, dir, "cron", "list", "srv")
	if r.err != nil || !strings.Contains(r.stdout, "backup") || !strings.Contains(r.stdout, "/opt/backup.sh") {
		t.Fatalf("cron list with a managed job: err=%v out=%q", r.err, r.combined)
	}
}
