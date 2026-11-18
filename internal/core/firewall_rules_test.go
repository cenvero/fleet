// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"strings"
	"testing"
)

const testAgentPort = 2222

func decide(t *testing.T, classify func(string, int) ruleEffect, rules ...string) agentPortDecision {
	t.Helper()
	return decideAgentPort(rules, testAgentPort, true, classify)
}

// The agent port counts as allowed only when a rule accepts it from anywhere,
// inbound, before anything that may block it — never because the number
// appears somewhere in a line.
func TestUFWRulesDecideTheAgentPort(t *testing.T) {
	allowed := [][]string{
		{"ufw allow 2222"},
		{"ufw allow 2222/tcp"},
		{"ufw allow in 2222/tcp"},
		{"ufw allow 22,2222/tcp"},
		{"ufw allow 2000:3000/tcp"},
		{"ufw allow proto tcp from any to any port 2222"},
		{"ufw allow from any to any port 2222 proto tcp"},
		{"ufw allow 2222/tcp comment 'fleet agent port'"},
		{"ufw allow 2222/tcp", "ufw deny from 10.0.0.0/8"}, // first match: the allow comes first
		{"ufw deny 443/tcp", "ufw allow 2222/tcp"},         // a deny on another port
		{"ufw deny 2222/udp", "ufw allow 2222/tcp"},        // UDP only
		{"ufw allow out 2222/tcp", "ufw allow 2222/tcp"},   // outbound rules do not decide
		{"ufw route allow 2222/tcp", "ufw allow 2222/tcp"}, // forwarding does not decide
		{"Added user rules (see 'ufw status' for running firewall):", "ufw allow 2222/tcp"},
	}
	for _, rules := range allowed {
		if d := decide(t, classifyUFWRule, rules...); !d.allowed {
			t.Errorf("%q: not allowed (blocked by %q, restricted %q)", rules, d.blockedBy, d.restricted)
		}
	}
	refused := []struct {
		rules []string
		why   string // blocked or restricted
	}{
		// The holes the review found: an allow that does not admit the controller.
		{[]string{"ufw allow out 2222/tcp"}, ""},
		{[]string{"ufw route allow 2222/tcp"}, ""},
		{[]string{"ufw allow from 10.9.9.9 to any port 2222"}, "restricted"},
		{[]string{"ufw allow in on tailscale0 to any port 2222"}, "restricted"},
		{[]string{"ufw allow proto udp to any port 2222"}, ""},
		{[]string{"ufw allow 22222/tcp"}, ""},
		{[]string{"ufw allow 22/tcp"}, ""},
		{[]string{"ufw allow ssh"}, ""},
		// ...and blocks that do not name the exact port, ahead of the allow.
		{[]string{"ufw deny 2000:3000/tcp", "ufw allow 2222/tcp"}, "blocked"},
		{[]string{"ufw deny 22,2222/tcp", "ufw allow 2222/tcp"}, "blocked"},
		{[]string{"ufw deny from 10.0.0.0/8", "ufw allow 2222/tcp"}, "blocked"},
		{[]string{"ufw deny in on eth0", "ufw allow 2222/tcp"}, "blocked"},
		{[]string{"ufw reject proto tcp from any to any port 2222", "ufw allow 2222/tcp"}, "blocked"},
		{[]string{"ufw limit 2222/tcp"}, "blocked"},
		{[]string{"ufw deny smtp", "ufw allow 2222/tcp"}, "blocked"},       // a service fleet does not resolve
		{[]string{"ufw frobnicate 2222", "ufw allow 2222/tcp"}, "blocked"}, // a line fleet cannot read
	}
	for _, tc := range refused {
		d := decide(t, classifyUFWRule, tc.rules...)
		if d.allowed {
			t.Errorf("%q: allowed, want refused", tc.rules)
			continue
		}
		switch tc.why {
		case "blocked":
			if d.blockedBy == "" {
				t.Errorf("%q: refused without naming the blocking rule", tc.rules)
			}
		case "restricted":
			if d.restricted == "" {
				t.Errorf("%q: refused without explaining the restricted allow", tc.rules)
			}
		}
	}
}

func TestIptablesRulesDecideTheAgentPort(t *testing.T) {
	allowed := [][]string{
		{"-A INPUT -p tcp -m tcp --dport 2222 -j ACCEPT"},
		{"-A INPUT -p tcp -m multiport --dports 22,2222 -j ACCEPT"},
		{"-A INPUT -p tcp --dport 2000:3000 -j ACCEPT"},
		{"-A INPUT -j ACCEPT"},
		{"-A INPUT -i lo -j ACCEPT", "-A INPUT -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT",
			"-A INPUT -m conntrack --ctstate INVALID -j DROP", "-A INPUT -p icmp -j ACCEPT",
			"-A INPUT -p tcp -m multiport --dports 22 -j f2b-sshd",
			`-A INPUT -p tcp -m tcp --dport 2222 -m comment --comment "fleet-fw-safe" -j ACCEPT`},
		{"-A INPUT -p udp --dport 2222 -j DROP", "-A INPUT -p tcp --dport 2222 -j ACCEPT"},
	}
	for _, rules := range allowed {
		if d := decide(t, classifyIptablesRule, rules...); !d.allowed {
			t.Errorf("%q: not allowed (blocked by %q)", rules, d.blockedBy)
		}
	}
	refused := [][]string{
		{"-A INPUT -s 10.9.9.9/32 -p tcp --dport 2222 -j ACCEPT"},
		{"-A INPUT -i wg0 -p tcp --dport 2222 -j ACCEPT"},
		{"-A INPUT -p tcp --sport 2222 -j ACCEPT"},
		{"-A INPUT -p udp --dport 2222 -j ACCEPT"},
		{"-A INPUT -p tcp --dport 2000:3000 -j DROP", "-A INPUT -p tcp --dport 2222 -j ACCEPT"},
		{"-A INPUT -s 10.0.0.0/8 -j DROP", "-A INPUT -p tcp --dport 2222 -j ACCEPT"},
		{"-A INPUT -j ufw-before-input", "-A INPUT -p tcp --dport 2222 -j ACCEPT"}, // another chain decides
		{"-A INPUT -p tcp --dport 2222 -m limit --limit 5/min -j ACCEPT"},
	}
	for _, rules := range refused {
		if d := decide(t, classifyIptablesRule, rules...); d.allowed {
			t.Errorf("%q: allowed, want refused", rules)
		}
	}
}

func TestNftRulesDecideTheAgentPort(t *testing.T) {
	allowed := [][]string{
		{"tcp dport 2222 accept"},
		{"tcp dport { 22, 2222 } accept"},
		{"tcp dport 2000-3000 accept"},
		{`iifname "lo" accept`, "ct state established,related accept", "ct state invalid drop",
			"meta l4proto { icmp, ipv6-icmp } accept", "tcp dport 2222 ct state new accept"},
		{"udp dport 2222 drop", "tcp dport 2222 accept"},
	}
	for _, rules := range allowed {
		if d := decide(t, classifyNftRule, rules...); !d.allowed {
			t.Errorf("%q: not allowed (blocked by %q)", rules, d.blockedBy)
		}
	}
	refused := [][]string{
		{"ip saddr 10.9.9.9 tcp dport 2222 accept"},
		{`iifname "wg0" tcp dport 2222 accept`},
		{"udp dport 2222 accept"},
		{"tcp dport @agent_ports accept"},
		{"tcp dport 2000-3000 drop", "tcp dport 2222 accept"},
		{"ip saddr 10.0.0.0/8 drop", "tcp dport 2222 accept"},
		{"jump extra", "tcp dport 2222 accept"},
	}
	for _, rules := range refused {
		if d := decide(t, classifyNftRule, rules...); d.allowed {
			t.Errorf("%q: allowed, want refused", rules)
		}
	}
}

func TestFirewalldZoneDecidesTheAgentPort(t *testing.T) {
	zone := func(extra string) string {
		return "public (active)\n  target: default\n  interfaces: eth0\n  sources: \n  services: ssh\n" + extra +
			"  protocols: \n  forward-ports: \n  source-ports: \n  icmp-blocks: \n  rich rules: \n"
	}
	for _, z := range []string{
		zone("  ports: 2222/tcp 443/tcp\n"),
		zone("  ports: 2000-3000/tcp\n"),
		zone("  ports: 443/tcp 2222/tcp 53/udp\n"),
	} {
		if d := firewalldDecision(z, testAgentPort); !d.allowed {
			t.Errorf("not allowed (blocked by %q): %s", d.blockedBy, z)
		}
	}
	for _, z := range []string{
		zone("  ports: 22222/tcp\n"),
		zone("  ports: 2222/udp\n"),
		zone("  ports: 2222/tcp\n") + "\trule family=\"ipv4\" source address=\"10.0.0.0/8\" reject\n",
		zone("  ports: 2222/tcp\n") + "\trule family=\"ipv4\" port port=\"2000-3000\" protocol=\"tcp\" drop\n",
	} {
		if d := firewalldDecision(z, testAgentPort); d.allowed {
			t.Errorf("allowed, want refused: %s", z)
		}
	}
	// source-ports alone must not count (only the ports: line does).
	z := strings.Replace(zone(""), "  source-ports: \n", "  source-ports: 2222/tcp\n", 1)
	if d := firewalldDecision(z, testAgentPort); d.allowed {
		t.Error("source-ports 2222/tcp counted as allowing the agent port")
	}
}

// The end-to-end decision the enable gate makes from a ufw dump, including
// the two holes: a range deny and an outbound-only allow.
func TestUFWGuardRefusesTheReviewedHoles(t *testing.T) {
	link := AgentLink{Server: "web-01", Port: testAgentPort}
	dump := func(added string) string {
		return "Status: inactive\n" + guardMarker + "added\n" + ufwAddedHeader + added
	}
	for name, added := range map[string]string{
		"range deny ahead of the allow": "ufw deny 2000:3000/tcp\nufw allow 2222/tcp\n",
		"address deny ahead":            "ufw deny from 10.0.0.0/8\nufw allow 2222/tcp\n",
		"outbound-only allow":           "ufw allow out 2222/tcp\n",
		"allow from one host":           "ufw allow from 10.9.9.9 to any port 2222\n",
	} {
		st := parseGuardState(FirewallBackendUFW, dump(added), nil, testAgentPort)
		if refusal := enableLockout(link, st, FixPortOpenCommand("web-01", testAgentPort)); refusal == nil {
			t.Errorf("%s: enabling was allowed", name)
		}
	}
	st := parseGuardState(FirewallBackendUFW, dump("ufw allow 2222/tcp\nufw deny from 10.0.0.0/8\n"), nil, testAgentPort)
	if refusal := enableLockout(link, st, ""); refusal != nil {
		t.Errorf("allow first, then a broad deny: refused: %v", refusal)
	}
	// Without `ufw show added`, fleet cannot read the rules: refuse.
	st = parseGuardState(FirewallBackendUFW, "Status: active\n\nTo Action From\n2222/tcp ALLOW IN Anywhere\n", nil, testAgentPort)
	if refusal := enableLockout(link, st, ""); refusal == nil {
		t.Error("allowed without `ufw show added`")
	}
}

// A server's output in firewall errors cannot carry terminal escape
// sequences to the operator's terminal.
func TestFirewallErrorsStripTerminalEscapes(t *testing.T) {
	eng := &FirewallEngine{Exec: func(string) (string, int, error) {
		return "ufw: \x1b]0;owned\x07\x1b[2Jbad rule", 1, nil
	}}
	_, err := eng.run("ufw status")
	if err == nil || strings.ContainsRune(err.Error(), 0x1b) || strings.ContainsRune(err.Error(), 0x07) {
		t.Fatalf("error carries control characters: %q", err)
	}
	if line := firstLine("\x1b[31mStatus: weird\n"); strings.ContainsRune(line, 0x1b) {
		t.Fatalf("firstLine carries control characters: %q", line)
	}
}
