// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

// fakeHealthExec builds a HealthExecFunc from per-server canned stdout/errors.
func fakeHealthExec(out map[string]string, fail map[string]error) HealthExecFunc {
	return func(server, _ string) (proto.ExecResult, error) {
		if err := fail[server]; err != nil {
			return proto.ExecResult{}, err
		}
		return proto.ExecResult{Stdout: out[server], ExitCode: 0}, nil
	}
}

// probeStdout assembles a probe-output string from the named fields.
func probeStdout(epoch int64, swapKB int64, disk string, reboot int, load1 string, nproc int) string {
	return fmt.Sprintf("EPOCH=%d\nSWAPKB=%d\nDISK=%s\nREBOOT=%d\nLOAD1=%s\nNPROC=%d\n",
		epoch, swapKB, disk, reboot, load1, nproc)
}

func TestEvaluateHealthAllHealthy(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	out := map[string]string{
		"web-01": probeStdout(now.Unix(), 2_097_152, "42", 0, "0.50", 4),
	}
	report := EvaluateHealth(fakeHealthExec(out, nil), []string{"web-01"}, DefaultHealthThresholds(), now)
	if len(report.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(report.Results))
	}
	r := report.Results[0]
	if !r.Reachable {
		t.Fatalf("expected reachable")
	}
	if !r.Healthy {
		t.Fatalf("expected healthy, got problems %v (disk=%v load/cpu=%v skew=%v)",
			r.Problems(), r.DiskPercent, r.LoadPerCPU, r.ClockSkewSecs)
	}
	if r.CPUCount != 4 {
		t.Errorf("cpu count = %d, want 4", r.CPUCount)
	}
	if r.LoadPerCPU != 0.125 {
		t.Errorf("load/cpu = %v, want 0.125", r.LoadPerCPU)
	}
}

func TestEvaluateHealthAgentOffline(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	fail := map[string]error{"db-01": errors.New("dial tcp: connection refused")}
	report := EvaluateHealth(fakeHealthExec(nil, fail), []string{"db-01"}, DefaultHealthThresholds(), now)
	r := report.Results[0]
	if !r.AgentOffline {
		t.Fatalf("expected agent offline")
	}
	if r.Reachable {
		t.Fatalf("expected not reachable")
	}
	if r.Healthy {
		t.Fatalf("offline server must not be healthy")
	}
	if got := r.Problems(); len(got) != 1 || got[0] != HealthCheckAgentOffline {
		t.Errorf("problems = %v, want [agent_offline]", got)
	}
}

func TestEvaluateHealthNonZeroExitIsOffline(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	exec := func(server, _ string) (proto.ExecResult, error) {
		return proto.ExecResult{Stderr: "boom", ExitCode: 7}, nil
	}
	report := EvaluateHealth(exec, []string{"x"}, DefaultHealthThresholds(), now)
	r := report.Results[0]
	if !r.AgentOffline {
		t.Fatalf("non-zero exit should be treated as offline")
	}
	if r.Error != "boom" {
		t.Errorf("error = %q, want boom", r.Error)
	}
}

func TestEvaluateHealthFlags(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	th := DefaultHealthThresholds() // disk>85, load>1.0/cpu, skew>5s

	cases := []struct {
		name    string
		stdout  string
		want    []HealthCheck
		healthy bool
	}{
		{
			name:    "no swap",
			stdout:  probeStdout(now.Unix(), 0, "10", 0, "0.10", 2),
			want:    []HealthCheck{HealthCheckNoSwap},
			healthy: false,
		},
		{
			name:    "disk full",
			stdout:  probeStdout(now.Unix(), 1024, "90", 0, "0.10", 2),
			want:    []HealthCheck{HealthCheckDiskFull},
			healthy: false,
		},
		{
			name:    "disk at threshold is ok",
			stdout:  probeStdout(now.Unix(), 1024, "85", 0, "0.10", 2),
			want:    nil,
			healthy: true,
		},
		{
			name:    "reboot required",
			stdout:  probeStdout(now.Unix(), 1024, "10", 1, "0.10", 2),
			want:    []HealthCheck{HealthCheckRebootRequired},
			healthy: false,
		},
		{
			name:    "high load per cpu",
			stdout:  probeStdout(now.Unix(), 1024, "10", 0, "5.00", 2), // 2.5/cpu > 1.0
			want:    []HealthCheck{HealthCheckHighLoad},
			healthy: false,
		},
		{
			name:    "load ok when spread across cpus",
			stdout:  probeStdout(now.Unix(), 1024, "10", 0, "1.50", 4), // 0.375/cpu
			want:    nil,
			healthy: true,
		},
		{
			name:    "clock skew ahead",
			stdout:  probeStdout(now.Unix()-30, 1024, "10", 0, "0.10", 2),
			want:    []HealthCheck{HealthCheckClockSkew},
			healthy: false,
		},
		{
			name:    "small skew within tolerance",
			stdout:  probeStdout(now.Unix()-2, 1024, "10", 0, "0.10", 2),
			want:    nil,
			healthy: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := map[string]string{"s": tc.stdout}
			report := EvaluateHealth(fakeHealthExec(out, nil), []string{"s"}, th, now)
			r := report.Results[0]
			if r.Healthy != tc.healthy {
				t.Fatalf("healthy = %v, want %v (problems %v)", r.Healthy, tc.healthy, r.Problems())
			}
			if !reflect.DeepEqual(r.Problems(), tc.want) {
				t.Fatalf("problems = %v, want %v", r.Problems(), tc.want)
			}
		})
	}
}

func TestEvaluateHealthCustomThresholds(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	// disk 90 is fine under a 95% threshold; load 3/cpu fine under 4.0.
	th := HealthThresholds{DiskPercent: 95, LoadPerCPU: 4.0}
	out := map[string]string{"s": probeStdout(now.Unix(), 1024, "90", 0, "6.00", 2)} // 3.0/cpu
	report := EvaluateHealth(fakeHealthExec(out, nil), []string{"s"}, th, now)
	r := report.Results[0]
	if !r.Healthy {
		t.Fatalf("expected healthy under relaxed thresholds, problems %v", r.Problems())
	}
}

func TestEvaluateHealthSingleCoreFallback(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	// NPROC missing/0 — engine falls back to raw load vs per-CPU threshold.
	out := map[string]string{"s": "EPOCH=" + fmt.Sprint(now.Unix()) + "\nSWAPKB=1024\nDISK=10\nREBOOT=0\nLOAD1=2.00\nNPROC=\n"}
	report := EvaluateHealth(fakeHealthExec(out, nil), []string{"s"}, DefaultHealthThresholds(), now)
	r := report.Results[0]
	if !r.HighLoad {
		t.Fatalf("expected high load via fallback; load/cpu=%v cpu=%d", r.LoadPerCPU, r.CPUCount)
	}
}

func TestEvaluateHealthMultipleServersOrderPreserved(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	out := map[string]string{
		"a": probeStdout(now.Unix(), 1024, "10", 0, "0.1", 2),
		"b": probeStdout(now.Unix(), 0, "10", 0, "0.1", 2),
	}
	report := EvaluateHealth(fakeHealthExec(out, nil), []string{"a", "b"}, DefaultHealthThresholds(), now)
	if len(report.Results) != 2 {
		t.Fatalf("want 2 results")
	}
	if report.Results[0].Server != "a" || report.Results[1].Server != "b" {
		t.Fatalf("order not preserved: %q %q", report.Results[0].Server, report.Results[1].Server)
	}
	if report.Results[0].NoSwap {
		t.Errorf("a should have swap")
	}
	if !report.Results[1].NoSwap {
		t.Errorf("b should be flagged no-swap")
	}
}

func TestEvaluateHealthOutputMissingDiskNoFlag(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	// df produced no value (empty DISK) — must not flag disk-full or crash.
	res := HealthResult{Server: "s", SwapTotalKB: -1, DiskPercent: -1, CPUCount: -1}
	EvaluateHealthOutput(&res, "EPOCH="+fmt.Sprint(now.Unix())+"\nSWAPKB=1024\nDISK=\nREBOOT=0\nLOAD1=0.1\nNPROC=2\n", DefaultHealthThresholds(), now)
	if res.DiskFull {
		t.Errorf("missing disk value must not flag disk-full")
	}
	if res.DiskPercent != -1 {
		t.Errorf("disk percent should stay -1 when unknown, got %v", res.DiskPercent)
	}
}
