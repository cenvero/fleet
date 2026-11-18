// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"regexp"
	"strconv"
	"strings"
)

// The agent-lockout check has to know, from a firewall's own listing, whether
// new inbound TCP connections to the agent's port will still be accepted.
// It used to look for the port number in each line, which misread rules both
// ways. A deny on a range (2000:3000) or on an address with no port was
// invisible, and an allow that only admits another host, only outbound
// traffic or only one interface counted as "the agent port is allowed". So
// each backend's rules are now parsed, in the order the firewall evaluates
// them, into one of these effects on that traffic. Anything fleet cannot read
// counts as possibly blocking: a wrong "allowed" locks fleet out of the
// server, while a wrong refusal costs one `--force-i-have-console`.
type ruleEffect int

const (
	// effNeutral: the rule does not decide new inbound TCP to the agent
	// port (another port, UDP, ICMP, loopback, established traffic, logging).
	effNeutral ruleEffect = iota
	// effAllows: it accepts that traffic from any source.
	effAllows
	// effAllowsRestricted: it accepts it only from some sources, interfaces
	// or addresses, so fleet cannot tell whether the controller is among them.
	effAllowsRestricted
	// effBlocks: it denies, rejects, drops or rate-limits traffic that
	// includes it (on that port, on a range or list covering it, or on every
	// port from some address or interface).
	effBlocks
	// effUnknown: fleet cannot tell what the rule does to it.
	effUnknown
)

// agentPortDecision is what a ruleset means for the agent port.
type agentPortDecision struct {
	allowed bool
	// blockedBy is the rule that may cut the agent off (or "" when none).
	blockedBy string
	// restricted is an allow that admits the port only from some sources,
	// kept to explain a refusal.
	restricted string
}

// decideAgentPort walks rules in evaluation order. With firstMatch (ufw,
// iptables, nftables) the first rule that decides the traffic wins, so a
// block after an unrestricted allow is shadowed; without it (firewalld, which
// evaluates deny rules ahead of allowed ports) any block refuses.
func decideAgentPort(rules []string, port int, firstMatch bool, classify func(string, int) ruleEffect) agentPortDecision {
	var d agentPortDecision
	if port < 1 {
		return d
	}
	for _, raw := range rules {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch classify(line, port) {
		case effAllows:
			d.allowed = true
			if firstMatch {
				return d
			}
		case effAllowsRestricted:
			if d.restricted == "" {
				d.restricted = line
			}
		case effBlocks, effUnknown:
			if d.blockedBy == "" {
				d.blockedBy = line
			}
			if firstMatch {
				d.allowed = false
				return d
			}
		}
	}
	if d.blockedBy != "" {
		d.allowed = false
	}
	return d
}

// portSpecCovers reports whether a port spec in any backend's syntax covers
// port: "2222", "22,2222", "2000:3000" (ufw, iptables), "2000-3000" (nft,
// firewalld), "{ 22, 2222 }" (nft sets), each with an optional "/proto"
// (handled by the caller). Malformed parts never match.
func portListCovers(spec string, port int) bool {
	spec = strings.Trim(spec, "{} \"")
	for _, part := range strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == ' ' || r == '{' || r == '}' }) {
		part, _, _ = strings.Cut(part, "/")
		lo, hi, isRange := strings.Cut(part, ":")
		if !isRange {
			lo, hi, isRange = strings.Cut(part, "-")
		}
		a, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				continue
			}
		}
		if a <= port && port <= b {
			return true
		}
	}
	return false
}

// anyAddress reports whether an address argument means "everywhere".
func anyAddress(a string) bool {
	a = strings.Trim(strings.ToLower(a), `"'`)
	return a == "any" || a == "anywhere" || a == "0.0.0.0/0" || a == "::/0" || a == ""
}

// blockOrRestricted is what an allow or a block means when it applies only
// to some traffic (a source, an interface, a destination address).
func blockOrRestricted(allow bool) ruleEffect {
	if allow {
		return effAllowsRestricted
	}
	return effBlocks
}

// --- ufw: the rules `ufw show added` lists, in order ---

var (
	ufwComment  = regexp.MustCompile(`\s+comment\s+('[^']*'|"[^"]*")`)
	ufwPortList = regexp.MustCompile(`^[0-9][0-9,:]*$`)
)

// classifyUFWRule reads one `ufw show added` line ("ufw allow 2222/tcp",
// "ufw deny from 10.0.0.0/8", "ufw allow in on eth0 to any port 2222
// proto tcp", ...).
func classifyUFWRule(line string, port int) ruleEffect {
	low := ufwComment.ReplaceAllString(strings.ToLower(strings.TrimSpace(line)), "")
	f := strings.Fields(low)
	if len(f) == 0 || f[0] != "ufw" {
		// The header ("Added user rules ...") and "(None)".
		if strings.HasPrefix(low, "added user rules") || low == "(none)" {
			return effNeutral
		}
		return effUnknown
	}
	f = f[1:]
	if len(f) > 0 && f[0] == "route" {
		return effNeutral // forwarded traffic, not traffic to this host
	}
	if len(f) == 0 {
		return effUnknown
	}
	var allow bool
	switch f[0] {
	case "allow":
		allow = true
	case "deny", "reject", "limit":
	default:
		return effUnknown
	}
	f = f[1:]
	restricted := false
	if len(f) > 0 && (f[0] == "in" || f[0] == "out") {
		if f[0] == "out" {
			return effNeutral
		}
		f = f[1:]
	}
	if len(f) > 1 && f[0] == "on" {
		restricted = true // one interface
		f = f[2:]
	}
	for len(f) > 0 && (f[0] == "log" || f[0] == "log-all") {
		f = f[1:]
	}
	if len(f) == 0 {
		return effUnknown
	}
	// Simple form: a port spec or an application/service name.
	if f[0] != "proto" && f[0] != "from" && f[0] != "to" {
		if len(f) != 1 {
			return effUnknown
		}
		spec, proto, _ := strings.Cut(f[0], "/")
		if proto == "udp" {
			return effNeutral
		}
		if !ufwPortList.MatchString(spec) {
			// "ssh", "OpenSSH": fleet does not resolve service names, so
			// it cannot confirm (or rule out) the agent port.
			if allow {
				return effNeutral
			}
			return effUnknown
		}
		if !portListCovers(spec, port) {
			return effNeutral
		}
		if restricted {
			return blockOrRestricted(allow)
		}
		if allow {
			return effAllows
		}
		return effBlocks
	}
	// Full form: [proto P] [from A [port P|app N]] [to A [port P|app N]].
	uncertain := effUnknown // what a part fleet cannot read means
	if allow {
		uncertain = effNeutral // an allow fleet cannot read confirms nothing
	}
	proto, from, to, fromPort, toPort := "", "any", "any", "", ""
	for i := 0; i < len(f); {
		word := f[i]
		if i+1 >= len(f) {
			return uncertain
		}
		switch word {
		case "proto":
			proto = f[i+1]
			i += 2
		case "from", "to":
			addr, p := f[i+1], ""
			i += 2
			if i+1 < len(f) && f[i] == "port" {
				p = f[i+1]
				i += 2
			} else if i+1 < len(f) && f[i] == "app" {
				return uncertain // an application profile: ports fleet does not resolve
			}
			if word == "from" {
				from, fromPort = addr, p
			} else {
				to, toPort = addr, p
			}
		default:
			return uncertain
		}
	}
	if proto != "" && proto != "tcp" && proto != "any" {
		return effNeutral // udp, icmp, …
	}
	if fromPort != "" {
		// A source-port match: the controller's source port is random.
		return uncertain
	}
	if toPort != "" {
		if !ufwPortList.MatchString(toPort) {
			return uncertain
		}
		if !portListCovers(toPort, port) {
			return effNeutral
		}
	}
	if !anyAddress(from) || !anyAddress(to) {
		restricted = true
	}
	if restricted {
		return blockOrRestricted(allow)
	}
	if allow {
		return effAllows // the agent port, or every port, from anywhere
	}
	return effBlocks
}

// --- iptables: `iptables -S` lines of the INPUT chain, in order ---

// classifyIptablesRule reads one "-A INPUT ..." line.
func classifyIptablesRule(line string, port int) ruleEffect {
	f := splitQuoted(line)
	if len(f) < 2 || f[0] != "-A" {
		return effNeutral
	}
	target, proto := "", ""
	var dports string
	restricted, negated, stateOK := false, false, true
	hasPortMatch := false
	for i := 2; i < len(f); i++ {
		a := f[i]
		next := func() string {
			if i+1 < len(f) {
				i++
				return f[i]
			}
			return ""
		}
		switch a {
		case "!":
			negated = true
		case "-p", "--protocol":
			proto = strings.ToLower(next())
		case "-s", "--source", "-d", "--destination":
			if !anyAddress(next()) {
				restricted = true
			}
		case "-i", "--in-interface":
			if v := next(); v == "lo" {
				return effNeutral
			} else if v != "" && v != "+" {
				restricted = true
			}
		case "-o", "--out-interface":
			next()
		case "-m", "--match":
			switch next() {
			case "tcp", "udp", "multiport", "conntrack", "state", "comment":
			case "limit", "hashlimit", "recent", "set", "connlimit", "string", "owner", "geoip", "u32", "bpf":
				restricted = true
			}
		case "--dport", "--destination-port", "--dports", "--destination-ports":
			dports = next()
			hasPortMatch = true
		case "--sport", "--source-port", "--sports", "--source-ports":
			next()
			restricted = true
		case "--ctstate", "--state":
			states := strings.ToUpper(next())
			if !strings.Contains(states, "NEW") && !strings.Contains(states, "UNTRACKED") {
				stateOK = false // established/related/invalid only
			}
		case "--comment":
			next()
		case "--syn", "--tcp-flags":
			if a == "--tcp-flags" {
				next()
				next()
			}
		case "-j", "--jump", "-g", "--goto":
			target = next()
		case "--match-set", "--limit", "--limit-burst", "--name", "--seconds", "--hitcount", "--icmp-type", "--reject-with", "--log-prefix", "--log-level":
			next()
		default:
			if strings.HasPrefix(a, "--") {
				restricted = true // a match fleet does not know
			}
		}
	}
	if !stateOK {
		return effNeutral
	}
	switch proto {
	case "", "tcp", "all", "6":
	default:
		return effNeutral // udp, icmp, …
	}
	if hasPortMatch && !portListCovers(dports, port) {
		if negated {
			if strings.ToUpper(target) == "ACCEPT" {
				return effNeutral
			}
			return effUnknown
		}
		return effNeutral
	}
	var allow bool
	switch strings.ToUpper(target) {
	case "ACCEPT":
		allow = true
	case "DROP", "REJECT", "TARPIT":
	case "LOG", "NFLOG", "ULOG", "RETURN", "":
		return effNeutral
	default:
		return effUnknown // a jump to another chain (fail2ban, ufw-*, …)
	}
	if negated || restricted {
		return blockOrRestricted(allow)
	}
	if allow {
		return effAllows
	}
	return effBlocks
}

// splitQuoted splits a line into words, keeping "quoted strings" together.
func splitQuoted(line string) []string {
	var out []string
	var b strings.Builder
	inQuote := false
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
		case (r == ' ' || r == '\t') && !inQuote:
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// --- nftables: the lines of fleet's own input chain, in order ---

var nftSet = regexp.MustCompile(`\{[^}]*\}`)

// classifyNftRule reads one rule line of an nft chain.
func classifyNftRule(line string, port int) ruleEffect {
	low := strings.ToLower(strings.TrimSpace(line))
	if low == "" || strings.HasPrefix(low, "type ") || strings.HasPrefix(low, "#") || strings.HasPrefix(low, "policy ") {
		return effNeutral
	}
	if i := strings.Index(low, " comment "); i >= 0 {
		low = low[:i]
	}
	// Keep { a, b } sets as one word.
	low = nftSet.ReplaceAllStringFunc(low, func(s string) string { return strings.ReplaceAll(s, " ", "") })
	f := strings.Fields(low)
	verdict := ""
	for _, w := range f {
		switch w {
		case "accept", "drop", "reject", "jump", "goto", "queue":
			verdict = w
		}
	}
	if verdict == "" {
		return effNeutral // counter, log, …
	}
	restricted, hasPort, covers, tcp, unsure := false, false, false, true, false
	for i := 0; i < len(f); i++ {
		w := f[i]
		arg := ""
		if i+1 < len(f) {
			arg = f[i+1]
		}
		switch {
		case w == "ct" && arg == "state":
			if i+2 < len(f) && !strings.Contains(f[i+2], "new") && !strings.Contains(f[i+2], "untracked") {
				return effNeutral // established/related/invalid
			}
			i += 2
		case w == "iif" || w == "iifname":
			if strings.Trim(arg, `"`) == "lo" {
				return effNeutral
			}
			restricted = true
			i++
		case (w == "udp" || w == "sctp" || w == "dccp") && (arg == "dport" || arg == "sport"):
			tcp = false
		case w == "icmp" || w == "icmpv6" || w == "ipv6-icmp":
			tcp = false
		case (w == "l4proto" || w == "protocol" || w == "nexthdr") && arg != "":
			if !strings.Contains(arg, "tcp") {
				tcp = false
			}
			i++
		case w == "dport" && arg != "":
			hasPort = true
			if strings.HasPrefix(arg, "@") || arg == "!=" {
				unsure = true // a named set, or a negated match
			} else {
				covers = portListCovers(arg, port)
			}
			i++
		case w == "sport":
			restricted = true
			i++
		case w == "saddr" || w == "daddr":
			if !anyAddress(arg) {
				restricted = true
			}
			i++
		case w == "limit" || w == "meter" || strings.HasPrefix(w, "@"):
			restricted = true
		case w == "!=":
			unsure = true
		}
	}
	if !tcp {
		return effNeutral
	}
	switch verdict {
	case "accept":
		switch {
		case unsure:
			return effNeutral // confirms nothing
		case hasPort && !covers:
			return effNeutral
		case restricted:
			return effAllowsRestricted
		}
		return effAllows
	case "drop", "reject":
		switch {
		case unsure:
			return effUnknown
		case hasPort && !covers:
			return effNeutral
		}
		return effBlocks
	}
	// jump, goto, queue: another chain decides.
	if hasPort && !covers && !unsure {
		return effNeutral
	}
	return effUnknown
}

// --- firewalld: `firewall-cmd --list-all` of a zone ---

var firewalldRichPort = regexp.MustCompile(`port="([^"]+)"`)

// firewalldDecision reads a zone listing. Allowed means its ports (or an
// accepting target) admit the agent port; any rich rule that may block it,
// from anywhere, refuses, since firewalld applies deny rules ahead of the
// zone's allowed ports.
func firewalldDecision(zone string, port int) agentPortDecision {
	var lines []string
	inRich := false
	for _, raw := range strings.Split(zone, "\n") {
		line := strings.TrimSpace(raw)
		low := strings.ToLower(line)
		switch {
		case strings.HasPrefix(low, "rich rules:"):
			inRich = true
			continue
		case strings.HasPrefix(low, "rule "):
			lines = append(lines, "rich: "+line)
			continue
		case strings.Contains(low, ":") && !strings.HasPrefix(low, "rule"):
			inRich = false
		}
		if !inRich {
			lines = append(lines, line)
		}
	}
	return decideAgentPort(lines, port, false, classifyFirewalldLine)
}

func classifyFirewalldLine(line string, port int) ruleEffect {
	low := strings.ToLower(line)
	if rule, ok := strings.CutPrefix(low, "rich: "); ok {
		return classifyFirewalldRich(rule, port)
	}
	key, value, ok := strings.Cut(low, ":")
	if !ok {
		return effNeutral
	}
	switch strings.TrimSpace(key) {
	case "ports":
		for _, spec := range strings.Fields(value) {
			list, proto, _ := strings.Cut(spec, "/")
			if proto != "udp" && proto != "sctp" && proto != "dccp" && portListCovers(list, port) {
				return effAllows
			}
		}
	case "protocols":
		for _, p := range strings.Fields(value) {
			if p == "tcp" {
				return effAllows
			}
		}
	case "target":
		if strings.TrimSpace(value) == "accept" {
			return effAllows
		}
	}
	return effNeutral
}

func classifyFirewalldRich(rule string, port int) ruleEffect {
	accept := strings.Contains(rule, " accept")
	block := strings.Contains(rule, " reject") || strings.Contains(rule, " drop")
	if !accept && !block {
		return effNeutral // log, audit, mark, forward-port
	}
	if strings.Contains(rule, "forward-port") || strings.Contains(rule, "source-port") {
		return effNeutral
	}
	if strings.Contains(rule, `protocol="udp"`) && !strings.Contains(rule, `protocol="tcp"`) {
		return effNeutral
	}
	if strings.Contains(rule, "service name=") {
		if accept {
			return effNeutral
		}
		return effUnknown
	}
	restricted := strings.Contains(rule, "source ") || strings.Contains(rule, "destination ") ||
		strings.Contains(rule, " not ") || strings.Contains(rule, "limit value")
	if m := firewalldRichPort.FindStringSubmatch(rule); m != nil {
		if !portListCovers(m[1], port) {
			return effNeutral
		}
	} else if accept && !restricted {
		return effAllows // accepts everything
	} else if accept {
		return effNeutral // port-less restricted allow: decides other traffic
	}
	if accept {
		if restricted {
			return effAllowsRestricted
		}
		return effAllows
	}
	return effBlocks
}
