// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// doctorFakeRule routes a command to a canned response by matching a substring
// of the command. The first matching rule wins; an unmatched command returns an
// empty ok result so checks that don't have a rule degrade gracefully.
type doctorFakeRule struct {
	match  string
	result ExecResultLike
	err    error
}

func doctorFakeExec(rules ...doctorFakeRule) doctorExec {
	return func(command string) (ExecResultLike, error) {
		for _, r := range rules {
			if strings.Contains(command, r.match) {
				return r.result, r.err
			}
		}
		return ExecResultLike{ExitCode: 0}, nil
	}
}

func doctorStatusOf(report DoctorReport, name string) (DoctorStatus, bool) {
	for _, c := range report.Checks {
		if c.Name == name {
			return c.Status, true
		}
	}
	return "", false
}

// healthyRules returns a baseline set of rules where every check passes, except
// any rule overridden by appending later (later-defined rules do not win, so
// callers should build their own slice when overriding). now feeds clock skew.
func healthyRules(now time.Time) []doctorFakeRule {
	return []doctorFakeRule{
		{match: "fleet-doctor-ok", result: ExecResultLike{Stdout: "fleet-doctor-ok\n"}},
		// The agent-port probe shell-quotes the port, so the literal command
		// contains /dev/tcp/127.0.0.1/'2222'.
		{match: "/dev/tcp/127.0.0.1/'2222'", result: ExecResultLike{Stdout: "open\n"}},
		{match: "/dev/tcp/127.0.0.1/22", result: ExecResultLike{Stdout: "open\n"}},
		{match: "df -P /", result: ExecResultLike{Stdout: "42%\n"}},
		{match: "/^Swap:/", result: ExecResultLike{Stdout: "2097152\n"}},
		{match: "reboot-required", result: ExecResultLike{Stdout: "no\n"}},
		{match: "date +%s", result: ExecResultLike{Stdout: fmt.Sprintf("%d\n", now.Unix())}},
	}
}

func TestRunDoctorAllHealthy(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	report := RunDoctor(DoctorProbe{Server: "web-01", AgentPort: 2222, Now: now}, doctorFakeExec(healthyRules(now)...))
	if !report.OK() {
		t.Fatalf("expected all checks ok, got %+v", report.Checks)
	}
	if report.Failed() {
		t.Fatalf("expected no failures, got %+v", report.Checks)
	}
	if len(report.Checks) != 7 {
		t.Fatalf("expected 7 checks, got %d", len(report.Checks))
	}
}

func TestRunDoctorAgentOfflineFails(t *testing.T) {
	exec := doctorFakeExec(
		doctorFakeRule{match: "fleet-doctor-ok", err: fmt.Errorf("dial tcp: connection refused")},
	)
	report := RunDoctor(DoctorProbe{Server: "db-01", AgentPort: 2222, Now: time.Unix(1_700_000_000, 0)}, exec)
	st, ok := doctorStatusOf(report, "agent online")
	if !ok || st != DoctorFail {
		t.Fatalf("expected agent online = fail, got %v ok=%v", st, ok)
	}
	if !report.Failed() {
		t.Fatalf("expected report.Failed() true")
	}
}

func TestRunDoctorDiskWarnAboveThreshold(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rules := healthyRules(now)
	rules[3] = doctorFakeRule{match: "df -P /", result: ExecResultLike{Stdout: "95%"}}
	report := RunDoctor(DoctorProbe{Server: "s", AgentPort: 2222, Now: now}, doctorFakeExec(rules...))
	if st, _ := doctorStatusOf(report, "disk usage"); st != DoctorWarn {
		t.Fatalf("expected disk usage = warn at 95%%, got %v", st)
	}
	// 90% exactly must not warn (threshold is strictly greater-than).
	rules[3] = doctorFakeRule{match: "df -P /", result: ExecResultLike{Stdout: "90%"}}
	report = RunDoctor(DoctorProbe{Server: "s", AgentPort: 2222, Now: now}, doctorFakeExec(rules...))
	if st, _ := doctorStatusOf(report, "disk usage"); st != DoctorOK {
		t.Fatalf("expected disk usage = ok at 90%%, got %v", st)
	}
}

func TestRunDoctorSwapZeroWarns(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rules := healthyRules(now)
	rules[4] = doctorFakeRule{match: "/^Swap:/", result: ExecResultLike{Stdout: "0"}}
	report := RunDoctor(DoctorProbe{Server: "s", AgentPort: 2222, Now: now}, doctorFakeExec(rules...))
	if st, _ := doctorStatusOf(report, "swap configured"); st != DoctorWarn {
		t.Fatalf("expected swap = warn when 0, got %v", st)
	}
}

func TestRunDoctorRebootRequiredWarns(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rules := healthyRules(now)
	rules[5] = doctorFakeRule{match: "reboot-required", result: ExecResultLike{Stdout: "yes"}}
	report := RunDoctor(DoctorProbe{Server: "s", AgentPort: 2222, Now: now}, doctorFakeExec(rules...))
	if st, _ := doctorStatusOf(report, "reboot required"); st != DoctorWarn {
		t.Fatalf("expected reboot required = warn, got %v", st)
	}
}

func TestRunDoctorClockSkewWarns(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rules := healthyRules(now)
	// remote clock 5 minutes behind the controller.
	rules[6] = doctorFakeRule{match: "date +%s", result: ExecResultLike{Stdout: "1699999700"}}
	report := RunDoctor(DoctorProbe{Server: "s", AgentPort: 2222, Now: now}, doctorFakeExec(rules...))
	if st, _ := doctorStatusOf(report, "clock skew"); st != DoctorWarn {
		t.Fatalf("expected clock skew = warn at 300s, got %v", st)
	}
}

func TestRunDoctorAgentPortClosedWarns(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rules := healthyRules(now)
	rules[1] = doctorFakeRule{match: "/dev/tcp/127.0.0.1/'2222'", result: ExecResultLike{Stdout: "closed"}}
	rules[2] = doctorFakeRule{match: "/dev/tcp/127.0.0.1/22", result: ExecResultLike{Stdout: "closed"}}
	report := RunDoctor(DoctorProbe{Server: "s", AgentPort: 2222, Now: now}, doctorFakeExec(rules...))
	if st, _ := doctorStatusOf(report, "agent port reachable"); st != DoctorWarn {
		t.Fatalf("expected agent port = warn when closed, got %v", st)
	}
	if st, _ := doctorStatusOf(report, "sshd reachable"); st != DoctorFail {
		t.Fatalf("expected sshd = fail when closed, got %v", st)
	}
}

func TestRunDoctorNilExec(t *testing.T) {
	report := RunDoctor(DoctorProbe{Server: "s", AgentPort: 2222, Now: time.Unix(1_700_000_000, 0)}, nil)
	if st, _ := doctorStatusOf(report, "agent online"); st != DoctorFail {
		t.Fatalf("expected agent online = fail with nil exec, got %v", st)
	}
	// Must not panic and must still produce the full checklist.
	if len(report.Checks) != 7 {
		t.Fatalf("expected 7 checks with nil exec, got %d", len(report.Checks))
	}
}

func TestAnalyzeCommandSafety(t *testing.T) {
	cases := []struct {
		name      string
		command   string
		agentPort int
		wantWarn  bool
	}{
		{name: "innocuous", command: "ls -la /var/log", agentPort: 2222, wantWarn: false},
		{name: "iptables drop policy", command: "iptables -P INPUT DROP", agentPort: 2222, wantWarn: true},
		{name: "iptables flush", command: "iptables -F", agentPort: 2222, wantWarn: true},
		{name: "iptables drop agent port", command: "iptables -A INPUT -p tcp --dport 2222 -j DROP", agentPort: 2222, wantWarn: true},
		{name: "nft flush ruleset", command: "nft flush ruleset", agentPort: 2222, wantWarn: true},
		{name: "firewalld panic", command: "firewall-cmd --panic-on", agentPort: 2222, wantWarn: true},
		{name: "ufw default deny", command: "ufw default deny incoming", agentPort: 2222, wantWarn: true},
		{name: "ufw default deny but allows agent", command: "ufw default deny incoming && ufw allow 2222", agentPort: 2222, wantWarn: false},
		{name: "ip route flush", command: "ip route flush table main", agentPort: 2222, wantWarn: true},
		{name: "del default route", command: "ip route del default", agentPort: 2222, wantWarn: true},
		{name: "empty", command: "", agentPort: 2222, wantWarn: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warnings := AnalyzeCommandSafety(tc.command, tc.agentPort)
			if got := len(warnings) > 0; got != tc.wantWarn {
				t.Fatalf("AnalyzeCommandSafety(%q) warnings=%v, want warn=%v", tc.command, warnings, tc.wantWarn)
			}
		})
	}
}

func TestAnalyzeCommandSafetyDefaultPort(t *testing.T) {
	// agentPort <= 0 must default to 2222 and still flag a drop of that port.
	warnings := AnalyzeCommandSafety("iptables -A INPUT -p tcp --dport 2222 -j DROP", 0)
	if len(warnings) == 0 {
		t.Fatalf("expected a warning for dropping the default agent port")
	}
}
