// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// guardHost is a programmable host for the firewall engine. It answers each
// command from the first rule whose substring matches and records every
// command, so a test can assert what ran and in which order.
type guardHost struct {
	calls   []string
	answers []guardAnswer
}

type guardAnswer struct {
	contains string
	out      string
	code     int
	err      error
}

func (h *guardHost) on(contains, out string) *guardHost {
	h.answers = append(h.answers, guardAnswer{contains: contains, out: out})
	return h
}

func (h *guardHost) fail(contains, out string, code int) *guardHost {
	h.answers = append(h.answers, guardAnswer{contains: contains, out: out, code: code})
	return h
}

func (h *guardHost) exec(cmd string) (string, int, error) {
	h.calls = append(h.calls, cmd)
	for _, a := range h.answers {
		if strings.Contains(cmd, a.contains) {
			return a.out, a.code, a.err
		}
	}
	return "", 0, nil
}

func (h *guardHost) ran(sub string) bool { return h.index(sub) >= 0 }

func (h *guardHost) index(sub string) int {
	for i, c := range h.calls {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

const (
	armedSystemd = "fleet-fw-safe-revert:armed:systemd-run"
	armedSetsid  = "fleet-fw-safe-revert:armed:setsid"
)

// readOnlyFirewallCommand reports whether cmd only reads host state: the
// backend's ruleset dump. Everything else may change the host.
func readOnlyFirewallCommand(cmd string) bool {
	for _, prefix := range []string{"iptables -S", "nft list ruleset", "ufw status verbose", "if firewall-cmd --state >/dev/null 2>&1; then echo", "firewall-cmd --list-all"} {
		if strings.HasPrefix(cmd, prefix) {
			return true
		}
	}
	return false
}

func assertNothingChanged(t *testing.T, h *guardHost) {
	t.Helper()
	for _, c := range h.calls {
		if !readOnlyFirewallCommand(c) {
			t.Fatalf("a refused enable must not change anything, but it ran %q (all calls: %q)", c, h.calls)
		}
	}
}

// The gate runs BEFORE any change: when the agent port is not allowed the
// engine refuses, names the command to run first, and runs nothing but the read.
func TestEnableSafeRefusesBeforeChangingAnything(t *testing.T) {
	host := (&guardHost{}).on("iptables -S", "-P INPUT ACCEPT\n-A INPUT -p tcp -m tcp --dport 22 -j ACCEPT\n")
	eng := &FirewallEngine{AgentPort: 2222, Exec: host.exec}
	_, err := eng.EnableSafe(FirewallBackendIptables, false, 60)
	if err == nil {
		t.Fatal("EnableSafe must refuse when the agent port is not allowed")
	}
	for _, want := range []string{"fleet fw allow", "2222/tcp", "--force-i-have-console"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should name %q; got: %v", want, err)
		}
	}
	assertNothingChanged(t, host)
}

// A rule that drops the agent port makes the port blocked even when an
// accept for it also exists, so the gate refuses.
func TestEnableSafeRefusesWhenARuleDropsTheAgentPort(t *testing.T) {
	host := (&guardHost{}).on("iptables -S", "-P INPUT ACCEPT\n-A INPUT -p tcp -m tcp --dport 2222 -j DROP\n-A INPUT -p tcp -m tcp --dport 2222 -j ACCEPT\n")
	eng := &FirewallEngine{AgentPort: 2222, Exec: host.exec}
	if _, err := eng.EnableSafe(FirewallBackendIptables, false, 60); err == nil {
		t.Fatal("EnableSafe must refuse while a rule drops the agent port")
	}
	assertNothingChanged(t, host)
}

// nft: enable only sets the policy of fleet's own `inet fleet` input chain, and
// an accept in a different table does not stop that chain from dropping the
// packet. The agent port must be accepted in the fleet chain itself.
func TestEnableSafeNftChecksTheFleetChain(t *testing.T) {
	ruleset := "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n\t\ttcp dport 2222 accept\n\t}\n}\n"
	host := (&guardHost{}).on("nft list ruleset", ruleset)
	eng := &FirewallEngine{AgentPort: 2222, Exec: host.exec}
	_, err := eng.EnableSafe(FirewallBackendNftables, false, 60)
	if err == nil {
		t.Fatal("EnableSafe must refuse: the fleet chain does not accept the agent port")
	}
	if !strings.Contains(err.Error(), "fleet fw allow") {
		t.Errorf("refusal should name `fleet fw allow`; got: %v", err)
	}
	assertNothingChanged(t, host)
}

const ufwAddedHeader = "Added user rules (see 'ufw status' for running firewall):\n"

// ufw prints only "Status: inactive" while it is off, so the pending rules come
// from `ufw show added`. Refuse while the agent port is missing there, then pass
// once `fleet fw allow` has added it: refuse -> remediate -> retry.
func TestEnableSafeUFWInactiveRefuseThenAllowThenEnable(t *testing.T) {
	added := ufwAddedHeader + "(None)\n"
	host := &guardHost{}
	host.answers = []guardAnswer{{contains: "systemd-run", out: armedSystemd}}
	dump := func() string { return "Status: inactive\n#fleet-guard:added\n" + added }
	respond := func(cmd string) (string, int, error) {
		if strings.Contains(cmd, "ufw status verbose") {
			host.calls = append(host.calls, cmd)
			return dump(), 0, nil
		}
		return host.exec(cmd)
	}
	eng := &FirewallEngine{AgentPort: 2222, Exec: respond}

	_, err := eng.EnableSafe(FirewallBackendUFW, false, 60)
	if err == nil || !strings.Contains(err.Error(), "fleet fw allow") || !strings.Contains(err.Error(), "2222/tcp") {
		t.Fatalf("first enable must refuse and name `fleet fw allow ... 2222/tcp`; got: %v", err)
	}
	if host.ran("ufw --force enable") || host.ran("ufw allow") {
		t.Fatalf("a refused enable must not change ufw; calls=%q", host.calls)
	}

	if _, err := eng.AllowPort(FirewallBackendUFW, PortSpec{Port: 2222, Proto: ProtoTCP}); err != nil {
		t.Fatalf("AllowPort: %v", err)
	}
	// `ufw allow 2222` is listed by `ufw show added` without a protocol.
	added = ufwAddedHeader + "ufw allow 2222\n"

	res, err := eng.EnableSafe(FirewallBackendUFW, false, 60)
	if err != nil {
		t.Fatalf("enable after allowing the agent port: %v", err)
	}
	if !host.ran("ufw --force enable") {
		t.Fatalf("ufw was never enabled; calls=%q", host.calls)
	}
	if res.UndoCmd != "ufw --force disable" {
		t.Errorf("ufw was inactive before, so the undo turns it back off; undo=%q", res.UndoCmd)
	}
}

// ufw was already active: enable changes nothing, so the undo must not turn
// the firewall off.
func TestEnableSafeUFWAlreadyActiveUndoKeepsItOn(t *testing.T) {
	dump := "Status: active\nDefault: deny (incoming), allow (outgoing), disabled (routed)\n\nTo                         Action      From\n--                         ------      ----\n2222/tcp                   ALLOW IN    Anywhere\n#fleet-guard:added\n" + ufwAddedHeader + "ufw allow 2222/tcp\n"
	host := (&guardHost{}).on("ufw status verbose", dump).on("systemd-run", armedSystemd)
	eng := &FirewallEngine{AgentPort: 2222, Exec: host.exec}
	res, err := eng.EnableSafe(FirewallBackendUFW, false, 60)
	if err != nil {
		t.Fatalf("EnableSafe: %v", err)
	}
	if strings.Contains(res.UndoCmd, "disable") {
		t.Fatalf("ufw was already active; the undo must not disable it: %q", res.UndoCmd)
	}
}

func TestEnableSafeIptablesAcceptsEstablishedAndLoopbackBeforeDrop(t *testing.T) {
	host := (&guardHost{}).on("iptables -S", "-P INPUT ACCEPT\n-A INPUT -p tcp -m tcp --dport 2222 -j ACCEPT\n").on("systemd-run", armedSystemd)
	eng := &FirewallEngine{AgentPort: 2222, Exec: host.exec}
	res, err := eng.EnableSafe(FirewallBackendIptables, false, 60)
	if err != nil {
		t.Fatalf("EnableSafe: %v", err)
	}
	cmd := res.EnableCmd
	drop := strings.Index(cmd, "iptables -P INPUT DROP")
	est := strings.Index(cmd, "iptables -I INPUT 1 -m conntrack --ctstate ESTABLISHED,RELATED")
	lo := strings.Index(cmd, "iptables -I INPUT 1 -i lo")
	if drop < 0 || est < 0 || lo < 0 {
		t.Fatalf("enable must insert established/related and loopback accepts and then drop; got:\n%s", cmd)
	}
	if est > drop || lo > drop {
		t.Fatalf("the accepts must come before the drop policy; got:\n%s", cmd)
	}
	// Idempotent: each accept is checked with -C before it is inserted, and the
	// drop only runs once both accepts are in place.
	for _, want := range []string{
		"iptables -C INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
		"iptables -C INPUT -i lo -j ACCEPT",
		"&& iptables -P INPUT DROP",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("enable command missing %q:\n%s", want, cmd)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(cmd), "iptables -P INPUT DROP") {
		t.Errorf("the drop policy must be the last step:\n%s", cmd)
	}
}

func TestEnableSafeNftAcceptsEstablishedAndLoopbackBeforeDrop(t *testing.T) {
	ruleset := "table inet fleet {\n\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n\t\ttcp dport 2222 accept\n\t}\n}\n"
	host := (&guardHost{}).on("nft list ruleset", ruleset).on("systemd-run", armedSystemd)
	eng := &FirewallEngine{AgentPort: 2222, Exec: host.exec}
	res, err := eng.EnableSafe(FirewallBackendNftables, false, 60)
	if err != nil {
		t.Fatalf("EnableSafe: %v", err)
	}
	cmd := res.EnableCmd
	drop := strings.Index(cmd, "policy drop")
	est := strings.Index(cmd, "nft insert rule inet fleet input ct state established,related accept")
	lo := strings.Index(cmd, "nft insert rule inet fleet input iif lo accept")
	if drop < 0 || est < 0 || lo < 0 {
		t.Fatalf("enable must accept established/related and loopback, then set policy drop; got:\n%s", cmd)
	}
	if est > drop || lo > drop {
		t.Fatalf("the accepts must come before the drop policy; got:\n%s", cmd)
	}
	for _, want := range []string{
		"grep -qF -- 'ct state established,related accept' || nft insert rule",
		`grep -qF -- 'iif "lo" accept' || nft insert rule`,
		"&& nft chain inet fleet input '{ policy drop ; }'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("enable command missing %q:\n%s", want, cmd)
		}
	}
}

// The undo reverses what enable added. iptables: restore the INPUT policy the
// host had before (not a blanket ACCEPT) and delete only the rules enable
// inserted.
func TestEnableSafeIptablesUndoRestoresThePriorPolicy(t *testing.T) {
	for _, prior := range []string{"ACCEPT", "DROP"} {
		host := (&guardHost{}).on("iptables -S", "-P INPUT "+prior+"\n-A INPUT -p tcp -m tcp --dport 2222 -j ACCEPT\n").on("systemd-run", armedSystemd)
		eng := &FirewallEngine{AgentPort: 2222, Exec: host.exec}
		res, err := eng.EnableSafe(FirewallBackendIptables, false, 60)
		if err != nil {
			t.Fatalf("prior %s: EnableSafe: %v", prior, err)
		}
		undo := res.UndoCmd
		if !strings.HasPrefix(undo, "iptables -P INPUT "+prior+";") {
			t.Errorf("prior %s: the undo must first restore -P INPUT %s; got %q", prior, prior, undo)
		}
		for _, want := range []string{
			"iptables -D INPUT -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment fleet-fw-safe -j ACCEPT",
			"iptables -D INPUT -i lo -m comment --comment fleet-fw-safe -j ACCEPT",
		} {
			if !strings.Contains(undo, want) {
				t.Errorf("prior %s: undo must delete the rule enable inserted (%q); got %q", prior, want, undo)
			}
		}
		if strings.Contains(undo, "--dport") {
			t.Errorf("prior %s: the undo must never delete an allow rule; got %q", prior, undo)
		}
	}
}

// nft: the `inet fleet` table may already hold `fleet fw allow` rules. The
// undo restores the chain policy and deletes only enable's tagged rules; it
// does not delete the table those allow rules live in.
func TestEnableSafeNftUndoKeepsAPreexistingFleetTable(t *testing.T) {
	ruleset := "table inet fleet {\n\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n\t\ttcp dport 2222 accept\n\t\ttcp dport 443 accept\n\t}\n}\n"
	host := (&guardHost{}).on("nft list ruleset", ruleset).on("systemd-run", armedSystemd)
	eng := &FirewallEngine{AgentPort: 2222, Exec: host.exec}
	res, err := eng.EnableSafe(FirewallBackendNftables, false, 60)
	if err != nil {
		t.Fatalf("EnableSafe: %v", err)
	}
	undo := res.UndoCmd
	if strings.Contains(undo, "delete table") {
		t.Fatalf("the fleet table existed before enable; the undo must not delete it: %q", undo)
	}
	if !strings.HasPrefix(undo, "nft chain inet fleet input '{ policy accept ; }'") {
		t.Errorf("the undo must first restore the prior policy (accept); got %q", undo)
	}
	if !strings.Contains(undo, `comment "fleet-fw-safe"`) || !strings.Contains(undo, "nft delete rule inet fleet input handle") {
		t.Errorf("the undo must delete enable's tagged rules by handle; got %q", undo)
	}
}

const firewalldZone = "public (active)\n  target: default\n  interfaces: eth0\n  services: dhcpv6-client ssh\n  ports: 2222/tcp\n  protocols: \n"

// firewalld was already running and enabled at boot: enable changes nothing,
// so the undo must not stop it.
func TestEnableSafeFirewalldRunningUndoDoesNotStopIt(t *testing.T) {
	dump := "#fleet-guard:firewalld-running\n" + firewalldZone + "#fleet-guard:permanent\n" + firewalldZone + "#fleet-guard:unit enabled\n"
	host := (&guardHost{}).on("--list-all", dump).on("systemd-run", armedSystemd)
	eng := &FirewallEngine{AgentPort: 2222, Exec: host.exec}
	res, err := eng.EnableSafe(FirewallBackendFirewalld, false, 60)
	if err != nil {
		t.Fatalf("EnableSafe: %v", err)
	}
	if strings.Contains(res.UndoCmd, "stop") {
		t.Fatalf("firewalld was already running; the undo must not stop it: %q", res.UndoCmd)
	}
}

// firewalld stopped: `firewall-cmd --permanent` needs the daemon, so nothing
// may call it. The agent port is read from the offline permanent config, and
// the undo stops and disables the daemon enable started.
func TestEnableSafeFirewalldFromStopped(t *testing.T) {
	dump := "#fleet-guard:firewalld-stopped\n#fleet-guard:permanent\n" + firewalldZone + "#fleet-guard:unit disabled\n"
	host := (&guardHost{}).
		on("--list-all", dump).
		fail("firewall-cmd --permanent", "FirewallD is not running", 252).
		on("systemd-run", armedSystemd)
	eng := &FirewallEngine{AgentPort: 2222, Exec: host.exec}
	res, err := eng.EnableSafe(FirewallBackendFirewalld, false, 60)
	if err != nil {
		t.Fatalf("EnableSafe from stopped firewalld: %v", err)
	}
	if !host.ran("systemctl enable --now firewalld") {
		t.Errorf("enable never started firewalld; calls=%q", host.calls)
	}
	if res.UndoCmd != "systemctl disable --now firewalld" {
		t.Errorf("firewalld was stopped and disabled, so the undo is `systemctl disable --now firewalld`; got %q", res.UndoCmd)
	}
}

// `fw allow` on firewalld must not `--reload`: a reload replaces the running
// rules with the permanent ones and drops any runtime-only allow, which can be
// the agent's. It also works while firewalld is stopped.
func TestAllowPortFirewalldNeverReloads(t *testing.T) {
	cmd, err := allowPortCommand(FirewallBackendFirewalld, PortSpec{Port: 443, Proto: ProtoTCP})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cmd, "--reload") {
		t.Fatalf("firewalld allow must not reload: %q", cmd)
	}
	for _, want := range []string{
		"firewall-cmd --permanent --add-port='443/tcp' && firewall-cmd --add-port='443/tcp'",
		"firewall-offline-cmd --add-port='443/tcp'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("firewalld allow missing %q: %q", want, cmd)
		}
	}
}

// The schedule command reports which path armed the timer. On the setsid
// path there is no systemd unit, so none may be reported.
func TestEnableSafeRevertPathIsReportedFromTheHost(t *testing.T) {
	iptablesOK := "-P INPUT ACCEPT\n-A INPUT -p tcp -m tcp --dport 2222 -j ACCEPT\n"

	host := (&guardHost{}).on("iptables -S", iptablesOK).on("systemd-run", armedSetsid)
	res, err := (&FirewallEngine{AgentPort: 2222, Exec: host.exec}).EnableSafe(FirewallBackendIptables, false, 60)
	if err != nil {
		t.Fatalf("setsid path: %v", err)
	}
	if !res.UndoScheduled || res.RevertUnit != "" {
		t.Errorf("setsid path: want armed with no systemd unit; got scheduled=%t unit=%q", res.UndoScheduled, res.RevertUnit)
	}

	host = (&guardHost{}).on("iptables -S", iptablesOK).on("systemd-run", armedSystemd)
	res, err = (&FirewallEngine{AgentPort: 2222, Exec: host.exec}).EnableSafe(FirewallBackendIptables, false, 60)
	if err != nil {
		t.Fatalf("systemd path: %v", err)
	}
	if !res.UndoScheduled || !strings.HasPrefix(res.RevertUnit, "fleet-fw-safe-revert-") {
		t.Errorf("systemd path: want armed with the unit name; got scheduled=%t unit=%q", res.UndoScheduled, res.RevertUnit)
	}
}

// Exit 0 with no marker means nothing confirmed the timer, so the lockdown must
// not go ahead.
func TestEnableSafeRequiresConfirmedArming(t *testing.T) {
	host := (&guardHost{}).on("iptables -S", "-P INPUT ACCEPT\n-A INPUT -p tcp -m tcp --dport 2222 -j ACCEPT\n")
	_, err := (&FirewallEngine{AgentPort: 2222, Exec: host.exec}).EnableSafe(FirewallBackendIptables, false, 60)
	if err == nil || !strings.Contains(err.Error(), "auto-revert") {
		t.Fatalf("an unconfirmed timer must refuse the enable; got %v", err)
	}
	if host.ran("iptables -P INPUT DROP") {
		t.Fatalf("enable ran without a confirmed self-revert; calls=%q", host.calls)
	}
}

// A backgrounded `setsid` that is missing still exits 0, so the fallback must
// check for setsid first and fail when neither tool exists. The setsid job
// carries the revert id as $0 so it can be found and cancelled.
func TestScheduleSelfRevertChecksForSetsid(t *testing.T) {
	cmd, id, err := scheduleSelfRevertCommand("ufw --force disable", 30)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"elif command -v setsid >/dev/null 2>&1; then setsid sh -c 'sleep 30; ufw --force disable' '" + id + "'",
		"echo '" + armedSetsid + "'",
		"echo '" + armedSystemd + "'",
		"exit 1",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("schedule command missing %q:\n%s", want, cmd)
		}
	}
}

// The schedule command run through a real /bin/sh, with fake systemd-run and
// setsid on PATH: each path prints its marker, a failing systemd-run falls back
// to setsid, and with neither tool the command fails instead of reporting a
// timer that does not exist.
func TestScheduleSelfRevertInARealShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a Unix /bin/sh")
	}
	cmd, id, err := scheduleSelfRevertCommand("true", 30)
	if err != nil {
		t.Fatal(err)
	}
	fake := func(dir, name string, exit int) string {
		t.Helper()
		argsFile := filepath.Join(dir, name+".args")
		script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %s\nexit %d\n", shellQuote(argsFile), exit)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil { // #nosec G306 -- test fake must be executable
			t.Fatal(err)
		}
		return argsFile
	}
	run := func(dir string) (string, int) {
		t.Helper()
		c := exec.Command("/bin/sh", "-c", cmd) // #nosec G204 -- the command under test, run against fakes
		c.Env = []string{"PATH=" + dir}
		out, err := c.CombinedOutput()
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("run schedule command: %v", err)
		}
		return string(out), code
	}
	readArgs := func(path string) string {
		t.Helper()
		for i := 0; i < 100; i++ { // the setsid job runs in the background
			if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
				return string(b)
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s was never written", path)
		return ""
	}

	dir := t.TempDir()
	systemdArgs := fake(dir, "systemd-run", 0)
	fake(dir, "setsid", 0)
	if out, code := run(dir); code != 0 || revertMethodFrom(out) != RevertMethodSystemd {
		t.Fatalf("systemd-run path: exit %d, output %q", code, out)
	}
	if got := readArgs(systemdArgs); got != "--on-active=30s\n--unit="+id+"\n/bin/sh\n-c\ntrue\n" {
		t.Errorf("systemd-run got args %q", got)
	}

	dir = t.TempDir()
	fake(dir, "systemd-run", 1) // present but failing: fall back to setsid
	setsidArgs := fake(dir, "setsid", 0)
	if out, code := run(dir); code != 0 || revertMethodFrom(out) != RevertMethodSetsid {
		t.Fatalf("setsid fallback: exit %d, output %q", code, out)
	}
	if got := readArgs(setsidArgs); got != "sh\n-c\nsleep 30; true\n"+id+"\n" {
		t.Errorf("setsid got args %q", got)
	}
	if cancel := cancelRevertCommand(RevertMethodSetsid, id); cancel != `pkill -f "[f]`+id[1:]+`"` {
		t.Errorf("setsid cancel command = %q", cancel)
	}

	if out, code := run(t.TempDir()); code == 0 || revertMethodFrom(out) != "" {
		t.Fatalf("with neither tool the command must fail and report nothing armed; exit %d, output %q", code, out)
	}
}

// fleet does not know the direct agent's port: no override can make enable
// safe (it could not even allow that port first), and the refusal is typed so
// callers audit it.
func TestEnableSafeUnknownAgentPortIsUnskippable(t *testing.T) {
	host := &guardHost{}
	_, err := (&FirewallEngine{AgentPort: 0, Server: "web-01", Exec: host.exec}).EnableSafe(FirewallBackendIptables, true, 60)
	var refusal *AgentLockoutError
	if !errors.As(err, &refusal) || !refusal.Unskippable || refusal.Op != "fw.enable" || refusal.Server != "web-01" {
		t.Fatalf("want an unskippable fw.enable refusal; got %#v (%v)", refusal, err)
	}
	if len(host.calls) != 0 {
		t.Fatalf("nothing may run; calls=%q", host.calls)
	}
}

// --force-i-have-console still injects the agent allow first, and that allow is
// idempotent (checked with -C before inserting) so repeated runs don't stack
// duplicate rules.
func TestEnableSafeForceInjectsIdempotentAgentAllowBeforeEnable(t *testing.T) {
	host := (&guardHost{}).on("iptables -S", "-P INPUT ACCEPT\n").on("systemd-run", armedSystemd)
	res, err := (&FirewallEngine{AgentPort: 2222, Exec: host.exec}).EnableSafe(FirewallBackendIptables, true, 60)
	if err != nil {
		t.Fatalf("EnableSafe(force): %v", err)
	}
	allow := host.index("iptables -C INPUT -p 'tcp' --dport '2222' -j ACCEPT 2>/dev/null || iptables -I INPUT 1 -p 'tcp' --dport '2222' -j ACCEPT")
	arm := host.index("systemd-run")
	enable := host.index("iptables -P INPUT DROP")
	if allow < 0 || arm < 0 || enable < 0 || !(allow < arm && arm < enable) {
		t.Fatalf("want idempotent agent allow, then arm, then enable; got allow=%d arm=%d enable=%d calls=%q", allow, arm, enable, host.calls)
	}
	if res.AgentAllowCmd == "" {
		t.Errorf("the injected allow should be reported")
	}
}
