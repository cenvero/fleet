// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"strings"
	"testing"
)

const testAgentPort = 2222

func decide(t *testing.T, classify func(string, int) ruleMatch, rules ...string) agentPortDecision {
	t.Helper()
	return decideAgentPort(rules, testAgentPort, true, classify, agentPeers{})
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
		if d := firewalldDecision(z, testAgentPort, agentPeers{}); !d.allowed {
			t.Errorf("not allowed (blocked by %q): %s", d.blockedBy, z)
		}
	}
	for _, z := range []string{
		zone("  ports: 22222/tcp\n"),
		zone("  ports: 2222/udp\n"),
		zone("  ports: 2222/tcp\n") + "\trule family=\"ipv4\" source address=\"10.0.0.0/8\" reject\n",
		zone("  ports: 2222/tcp\n") + "\trule family=\"ipv4\" port port=\"2000-3000\" protocol=\"tcp\" drop\n",
	} {
		if d := firewalldDecision(z, testAgentPort, agentPeers{}); d.allowed {
			t.Errorf("allowed, want refused: %s", z)
		}
	}
	// source-ports alone must not count (only the ports: line does).
	z := strings.Replace(zone(""), "  source-ports: \n", "  source-ports: 2222/tcp\n", 1)
	if d := firewalldDecision(z, testAgentPort, agentPeers{}); d.allowed {
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

// With firewalld running, the agent port must be allowed in every active
// zone: the controller's traffic may arrive through any of them.
func TestFirewalldGuardChecksEveryActiveZone(t *testing.T) {
	link := AgentLink{Server: "web-01", Port: testAgentPort}
	zone := func(name, ports string) string {
		return name + " (active)\n  target: default\n  interfaces: eth0\n  ports: " + ports + "\n  rich rules: \n"
	}
	dump := func(zones string) string {
		return guardMarker + "firewalld-running\n" + zone("public", "2222/tcp") +
			guardMarker + "permanent\n" + zone("public", "2222/tcp") +
			guardMarker + "zones\n" + zones + guardMarker + "unit enabled\n"
	}
	ok := parseGuardState(FirewallBackendFirewalld, dump(zone("public", "2222/tcp")+zone("internal", "2222/tcp 80/tcp")), nil, testAgentPort)
	if refusal := enableLockout(link, ok, ""); refusal != nil {
		t.Fatalf("allowed in every zone, refused: %v", refusal)
	}
	missing := parseGuardState(FirewallBackendFirewalld, dump(zone("public", "2222/tcp")+zone("internal", "80/tcp")), nil, testAgentPort)
	if refusal := enableLockout(link, missing, ""); refusal == nil {
		t.Fatal("a zone without the agent port was not refused")
	}
}

// What the server lists while fleet is connected from 203.0.113.7 (seen
// after NAT) to its public address on eth0, and from 100.64.0.9 over
// Tailscale.
const (
	ssFromController = "Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n" +
		"0      0      10.0.0.5:2222      203.0.113.7:51514\n" +
		"0      0      10.0.0.5:22        198.51.100.1:40000\n" // another service: not the agent port
	ssFromTailscale = "0 0 100.64.0.5:2222 100.64.0.9:60111\n"
	ipAddrs         = "1: lo    inet 127.0.0.1/8 scope host lo\n" +
		"2: eth0    inet 10.0.0.5/24 brd 10.0.0.255 scope global eth0\n" +
		"3: tailscale0    inet 100.64.0.5/32 scope global tailscale0\n"
)

func peersFrom(t *testing.T, ss string) agentPeers {
	t.Helper()
	p := parseAgentPeers(ss, ipAddrs, testAgentPort)
	if !p.known {
		t.Fatalf("no connections to the agent port read from %q", ss)
	}
	return p
}

func TestAgentPeersAreReadFromSSAndNetstat(t *testing.T) {
	p := peersFrom(t, ssFromController)
	if len(p.conns) != 1 || p.conns[0].remote.String() != "203.0.113.7" || p.conns[0].iface != "eth0" {
		t.Fatalf("ss: %+v", p.conns)
	}
	netstat := "tcp        0      0 10.0.0.5:2222           203.0.113.7:51514       ESTABLISHED\n" +
		"tcp6       0      0 ::ffff:10.0.0.5:2222    ::ffff:203.0.113.7:51515 ESTABLISHED\n"
	p = peersFrom(t, netstat)
	if len(p.conns) != 2 || p.conns[1].remote.String() != "203.0.113.7" {
		t.Fatalf("netstat: %+v", p.conns)
	}
	v6 := "0 0 [2001:db8::5]:2222 [2001:db8:1::7]:51514\n"
	if p := parseAgentPeers(v6, "", testAgentPort); !p.known || p.conns[0].remote.String() != "2001:db8:1::7" {
		t.Fatalf("ipv6: %+v", p)
	}
	if p := parseAgentPeers("", ipAddrs, testAgentPort); p.known {
		t.Fatal("no listing counted as known")
	}
}

// Allowing the agent port only from the controller's address, its network,
// or only on the interface its connection uses, is confirmed — once the
// server shows fleet's connection coming from there.
func TestRestrictedAllowsAreConfirmedAgainstTheController(t *testing.T) {
	ctrl := peersFrom(t, ssFromController)
	ts := peersFrom(t, ssFromTailscale)
	cases := []struct {
		name     string
		classify func(string, int) ruleMatch
		rules    []string
		peers    agentPeers
		allowed  bool
	}{
		{"ufw from the controller's IP", classifyUFWRule, []string{"ufw allow from 203.0.113.7 to any port 2222 proto tcp"}, ctrl, true},
		{"ufw from the controller's network", classifyUFWRule, []string{"ufw allow from 203.0.113.0/24 to any port 2222"}, ctrl, true},
		{"ufw from another host", classifyUFWRule, []string{"ufw allow from 198.51.100.9 to any port 2222"}, ctrl, false},
		{"ufw to the address it reaches", classifyUFWRule, []string{"ufw allow from any to 10.0.0.5 port 2222"}, ctrl, true},
		{"ufw on its interface", classifyUFWRule, []string{"ufw allow in on tailscale0 to any port 2222"}, ts, true},
		{"ufw on another interface", classifyUFWRule, []string{"ufw allow in on tailscale0 to any port 2222"}, ctrl, false},
		{"ufw all ports from the controller", classifyUFWRule, []string{"ufw allow from 203.0.113.7"}, ctrl, true},
		// A blocklist entry that is not the controller does not stand in the way.
		{"ufw deny of another host first", classifyUFWRule, []string{"ufw deny from 192.0.2.66", "ufw allow 2222/tcp"}, ctrl, true},
		{"ufw deny of the controller first", classifyUFWRule, []string{"ufw deny from 203.0.113.0/24", "ufw allow 2222/tcp"}, ctrl, false},
		{"iptables -s the controller", classifyIptablesRule, []string{"-A INPUT -s 203.0.113.7/32 -p tcp -m tcp --dport 2222 -j ACCEPT"}, ctrl, true},
		{"iptables -s another host", classifyIptablesRule, []string{"-A INPUT -s 198.51.100.9/32 -p tcp --dport 2222 -j ACCEPT"}, ctrl, false},
		{"iptables -i its interface", classifyIptablesRule, []string{"-A INPUT -i tailscale0 -p tcp --dport 2222 -j ACCEPT"}, ts, true},
		{"nft saddr set with the controller", classifyNftRule, []string{"ip saddr { 192.0.2.1, 203.0.113.0/24 } tcp dport 2222 accept"}, ctrl, true},
		{"nft iifname its interface", classifyNftRule, []string{`iifname "tailscale0" tcp dport 2222 accept`}, ts, true},
		// Without the connection listing, a restricted allow still cannot be confirmed.
		{"no listing", classifyUFWRule, []string{"ufw allow from 203.0.113.7 to any port 2222"}, agentPeers{}, false},
	}
	for _, tc := range cases {
		d := decideAgentPort(tc.rules, testAgentPort, true, tc.classify, tc.peers)
		if d.allowed != tc.allowed {
			t.Errorf("%s: allowed = %v (blocked by %q, restricted %q), want %v", tc.name, d.allowed, d.blockedBy, d.restricted, tc.allowed)
		}
	}
	// Two connections, one from outside the allowed network: fleet cannot
	// tell which is its own, so it refuses.
	both := peersFrom(t, ssFromController+"0 0 10.0.0.5:2222 198.51.100.9:50000\n")
	if d := decideAgentPort([]string{"ufw allow from 203.0.113.0/24 to any port 2222"}, testAgentPort, true, classifyUFWRule, both); d.allowed {
		t.Error("allowed although one connection comes from outside the allowed network")
	}
}

// The whole check, from a ufw dump with the connection listing, and the
// refusal naming where the server sees fleet coming from.
func TestUFWGuardWithTheControllersAddress(t *testing.T) {
	link := AgentLink{Server: "web-01", Port: testAgentPort}
	dump := func(added string) string {
		return "Status: inactive\n" + guardMarker + "added\n" + ufwAddedHeader + added +
			guardMarker + "peers\n" + ssFromController + guardMarker + "addrs\n" + ipAddrs
	}
	st := parseGuardState(FirewallBackendUFW, dump("ufw allow from 203.0.113.7 to any port 2222\n"), nil, testAgentPort)
	if refusal := enableLockout(link, st, ""); refusal != nil {
		t.Fatalf("allowed only from the controller's own IP, refused: %v", refusal)
	}
	st = parseGuardState(FirewallBackendUFW, dump("ufw allow from 198.51.100.9 to any port 2222\n"), nil, testAgentPort)
	refusal := enableLockout(link, st, "")
	if refusal == nil || !strings.Contains(refusal.Error(), "203.0.113.7") {
		t.Fatalf("refusal does not say where fleet connects from: %v", refusal)
	}
}

// firewalld: the zone that claims the controller's address decides, so a
// trusted zone for the admin network admits it even when the default zone
// does not list the port.
func TestFirewalldZoneForTheController(t *testing.T) {
	link := AgentLink{Server: "web-01", Port: testAgentPort}
	public := "public (active)\n  target: default\n  interfaces: eth0\n  ports: 443/tcp\n  rich rules: \n"
	trusted := "trusted (active)\n  target: ACCEPT\n  sources: 203.0.113.0/24\n  ports: \n  rich rules: \n"
	dump := guardMarker + "firewalld-running\n" + public + guardMarker + "permanent\n" +
		"public\n  target: default\n  ports: 2222/tcp\n" + guardMarker + "zones\n" + public + trusted +
		guardMarker + "unit enabled\n" + guardMarker + "peers\n" + ssFromController + guardMarker + "addrs\n" + ipAddrs
	st := parseGuardState(FirewallBackendFirewalld, dump, nil, testAgentPort)
	if refusal := enableLockout(link, st, ""); refusal != nil {
		t.Fatalf("the controller's own trusted zone accepts it, refused: %v", refusal)
	}
}
