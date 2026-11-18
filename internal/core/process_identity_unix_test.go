// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package core

import (
	"os/exec"
	"testing"
)

// Before fleet signals a pid from a pid or state file, it checks that the
// process runs the right fleet command, so a stale file naming a reused pid
// never gets an unrelated process stopped (on Linux through /proc, elsewhere
// through ps).
func TestProcessLooksLikeFleetRefusesAnUnrelatedProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep binary:", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for _, words := range [][]string{{"console", "serve"}, {"daemon"}, {HelperCommand}} {
		if ok, why := processLooksLikeFleet(cmd.Process.Pid, words...); ok {
			t.Fatalf("`sleep 30` passed as a fleet %v", words)
		} else if why == "" {
			t.Fatalf("refused without saying why (%v)", words)
		}
	}
}
