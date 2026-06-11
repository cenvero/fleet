// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

// FL-020 — fleet health.
//
// This file holds the pure evaluation engine for `fleet health`. It is driven
// by a HealthExecFunc (a thin shim over App.ExecCommand) so the logic can be
// unit-tested with a fake exec function and does not hard-depend on *App.
//
// A single round-trip probe script gathers everything we need from the remote
// host (swap, disk usage on /, reboot-required marker, uptime/clock, loadavg,
// and nproc). EvaluateHealthOutput parses that output and applies the operator
// thresholds, producing a HealthReport whose per-check booleans drive the CLI
// table and the --json schema.

// HealthExecFunc runs a command on a server and returns its exec result. It
// mirrors App.ExecCommand so the engine can be exercised with a fake.
type HealthExecFunc func(server, command string) (proto.ExecResult, error)

// HealthThresholds are the operator-tunable limits for the health checks.
type HealthThresholds struct {
	// DiskPercent flags servers whose / usage is strictly greater than this.
	DiskPercent float64
	// LoadPerCPU flags servers whose 1-minute load average per CPU exceeds this.
	// When zero, a default of 1.0 (one busy core per CPU) is used.
	LoadPerCPU float64
	// ClockSkew flags servers whose clock differs from the controller by more
	// than this. When zero, DefaultClockSkew is used.
	ClockSkew time.Duration
}

// DefaultHealthThresholds returns the built-in defaults (disk 85%, load 1.0/CPU).
func DefaultHealthThresholds() HealthThresholds {
	return HealthThresholds{
		DiskPercent: 85,
		LoadPerCPU:  1.0,
		ClockSkew:   DefaultClockSkew,
	}
}

// DefaultClockSkew is the clock-skew tolerance before a server is flagged.
const DefaultClockSkew = 5 * time.Second

// HealthCheck identifies a single per-server condition.
type HealthCheck string

const (
	HealthCheckAgentOffline   HealthCheck = "agent_offline"
	HealthCheckNoSwap         HealthCheck = "no_swap"
	HealthCheckDiskFull       HealthCheck = "disk_full"
	HealthCheckRebootRequired HealthCheck = "reboot_required"
	HealthCheckClockSkew      HealthCheck = "clock_skew"
	HealthCheckHighLoad       HealthCheck = "high_load"
)

// HealthResult is the per-server outcome of a health probe.
type HealthResult struct {
	Server string `json:"server"`

	// Reachable is false when the agent exec failed (agent offline / unreachable).
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`

	// Per-check flags. A true value means the condition is present (a problem).
	AgentOffline   bool `json:"agent_offline"`
	NoSwap         bool `json:"no_swap"`
	DiskFull       bool `json:"disk_full"`
	RebootRequired bool `json:"reboot_required"`
	ClockSkew      bool `json:"clock_skew"`
	HighLoad       bool `json:"high_load"`

	// Observed values backing the flags (for the table and JSON detail).
	DiskPercent   float64 `json:"disk_percent"`
	Load1         float64 `json:"load1"`
	CPUCount      int     `json:"cpu_count"`
	LoadPerCPU    float64 `json:"load_per_cpu"`
	SwapTotalKB   int64   `json:"swap_total_kb"`
	ClockSkewSecs float64 `json:"clock_skew_secs"`

	// Healthy is true when no problem flags are set and the agent is reachable.
	Healthy bool `json:"healthy"`
}

// Problems returns the list of failing checks for this result, in a stable order.
func (r HealthResult) Problems() []HealthCheck {
	var out []HealthCheck
	if r.AgentOffline {
		out = append(out, HealthCheckAgentOffline)
	}
	if r.NoSwap {
		out = append(out, HealthCheckNoSwap)
	}
	if r.DiskFull {
		out = append(out, HealthCheckDiskFull)
	}
	if r.RebootRequired {
		out = append(out, HealthCheckRebootRequired)
	}
	if r.ClockSkew {
		out = append(out, HealthCheckClockSkew)
	}
	if r.HighLoad {
		out = append(out, HealthCheckHighLoad)
	}
	return out
}

// HealthReport is the full fleet-wide result set.
type HealthReport struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Thresholds  HealthThresholds `json:"thresholds"`
	Results     []HealthResult   `json:"results"`
}

// healthProbeScript collects every metric we need in one round trip. It is a
// constant string with no interpolation, so there is nothing to shell-quote
// here; the only interpolated value in this feature is the server name handed
// to ExecCommand, which the agent treats as an addressing token, not a shell
// fragment. Output is a set of KEY=VALUE lines, tolerant of missing tools.
const healthProbeScript = `
echo "EPOCH=$(date +%s 2>/dev/null)";
echo "SWAPKB=$(awk '/^SwapTotal:/{print $2}' /proc/meminfo 2>/dev/null)";
echo "DISK=$(df -P / 2>/dev/null | awk 'NR==2{gsub("%","",$5); print $5}')";
if [ -e /var/run/reboot-required ] || [ -e /run/reboot-required ]; then echo "REBOOT=1"; else echo "REBOOT=0"; fi;
echo "LOAD1=$(awk '{print $1}' /proc/loadavg 2>/dev/null)";
echo "NPROC=$(nproc 2>/dev/null || awk '/^processor/{n++}END{print n}' /proc/cpuinfo 2>/dev/null)";
`

// HealthProbeCommand returns the remote command used to gather health data.
// It is exported so the CLI can reuse the exact same probe.
func HealthProbeCommand() string { return healthProbeScript }

// EvaluateHealth probes every named server via exec and evaluates the checks.
// controllerNow is the controller's clock at probe time (used for skew); pass
// time.Now().UTC(). It never returns an error for a single bad server — a
// failed exec is recorded as an offline agent on that server's result.
func EvaluateHealth(exec HealthExecFunc, servers []string, th HealthThresholds, controllerNow time.Time) HealthReport {
	th = normalizeThresholds(th)
	report := HealthReport{
		GeneratedAt: controllerNow,
		Thresholds:  th,
		Results:     make([]HealthResult, 0, len(servers)),
	}
	for _, name := range servers {
		report.Results = append(report.Results, evaluateServerHealth(exec, name, th, controllerNow))
	}
	return report
}

func normalizeThresholds(th HealthThresholds) HealthThresholds {
	// Only a negative value means "unset": an explicit 0 disables that check.
	if th.DiskPercent < 0 {
		th.DiskPercent = 85
	}
	if th.LoadPerCPU < 0 {
		th.LoadPerCPU = 1.0
	}
	if th.ClockSkew <= 0 {
		th.ClockSkew = DefaultClockSkew
	}
	return th
}

func evaluateServerHealth(exec HealthExecFunc, name string, th HealthThresholds, controllerNow time.Time) HealthResult {
	res := HealthResult{Server: name, SwapTotalKB: -1, DiskPercent: -1, CPUCount: -1}
	// Time the probe round trip so clock skew is measured against the round-trip
	// midpoint rather than the moment before the probe. controllerNow is the
	// controller's clock at the START of this probe; adding half the round trip
	// yields the controller's clock at the instant the remote read its own time,
	// which is what we should compare the remote epoch against. Without this, the
	// exec round-trip latency is counted as skew and high-latency links
	// false-positive as clock-skewed. (A near-instant exec — e.g. a unit-test
	// fake — leaves controllerNow effectively unchanged.)
	tBefore := time.Now()
	out, err := exec(name, healthProbeScript)
	tAfter := time.Now()
	if err != nil {
		res.AgentOffline = true
		res.Error = err.Error()
		return res
	}
	if out.ExitCode != 0 {
		// The probe ran but the shell returned non-zero — treat as offline so
		// the operator notices rather than silently passing every check.
		res.AgentOffline = true
		msg := strings.TrimSpace(out.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("probe exited %d", out.ExitCode)
		}
		res.Error = msg
		return res
	}
	res.Reachable = true
	skewRef := controllerNow.Add(tAfter.Sub(tBefore) / 2)
	EvaluateHealthOutput(&res, out.Stdout, th, skewRef)
	res.Healthy = res.Reachable && len(res.Problems()) == 0
	return res
}

// EvaluateHealthOutput parses KEY=VALUE probe stdout into res and applies the
// thresholds. It is split out so it can be unit-tested directly without exec.
// The caller is responsible for setting Reachable and Healthy.
func EvaluateHealthOutput(res *HealthResult, stdout string, th HealthThresholds, controllerNow time.Time) {
	th = normalizeThresholds(th)
	fields := parseKeyValueLines(stdout)

	// Swap: a total of 0 KB means no swap is configured.
	if v, ok := fields["SWAPKB"]; ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			res.SwapTotalKB = n
			res.NoSwap = n == 0
		}
	} else {
		// Missing meminfo line: can't tell, leave SwapTotalKB at -1 and don't flag.
		res.SwapTotalKB = -1
	}

	// Disk usage on /. A threshold of 0 disables the check.
	if v, ok := fields["DISK"]; ok && strings.TrimSpace(v) != "" {
		if pct, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			res.DiskPercent = pct
			res.DiskFull = th.DiskPercent > 0 && pct > th.DiskPercent
		}
	}

	// Reboot-required marker.
	if strings.TrimSpace(fields["REBOOT"]) == "1" {
		res.RebootRequired = true
	}

	// Load average per CPU.
	if v, ok := fields["LOAD1"]; ok && strings.TrimSpace(v) != "" {
		if l1, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			res.Load1 = l1
		}
	}
	if v, ok := fields["NPROC"]; ok && strings.TrimSpace(v) != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			res.CPUCount = n
		}
	}
	// A load threshold of 0 disables the check.
	if res.CPUCount > 0 {
		res.LoadPerCPU = res.Load1 / float64(res.CPUCount)
		res.HighLoad = th.LoadPerCPU > 0 && res.LoadPerCPU > th.LoadPerCPU
	} else {
		// No CPU count: fall back to raw load against the per-CPU threshold so a
		// genuinely overloaded single-core box still trips.
		res.LoadPerCPU = res.Load1
		res.HighLoad = th.LoadPerCPU > 0 && res.Load1 > th.LoadPerCPU
	}

	// Clock skew: compare the remote epoch to the controller clock.
	if v, ok := fields["EPOCH"]; ok && strings.TrimSpace(v) != "" {
		if epoch, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && epoch > 0 {
			remote := time.Unix(epoch, 0)
			skew := controllerNow.Sub(remote)
			res.ClockSkewSecs = skew.Seconds()
			if math.Abs(skew.Seconds()) > th.ClockSkew.Seconds() {
				res.ClockSkew = true
			}
		}
	}
}

// parseKeyValueLines parses "KEY=VALUE" lines into a map. Lines without an '='
// are ignored. Only the first '=' splits the line, so values may contain '='.
func parseKeyValueLines(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

// CheckLabel returns a short, stable label for a health check (for tables).
func CheckLabel(c HealthCheck) string {
	switch c {
	case HealthCheckAgentOffline:
		return "offline"
	case HealthCheckNoSwap:
		return "no-swap"
	case HealthCheckDiskFull:
		return "disk-full"
	case HealthCheckRebootRequired:
		return "reboot"
	case HealthCheckClockSkew:
		return "clock-skew"
	case HealthCheckHighLoad:
		return "high-load"
	default:
		return string(c)
	}
}
