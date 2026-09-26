// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
)

// finishFakeJob makes the fake agent report job id as finished with code.
func finishFakeJob(t *testing.T, dir string, fake *fakeExecRPC, id, code int) {
	t.Helper()
	rec, err := core.NewJobStore(dir).Get(id)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.behave["srv"] = fakeExecBehavior{stdout: "output\nFLEETEXIT:" + rec.Nonce + ":" + strconv.Itoa(code) + "\n"}
	fake.mu.Unlock()
}

// TestJobWaitExitsNonZeroWhenTheJobFailed is the regression for `fleet job
// wait` exiting 0 whatever the job's exit code was.
func TestJobWaitExitsNonZeroWhenTheJobFailed(t *testing.T) {
	dir, fake := setupExecFanout(t, map[string]fakeExecBehavior{"srv": {}}, nil)
	for i := 0; i < 2; i++ {
		if r := runExecFleet(t, dir, "job", "run", "srv", "--", "false"); r.err != nil {
			t.Fatalf("job run: %v\n%s", r.err, r.combined)
		}
	}

	finishFakeJob(t, dir, fake, 1, 3)
	r := runExecFleet(t, dir, "job", "wait", "1", "--poll", "10ms")
	if r.err == nil || !strings.Contains(r.err.Error(), "job 1 on srv failed with exit code 3") {
		t.Fatalf("wait on a failed job: err=%v out=%q", r.err, r.combined)
	}
	if !strings.Contains(r.stdout, "job 1 done (exit 3)") {
		t.Fatalf("wait output = %q", r.stdout)
	}

	// --json keeps printing the record, and still exits non-zero.
	r = runExecFleet(t, dir, "job", "wait", "1", "--json")
	var rec core.JobRecord
	if err := json.Unmarshal([]byte(r.stdout), &rec); err != nil || rec.ExitCode != 3 || rec.Status != core.JobDone {
		t.Fatalf("wait --json record = %+v, %v\n%s", rec, err, r.stdout)
	}
	if r.err == nil {
		t.Fatal("wait --json on a failed job exited 0")
	}

	finishFakeJob(t, dir, fake, 2, 0)
	r = runExecFleet(t, dir, "job", "wait", "2", "--poll", "10ms")
	if r.err != nil || !strings.Contains(r.stdout, "job 2 done (exit 0)") {
		t.Fatalf("wait on a successful job: err=%v out=%q", r.err, r.combined)
	}
}
