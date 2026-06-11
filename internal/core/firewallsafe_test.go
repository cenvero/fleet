// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"strings"
	"testing"
)

// fakeFirewall records every command and replies from a programmable table.
type fakeFirewall struct {
	calls    []string
	respond  func(cmd string) (string, int, error)
	failCmds map[string]bool
}

func (f *fakeFirewall) exec(cmd string) (string, int, error) {
	f.calls = append(f.calls, cmd)
	if f.failCmds[cmd] {
		return "", 1, nil
	}
	if f.respond != nil {
		return f.respond(cmd)
	}
	return "", 0, nil
}

func (f *fakeFirewall) sawSubstring(sub string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func (f *fakeFirewall) indexOf(sub string) int {
	for i, c := range f.calls {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

func TestParsePortSpec(t *testing.T) {
	cases := []struct {
		in      string
		want    PortSpec
		wantErr bool
	}{
		{"443", PortSpec{443, ProtoTCP}, false},
		{"443/tcp", PortSpec{443, ProtoTCP}, false},
		{"53/udp", PortSpec{53, ProtoUDP}, false},
		{" 8080 / TCP ", PortSpec{8080, ProtoTCP}, false},
		{"0", PortSpec{}, true},
		{"70000", PortSpec{}, true},
		{"abc", PortSpec{}, true},
		{"443/sctp", PortSpec{}, true},
		{"", PortSpec{}, true},
	}
	for _, tc := range cases {
		got, err := ParsePortSpec(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParsePortSpec(%q): expected error, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsePortSpec(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParsePortSpec(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestDetectBackendPriority(t *testing.T) {
	// ufw and iptables both present -> ufw wins (higher priority manager).
	fake := &fakeFirewall{respond: func(cmd string) (string, int, error) {
		if strings.Contains(cmd, "command -v 'ufw'") || strings.Contains(cmd, "command -v 'iptables'") {
			return "yes", 0, nil
		}
		return "no", 0, nil
	}}
	eng := &FirewallEngine{AgentPort: 2222, Exec: fake.exec}
	backend, err := eng.DetectBackend()
	if err != nil {
		t.Fatalf("DetectBackend: %v", err)
	}
	if backend != FirewallBackendUFW {
		t.Fatalf("backend = %q, want ufw", backend)
	}
}

func TestDetectBackendUnknown(t *testing.T) {
	fake := &fakeFirewall{respond: func(cmd string) (string, int, error) { return "no", 0, nil }}
	eng := &FirewallEngine{AgentPort: 2222, Exec: fake.exec}
	backend, err := eng.DetectBackend()
	if err != nil {
		t.Fatalf("DetectBackend: %v", err)
	}
	if backend != FirewallBackendUnknown {
		t.Fatalf("backend = %q, want unknown", backend)
	}
}

func TestAllowPortCommandsShellQuoted(t *testing.T) {
	spec := PortSpec{443, ProtoTCP}
	for _, b := range []FirewallBackend{FirewallBackendUFW, FirewallBackendFirewalld, FirewallBackendNftables, FirewallBackendIptables} {
		cmd, err := allowPortCommand(b, spec)
		if err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		if !strings.Contains(cmd, "443") {
			t.Errorf("%s allow cmd missing port: %q", b, cmd)
		}
	}
}

// TestEnableSafeAllowsAgentPortFirst is the safety-critical invariant: the agent
// port allow MUST be issued before the enable command.
func TestEnableSafeAllowsAgentPortFirst(t *testing.T) {
	fake := &fakeFirewall{respond: func(cmd string) (string, int, error) {
		// After the allow, the ruleset dump must report the agent port open so the
		// lockout guard passes.
		if strings.Contains(cmd, "iptables -S") {
			return "-A INPUT -p tcp --dport 2222 -j ACCEPT", 0, nil
		}
		return "", 0, nil
	}}
	eng := &FirewallEngine{AgentPort: 2222, Exec: fake.exec}
	res, err := eng.EnableSafe(FirewallBackendIptables, false, 60)
	if err != nil {
		t.Fatalf("EnableSafe: %v", err)
	}
	allowIdx := fake.indexOf("--dport '2222' -j ACCEPT")
	enableIdx := fake.indexOf("iptables -P INPUT DROP")
	if allowIdx < 0 {
		t.Fatalf("agent port allow never issued; calls=%v", fake.calls)
	}
	if enableIdx < 0 {
		t.Fatalf("enable never issued; calls=%v", fake.calls)
	}
	if allowIdx >= enableIdx {
		t.Fatalf("agent allow (idx %d) must precede enable (idx %d)", allowIdx, enableIdx)
	}
	if !res.UndoScheduled {
		t.Errorf("expected self-revert timer to be scheduled")
	}
	if !fake.sawSubstring("systemd-run --on-active='60s'") {
		t.Errorf("expected systemd-run self-revert command, calls=%v", fake.calls)
	}
}

// TestEnableSafeRefusesLockout: if the resulting ruleset does NOT show the agent
// port open and --force is not set, EnableSafe must refuse and NOT enable.
func TestEnableSafeRefusesLockout(t *testing.T) {
	fake := &fakeFirewall{respond: func(cmd string) (string, int, error) {
		if strings.Contains(cmd, "iptables -S") {
			return "-P INPUT DROP", 0, nil // no agent-port accept present
		}
		return "", 0, nil
	}}
	eng := &FirewallEngine{AgentPort: 2222, Exec: fake.exec}
	_, err := eng.EnableSafe(FirewallBackendIptables, false, 60)
	if err == nil {
		t.Fatalf("expected EnableSafe to refuse lockout")
	}
	if !strings.Contains(err.Error(), "force-i-have-console") {
		t.Errorf("error should mention the force flag, got: %v", err)
	}
	if fake.sawSubstring("iptables -P INPUT DROP") {
		t.Errorf("enable must NOT run when lockout is refused; calls=%v", fake.calls)
	}
	// The agent-port allow should still have been attempted first.
	if !fake.sawSubstring("--dport '2222' -j ACCEPT") {
		t.Errorf("agent port allow should run even when refusing; calls=%v", fake.calls)
	}
}

// TestEnableSafeForceBypassesLockout: with --force, enable proceeds even when the
// ruleset check does not confirm the agent port, but still allows it first.
func TestEnableSafeForceBypassesLockout(t *testing.T) {
	fake := &fakeFirewall{respond: func(cmd string) (string, int, error) {
		if strings.Contains(cmd, "iptables -S") {
			return "-P INPUT DROP", 0, nil
		}
		return "", 0, nil
	}}
	eng := &FirewallEngine{AgentPort: 2222, Exec: fake.exec}
	_, err := eng.EnableSafe(FirewallBackendIptables, true, 30)
	if err != nil {
		t.Fatalf("EnableSafe(force): %v", err)
	}
	if !fake.sawSubstring("iptables -P INPUT DROP") {
		t.Errorf("enable should run under --force; calls=%v", fake.calls)
	}
	if !fake.sawSubstring("--dport '2222' -j ACCEPT") {
		t.Errorf("agent allow must still run first under --force; calls=%v", fake.calls)
	}
}

// TestEnableSafeFailsClosedOnDumpError: if the ruleset dump ERRORS we cannot
// verify the agent port survives, so EnableSafe must refuse (fail CLOSED) and
// must NOT enable a default-drop firewall — otherwise a transient read failure
// could lock out the controller. The agent-port allow is still attempted first.
func TestEnableSafeFailsClosedOnDumpError(t *testing.T) {
	fake := &fakeFirewall{respond: func(cmd string) (string, int, error) {
		if strings.Contains(cmd, "iptables -S") {
			return "", 0, fmt.Errorf("transport read failed")
		}
		return "", 0, nil
	}}
	eng := &FirewallEngine{AgentPort: 2222, Exec: fake.exec}
	_, err := eng.EnableSafe(FirewallBackendIptables, false, 60)
	if err == nil {
		t.Fatalf("expected EnableSafe to refuse when the ruleset dump errors (fail-closed)")
	}
	if !strings.Contains(err.Error(), "force-i-have-console") {
		t.Errorf("refusal should mention the force flag, got: %v", err)
	}
	if fake.sawSubstring("iptables -P INPUT DROP") {
		t.Errorf("enable must NOT run when the dump fails; calls=%v", fake.calls)
	}
	if !fake.sawSubstring("--dport '2222' -j ACCEPT") {
		t.Errorf("agent port allow should still run first; calls=%v", fake.calls)
	}
}

// TestEnableSafeForceBypassesDumpError: --force-i-have-console bypasses the
// fail-closed dump-error refusal too (the operator accepts the lockout risk),
// but the agent-port allow still runs first.
func TestEnableSafeForceBypassesDumpError(t *testing.T) {
	fake := &fakeFirewall{respond: func(cmd string) (string, int, error) {
		if strings.Contains(cmd, "iptables -S") {
			return "", 0, fmt.Errorf("transport read failed")
		}
		return "", 0, nil
	}}
	eng := &FirewallEngine{AgentPort: 2222, Exec: fake.exec}
	if _, err := eng.EnableSafe(FirewallBackendIptables, true, 30); err != nil {
		t.Fatalf("EnableSafe(force) should proceed despite a dump error: %v", err)
	}
	if !fake.sawSubstring("iptables -P INPUT DROP") {
		t.Errorf("enable should run under --force even when the dump errors; calls=%v", fake.calls)
	}
	if !fake.sawSubstring("--dport '2222' -j ACCEPT") {
		t.Errorf("agent allow must still run first under --force; calls=%v", fake.calls)
	}
}

func TestEnableSafeRejectsBadAgentPort(t *testing.T) {
	fake := &fakeFirewall{}
	eng := &FirewallEngine{AgentPort: 0, Exec: fake.exec}
	if _, err := eng.EnableSafe(FirewallBackendIptables, false, 60); err == nil {
		t.Fatalf("expected refusal for invalid agent port")
	}
	if len(fake.calls) != 0 {
		t.Errorf("nothing should be executed with an invalid agent port; calls=%v", fake.calls)
	}
}

func TestEnableSafeAbortsIfAgentAllowFails(t *testing.T) {
	allowCmd := "iptables -I INPUT 1 -p 'tcp' --dport '2222' -j ACCEPT"
	fake := &fakeFirewall{failCmds: map[string]bool{allowCmd: true}}
	eng := &FirewallEngine{AgentPort: 2222, Exec: fake.exec}
	_, err := eng.EnableSafe(FirewallBackendIptables, false, 60)
	if err == nil {
		t.Fatalf("expected EnableSafe to abort when the agent allow fails")
	}
	if fake.sawSubstring("iptables -P INPUT DROP") {
		t.Errorf("enable must not run if the agent allow failed; calls=%v", fake.calls)
	}
}

func TestStatusNormalizesUFW(t *testing.T) {
	dump := "Status: active\nDefault: deny (incoming), allow (outgoing)\n\nTo                         Action      From\n--                         ------      ----\n2222/tcp                   ALLOW IN    Anywhere\n443/tcp                    ALLOW IN    Anywhere\n"
	fake := &fakeFirewall{respond: func(cmd string) (string, int, error) {
		if strings.Contains(cmd, "command -v 'ufw'") {
			return "yes", 0, nil
		}
		if strings.HasPrefix(cmd, "command -v") {
			return "no", 0, nil
		}
		if strings.Contains(cmd, "ufw status") {
			return dump, 0, nil
		}
		return "", 0, nil
	}}
	eng := &FirewallEngine{AgentPort: 2222, Exec: fake.exec}
	report, err := eng.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if report.Backend != FirewallBackendUFW {
		t.Errorf("backend = %q, want ufw", report.Backend)
	}
	if !report.Active {
		t.Errorf("expected Active=true")
	}
	if !report.DefaultDrop {
		t.Errorf("expected DefaultDrop=true")
	}
	if !report.AgentPortAllowed {
		t.Errorf("expected AgentPortAllowed=true for port 2222")
	}
}

func TestRulesetAllowsPort(t *testing.T) {
	cases := []struct {
		dump string
		port int
		want bool
	}{
		{"-A INPUT -p tcp --dport 2222 -j ACCEPT", 2222, true},
		{"tcp dport 2222 accept", 2222, true},
		{"2222/tcp                   ALLOW IN", 2222, true},
		{"ports: 2222 443", 2222, true},
		{"-P INPUT DROP", 2222, false},
		{"443/tcp ALLOW IN", 2222, false},
	}
	for i, tc := range cases {
		if got := rulesetAllowsPort(tc.dump, tc.port); got != tc.want {
			t.Errorf("case %d rulesetAllowsPort(%q,%d) = %v, want %v", i, tc.dump, tc.port, got, tc.want)
		}
	}
}

func TestScheduleSelfRevertCommand(t *testing.T) {
	cmd, unit, err := scheduleSelfRevertCommand("ufw --force disable", 45)
	if err != nil {
		t.Fatalf("scheduleSelfRevertCommand: %v", err)
	}
	if !strings.Contains(cmd, "systemd-run --on-active='45s'") {
		t.Errorf("missing on-active delay: %q", cmd)
	}
	if !strings.Contains(cmd, "/bin/sh -c 'ufw --force disable'") {
		t.Errorf("undo command not shell-quoted as a single arg: %q", cmd)
	}
	if !strings.Contains(cmd, "setsid sh -c 'sleep 45; ufw --force disable'") {
		t.Errorf("missing setsid fallback: %q", cmd)
	}
	if unit == "" {
		t.Errorf("expected a non-empty revert unit name")
	}
	// The unit name must be unique per invocation so a 2nd enable does not collide.
	if _, unit2, _ := scheduleSelfRevertCommand("ufw --force disable", 45); unit2 == unit {
		t.Errorf("revert unit name not unique: %q == %q", unit, unit2)
	}
	if _, _, err := scheduleSelfRevertCommand("", 60); err == nil {
		t.Errorf("expected error for empty undo command")
	}
}

func TestExecNilGuards(t *testing.T) {
	eng := &FirewallEngine{AgentPort: 2222}
	if _, err := eng.DetectBackend(); err == nil {
		t.Errorf("DetectBackend should error with nil Exec")
	}
	if _, err := eng.EnableSafe(FirewallBackendIptables, false, 60); err == nil {
		t.Errorf("EnableSafe should error with nil Exec")
	}
}

// Sanity: every supported backend produces non-empty enable/disable commands.
func TestEnableDisableCommandsNonEmpty(t *testing.T) {
	for _, b := range SortedBackends() {
		if strings.TrimSpace(enableCommand(b)) == "" {
			t.Errorf("enableCommand(%s) empty", b)
		}
		if strings.TrimSpace(disableCommand(b)) == "" {
			t.Errorf("disableCommand(%s) empty", b)
		}
	}
	_ = fmt.Sprint(FirewallBackendUnknown)
}

// TestNftablesUndoScopedToFleetTable is the regression for the LOW finding: the
// nftables self-revert undo must remove ONLY the fleet table, never the whole
// host ruleset. A `nft flush ruleset` would wipe every unrelated host firewall
// rule, so it must not appear in the undo command.
func TestNftablesUndoScopedToFleetTable(t *testing.T) {
	undo := disableCommand(FirewallBackendNftables)
	if strings.Contains(undo, "flush ruleset") {
		t.Fatalf("nftables undo still flushes the whole ruleset (wipes all host rules):\n%s", undo)
	}
	// It must delete the fleet table specifically.
	if !strings.Contains(undo, "delete table inet fleet") {
		t.Fatalf("nftables undo should delete only the fleet table, got:\n%s", undo)
	}
	// And it should first reopen the fleet chain policy so the host reopens even
	// before the table delete lands.
	if !strings.Contains(undo, "policy accept") {
		t.Fatalf("nftables undo should reset the fleet chain policy to accept, got:\n%s", undo)
	}
}
