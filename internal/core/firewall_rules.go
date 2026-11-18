// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// The agent-lockout check has to know, from a firewall's own listing, whether
// new inbound TCP connections from the controller to the agent's port will
// still be accepted. Each backend's rules are parsed, in the order the
// firewall evaluates them, into a ruleMatch: what the rule does to new TCP
// traffic on the agent port, and to which part of it (some sources, some
// destination addresses, some interfaces).
//
// Restricted rules are resolved against the controller's own connections.
// While the check runs, fleet is connected to the agent port, so the server
// lists those connections (`ss`, or `netstat`) with the address the
// controller appears to come from (after any NAT) and the local address it
// reached, whose interface `ip addr` gives. An allow only from your
// controller's IP or network, only on your VPN interface, or only to one
// address of the server is then confirmed when every such connection
// matches it. A block that matches none of them does not count.
//
// Anything fleet cannot read or resolve counts against the change: a wrong
// "allowed" locks fleet out of the server, while a wrong refusal costs one
// `--force-i-have-console`.
type ruleEffect int

const (
	// effNeutral: the rule does not decide the controller's new TCP
	// connections to the agent port.
	effNeutral ruleEffect = iota
	// effAllows: it accepts them.
	effAllows
	// effAllowsRestricted: it accepts some traffic to the port, but fleet
	// cannot confirm the controller's is among it.
	effAllowsRestricted
	// effBlocks: it denies, rejects, drops or rate-limits traffic that
	// includes them.
	effBlocks
	// effUnknown: fleet cannot tell what it does to them.
	effUnknown
)

// ruleVerdict is what a rule does to new inbound TCP on the agent port,
// before its source, destination and interface restrictions are applied.
type ruleVerdict int

const (
	vNone    ruleVerdict = iota // does not concern that traffic
	vAccept                     // accepts it
	vBlock                      // denies, rejects, drops or limits it
	vUnknown                    // another chain decides, or fleet cannot read it
)

// ruleMatch is one parsed rule.
type ruleMatch struct {
	verdict ruleVerdict
	sources []netip.Prefix // nil: from anywhere
	dests   []netip.Prefix // nil: to any of the server's addresses
	ifaces  []string       // nil: on any interface
	// opaque marks a restriction fleet cannot evaluate (a rate limit, a
	// source port, a named set, a negation).
	opaque bool
}

func (m ruleMatch) restricted() bool {
	return m.opaque || len(m.sources) > 0 || len(m.dests) > 0 || len(m.ifaces) > 0
}

// agentConn is one established connection to the agent port, as the server
// sees it.
type agentConn struct {
	local, remote netip.Addr
	iface         string // the interface owning local, or ""
}

// agentPeers are the connections to the agent port while the check ran. The
// controller's own is one of them.
type agentPeers struct {
	known bool
	conns []agentConn
}

// matchCount tells how many of the connections a restricted rule matches.
type matchCount int

const (
	matchNone matchCount = iota
	matchSome            // some, or fleet cannot tell for some
	matchAll
)

func (m ruleMatch) matchPeers(peers agentPeers) matchCount {
	yes, no := 0, 0
	for _, c := range peers.conns {
		switch m.matchConn(c) {
		case triYes:
			yes++
		case triNo:
			no++
		default:
			return matchSome
		}
	}
	switch {
	case yes > 0 && no == 0:
		return matchAll
	case yes == 0:
		return matchNone
	}
	return matchSome
}

func (m ruleMatch) matchConn(c agentConn) triState {
	if len(m.sources) > 0 && !prefixesContain(m.sources, c.remote) {
		return triNo
	}
	if len(m.dests) > 0 && !prefixesContain(m.dests, c.local) {
		return triNo
	}
	if len(m.ifaces) > 0 {
		if c.iface == "" {
			return triUnknown
		}
		found := false
		for _, i := range m.ifaces {
			if i == c.iface {
				found = true
			}
		}
		if !found {
			return triNo
		}
	}
	return triYes
}

func prefixesContain(ps []netip.Prefix, a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// effect resolves m against the controller's connections.
func (m ruleMatch) effect(peers agentPeers) ruleEffect {
	switch m.verdict {
	case vNone:
		return effNeutral
	case vUnknown:
		return effUnknown
	}
	accept := m.verdict == vAccept
	if !m.restricted() {
		if accept {
			return effAllows
		}
		return effBlocks
	}
	if m.opaque || !peers.known || len(peers.conns) == 0 {
		if accept {
			return effAllowsRestricted
		}
		return effBlocks
	}
	switch m.matchPeers(peers) {
	case matchAll:
		if accept {
			return effAllows
		}
		return effBlocks
	case matchNone:
		return effNeutral // it decides other traffic, not the controller's
	}
	if accept {
		return effAllowsRestricted
	}
	return effBlocks
}

// agentPortDecision is what a ruleset means for the agent port.
type agentPortDecision struct {
	allowed bool
	// blockedBy is the rule that may cut the agent off (or "" when none).
	blockedBy string
	// restricted is an allow that admits the port only to some traffic that
	// fleet could not confirm includes the controller's, kept to explain a
	// refusal.
	restricted string
}

// decideAgentPort walks rules in evaluation order. With firstMatch (ufw,
// iptables, nftables) the first rule that decides the controller's traffic
// wins, so a block after an allow that admits it is shadowed; without it
// (firewalld, which evaluates deny rules ahead of allowed ports) any block
// refuses.
func decideAgentPort(rules []string, port int, firstMatch bool, classify func(string, int) ruleMatch, peers agentPeers) agentPortDecision {
	var d agentPortDecision
	if port < 1 {
		return d
	}
	for _, raw := range rules {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		m := classify(line, port)
		eff := m.effect(peers)
		if m.verdict == vAccept && m.restricted() && eff != effAllows && d.restricted == "" {
			d.restricted = line // an allow for some traffic only, not (known to be) the controller's
		}
		switch eff {
		case effAllows:
			d.allowed = true
			if firstMatch {
				return d
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

// portListCovers reports whether a port spec in any backend's syntax covers
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

// parseAddrList reads addresses or networks ("10.0.0.5", "10.0.0.0/8",
// "2001:db8::/32", "{ 10.0.0.5, 10.1.0.0/16 }", "a,b"). It returns nil for
// "any", and ok=false when any part is not an address fleet can read (a host
// name, a named set).
func parseAddrList(s string) ([]netip.Prefix, bool) {
	s = strings.Trim(strings.TrimSpace(s), `"'{}`)
	if anyAddress(s) {
		return nil, true
	}
	var out []netip.Prefix
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		part = strings.Trim(part, `"'`)
		if anyAddress(part) {
			return nil, true
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(part)
		if err != nil {
			return nil, false
		}
		a = a.Unmap().WithZone("")
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// uncertain is what a rule fleet cannot fully read means: an allow
// confirms nothing; a block may cut the controller off.
func uncertain(allow bool) ruleMatch {
	if allow {
		return ruleMatch{verdict: vNone}
	}
	return ruleMatch{verdict: vUnknown}
}

func verdictOf(allow bool) ruleVerdict {
	if allow {
		return vAccept
	}
	return vBlock
}

// --- ufw: the rules `ufw show added` lists, in order ---

var (
	ufwComment  = regexp.MustCompile(`\s+comment\s+('[^']*'|"[^"]*")`)
	ufwPortList = regexp.MustCompile(`^[0-9][0-9,:]*$`)
)

// classifyUFWRule reads one `ufw show added` line ("ufw allow 2222/tcp",
// "ufw allow from 203.0.113.7 to any port 2222 proto tcp", "ufw allow in on
// tailscale0 to any port 2222", "ufw deny from 10.0.0.0/8", ...).
func classifyUFWRule(line string, port int) ruleMatch {
	low := ufwComment.ReplaceAllString(strings.ToLower(strings.TrimSpace(line)), "")
	f := strings.Fields(low)
	if len(f) == 0 || f[0] != "ufw" {
		// The header ("Added user rules ...") and "(None)".
		if strings.HasPrefix(low, "added user rules") || low == "(none)" {
			return ruleMatch{verdict: vNone}
		}
		return ruleMatch{verdict: vUnknown}
	}
	f = f[1:]
	if len(f) > 0 && f[0] == "route" {
		return ruleMatch{verdict: vNone} // forwarded traffic, not traffic to this host
	}
	if len(f) == 0 {
		return ruleMatch{verdict: vUnknown}
	}
	var allow bool
	switch f[0] {
	case "allow":
		allow = true
	case "deny", "reject", "limit":
	default:
		return ruleMatch{verdict: vUnknown}
	}
	f = f[1:]
	m := ruleMatch{verdict: verdictOf(allow)}
	if len(f) > 0 && (f[0] == "in" || f[0] == "out") {
		if f[0] == "out" {
			return ruleMatch{verdict: vNone}
		}
		f = f[1:]
	}
	if len(f) > 1 && f[0] == "on" {
		m.ifaces = []string{f[1]}
		f = f[2:]
	}
	for len(f) > 0 && (f[0] == "log" || f[0] == "log-all") {
		f = f[1:]
	}
	if len(f) == 0 {
		return ruleMatch{verdict: vUnknown}
	}
	// Simple form: a port spec or an application/service name.
	if f[0] != "proto" && f[0] != "from" && f[0] != "to" {
		if len(f) != 1 {
			return uncertain(allow)
		}
		spec, proto, _ := strings.Cut(f[0], "/")
		if proto == "udp" {
			return ruleMatch{verdict: vNone}
		}
		if !ufwPortList.MatchString(spec) {
			// "ssh", "OpenSSH": fleet does not resolve service names.
			return uncertain(allow)
		}
		if !portListCovers(spec, port) {
			return ruleMatch{verdict: vNone}
		}
		return m
	}
	// Full form: [proto P] [from A [port P|app N]] [to A [port P|app N]].
	proto, fromPort, toPort := "", "", ""
	for i := 0; i < len(f); {
		word := f[i]
		if i+1 >= len(f) {
			return uncertain(allow)
		}
		switch word {
		case "proto":
			proto = f[i+1]
			i += 2
		case "from", "to":
			addrs, ok := parseAddrList(f[i+1])
			if !ok {
				return uncertain(allow)
			}
			p := ""
			i += 2
			if i+1 < len(f) && f[i] == "port" {
				p = f[i+1]
				i += 2
			} else if i+1 < len(f) && f[i] == "app" {
				return uncertain(allow) // an application profile: ports fleet does not resolve
			}
			if word == "from" {
				m.sources, fromPort = addrs, p
			} else {
				m.dests, toPort = addrs, p
			}
		default:
			return uncertain(allow)
		}
	}
	if proto != "" && proto != "tcp" && proto != "any" {
		return ruleMatch{verdict: vNone} // udp, icmp, …
	}
	if fromPort != "" {
		m.opaque = true // a source-port match: the controller's source port is random
	}
	if toPort != "" {
		if !ufwPortList.MatchString(toPort) {
			return uncertain(allow)
		}
		if !portListCovers(toPort, port) {
			return ruleMatch{verdict: vNone}
		}
	}
	return m
}

// --- iptables: `iptables -S` lines of the INPUT chain, in order ---

// classifyIptablesRule reads one "-A INPUT ..." line.
func classifyIptablesRule(line string, port int) ruleMatch {
	f := splitQuoted(line)
	if len(f) < 2 || f[0] != "-A" {
		return ruleMatch{verdict: vNone}
	}
	var m ruleMatch
	target, proto := "", ""
	var dports string
	stateOK, hasPortMatch, dportNegated := true, false, false
	for i := 2; i < len(f); i++ {
		a := f[i]
		next := func() string {
			if i+1 < len(f) {
				i++
				return f[i]
			}
			return ""
		}
		negated := false
		if a == "!" {
			negated = true
			if i+1 >= len(f) {
				break
			}
			i++
			a = f[i]
		}
		switch a {
		case "-p", "--protocol":
			proto = strings.ToLower(next())
			if negated {
				m.opaque = true
			}
		case "-s", "--source", "-d", "--destination":
			addrs, ok := parseAddrList(next())
			switch {
			case negated || !ok:
				m.opaque = true
			case a == "-s" || a == "--source":
				m.sources = addrs
			default:
				m.dests = addrs
			}
		case "-i", "--in-interface":
			v := next()
			switch {
			case v == "lo" && !negated:
				return ruleMatch{verdict: vNone}
			case negated || strings.HasSuffix(v, "+"):
				m.opaque = true
			case v != "":
				m.ifaces = []string{v}
			}
		case "-o", "--out-interface":
			next()
		case "-m", "--match":
			switch next() {
			case "tcp", "udp", "multiport", "conntrack", "state", "comment":
			case "limit", "hashlimit", "recent", "set", "connlimit", "string", "owner", "geoip", "u32", "bpf":
				m.opaque = true
			}
		case "--dport", "--destination-port", "--dports", "--destination-ports":
			dports = next()
			hasPortMatch = true
			dportNegated = negated
		case "--sport", "--source-port", "--sports", "--source-ports":
			next()
			m.opaque = true
		case "--ctstate", "--state":
			states := strings.ToUpper(next())
			if !negated && !strings.Contains(states, "NEW") && !strings.Contains(states, "UNTRACKED") {
				stateOK = false // established/related/invalid only
			}
		case "--comment":
			next()
		case "--syn":
		case "--tcp-flags":
			next()
			next()
		case "-j", "--jump", "-g", "--goto":
			target = next()
		case "--match-set", "--limit", "--limit-burst", "--name", "--seconds", "--hitcount", "--icmp-type", "--reject-with", "--log-prefix", "--log-level":
			next()
		default:
			if strings.HasPrefix(a, "-") {
				m.opaque = true // a match fleet does not know
			}
		}
	}
	if !stateOK {
		return ruleMatch{verdict: vNone}
	}
	switch proto {
	case "", "tcp", "all", "6":
	default:
		return ruleMatch{verdict: vNone} // udp, icmp, …
	}
	allow := strings.ToUpper(target) == "ACCEPT"
	if hasPortMatch {
		covers := portListCovers(dports, port)
		if dportNegated {
			if covers {
				return ruleMatch{verdict: vNone} // every port but the agent's
			}
			return uncertain(allow)
		}
		if !covers {
			return ruleMatch{verdict: vNone}
		}
	}
	switch strings.ToUpper(target) {
	case "ACCEPT":
		m.verdict = vAccept
	case "DROP", "REJECT", "TARPIT":
		m.verdict = vBlock
	case "LOG", "NFLOG", "ULOG", "RETURN", "":
		return ruleMatch{verdict: vNone}
	default:
		return ruleMatch{verdict: vUnknown} // a jump to another chain (fail2ban, ufw-*, …)
	}
	return m
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
func classifyNftRule(line string, port int) ruleMatch {
	low := strings.ToLower(strings.TrimSpace(line))
	if low == "" || strings.HasPrefix(low, "type ") || strings.HasPrefix(low, "#") || strings.HasPrefix(low, "policy ") {
		return ruleMatch{verdict: vNone}
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
		return ruleMatch{verdict: vNone} // counter, log, …
	}
	var m ruleMatch
	hasPort, covers, unsure := false, false, false
	for i := 0; i < len(f); i++ {
		w := f[i]
		arg := ""
		if i+1 < len(f) {
			arg = f[i+1]
		}
		switch {
		case w == "ct" && arg == "state":
			if i+2 < len(f) && !strings.Contains(f[i+2], "new") && !strings.Contains(f[i+2], "untracked") {
				return ruleMatch{verdict: vNone} // established/related/invalid
			}
			i += 2
		case w == "iif" || w == "iifname":
			v := strings.Trim(arg, `"`)
			switch {
			case v == "lo":
				return ruleMatch{verdict: vNone}
			case v == "!=" || strings.HasPrefix(v, "{") || strings.HasPrefix(v, "@") || strings.Contains(v, "*"):
				m.opaque = true
			default:
				m.ifaces = []string{v}
			}
			i++
		case (w == "udp" || w == "sctp" || w == "dccp") && (arg == "dport" || arg == "sport"):
			return ruleMatch{verdict: vNone}
		case w == "icmp" || w == "icmpv6" || w == "ipv6-icmp":
			return ruleMatch{verdict: vNone}
		case (w == "l4proto" || w == "protocol" || w == "nexthdr") && arg != "":
			if !strings.Contains(arg, "tcp") {
				return ruleMatch{verdict: vNone}
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
			m.opaque = true
			i++
		case w == "saddr" || w == "daddr":
			if arg == "!=" || strings.HasPrefix(arg, "@") {
				m.opaque = true
			} else if addrs, ok := parseAddrList(arg); !ok {
				m.opaque = true
			} else if w == "saddr" {
				m.sources = addrs
			} else {
				m.dests = addrs
			}
			i++
		case w == "limit" || w == "meter" || strings.HasPrefix(w, "@"):
			m.opaque = true
		case w == "!=":
			unsure = true
		}
	}
	allow := verdict == "accept"
	switch verdict {
	case "accept", "drop", "reject":
		if unsure {
			return uncertain(allow)
		}
		if hasPort && !covers {
			return ruleMatch{verdict: vNone}
		}
		m.verdict = verdictOf(allow)
		return m
	}
	// jump, goto, queue: another chain decides.
	if hasPort && !covers && !unsure {
		return ruleMatch{verdict: vNone}
	}
	return ruleMatch{verdict: vUnknown}
}

// --- firewalld: `firewall-cmd --list-all` of a zone ---

var firewalldRichPort = regexp.MustCompile(`port="([^"]+)"`)
var firewalldRichSource = regexp.MustCompile(`source address="([^"]+)"`)
var firewalldRichDest = regexp.MustCompile(`destination address="([^"]+)"`)

// firewalldZoneInfo is one zone listing.
type firewalldZoneInfo struct {
	name       string
	sources    []netip.Prefix // the zone's "sources:" (traffic from them is this zone's)
	interfaces []string
	listing    string
}

// parseFirewalldZone reads a zone listing's header and bindings.
func parseFirewalldZone(listing string) firewalldZoneInfo {
	z := firewalldZoneInfo{listing: listing}
	for i, raw := range strings.Split(listing, "\n") {
		line := strings.TrimSpace(raw)
		if i == 0 {
			z.name, _, _ = strings.Cut(line, " ")
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "sources":
			if addrs, ok := parseAddrList(value); ok {
				z.sources = addrs
			}
		case "interfaces":
			z.interfaces = strings.Fields(value)
		}
	}
	return z
}

// firewalldDecision reads a zone listing. Allowed means its ports (or an
// accepting target) admit the agent port; any rich rule that may block the
// controller refuses, since firewalld applies deny rules ahead of the zone's
// allowed ports.
func firewalldDecision(zone string, port int, peers agentPeers) agentPortDecision {
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
	return decideAgentPort(lines, port, false, classifyFirewalldLine, peers)
}

func classifyFirewalldLine(line string, port int) ruleMatch {
	low := strings.ToLower(line)
	if rule, ok := strings.CutPrefix(low, "rich: "); ok {
		return classifyFirewalldRich(rule, port)
	}
	key, value, ok := strings.Cut(low, ":")
	if !ok {
		return ruleMatch{verdict: vNone}
	}
	switch strings.TrimSpace(key) {
	case "ports":
		for _, spec := range strings.Fields(value) {
			list, proto, _ := strings.Cut(spec, "/")
			if proto != "udp" && proto != "sctp" && proto != "dccp" && portListCovers(list, port) {
				return ruleMatch{verdict: vAccept}
			}
		}
	case "protocols":
		for _, p := range strings.Fields(value) {
			if p == "tcp" {
				return ruleMatch{verdict: vAccept}
			}
		}
	case "target":
		if strings.TrimSpace(value) == "accept" {
			return ruleMatch{verdict: vAccept}
		}
	}
	return ruleMatch{verdict: vNone}
}

func classifyFirewalldRich(rule string, port int) ruleMatch {
	accept := strings.Contains(rule, " accept")
	block := strings.Contains(rule, " reject") || strings.Contains(rule, " drop")
	if !accept && !block {
		return ruleMatch{verdict: vNone} // log, audit, mark
	}
	if strings.Contains(rule, "forward-port") || strings.Contains(rule, "source-port") {
		return ruleMatch{verdict: vNone}
	}
	if strings.Contains(rule, `protocol="udp"`) && !strings.Contains(rule, `protocol="tcp"`) {
		return ruleMatch{verdict: vNone}
	}
	if strings.Contains(rule, "service name=") {
		return uncertain(accept)
	}
	m := ruleMatch{verdict: verdictOf(accept)}
	if strings.Contains(rule, " not ") || strings.Contains(rule, "limit value") || strings.Contains(rule, "ipset=") {
		m.opaque = true
	}
	if s := firewalldRichSource.FindStringSubmatch(rule); s != nil {
		if addrs, ok := parseAddrList(s[1]); ok {
			m.sources = addrs
		} else {
			m.opaque = true
		}
	}
	if d := firewalldRichDest.FindStringSubmatch(rule); d != nil {
		if addrs, ok := parseAddrList(d[1]); ok {
			m.dests = addrs
		} else {
			m.opaque = true
		}
	}
	if p := firewalldRichPort.FindStringSubmatch(rule); p != nil {
		if !portListCovers(p[1], port) {
			return ruleMatch{verdict: vNone}
		}
	} else if accept && m.restricted() {
		// A port-less restricted allow (all ports from some source): it
		// admits the agent port for that traffic too.
		return m
	}
	return m
}

// firewalldZoneBlocks splits the concatenated `firewall-cmd --zone=Z
// --list-all` listings of the active zones: each starts with an unindented
// "<zone> (active)" line.
func firewalldZoneBlocks(text string) []string {
	var zones []string
	var cur strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			if cur.Len() > 0 {
				zones = append(zones, cur.String())
				cur.Reset()
			}
		}
		if strings.TrimSpace(line) != "" {
			cur.WriteString(line + "\n")
		}
	}
	if cur.Len() > 0 {
		zones = append(zones, cur.String())
	}
	return zones
}

// zonesForPeers picks the active zones that carry the controller's
// connections: a zone whose sources include the connection's address,
// otherwise the zone bound to its interface, otherwise the default zone.
// With no connection information, or one fleet cannot place, every active
// zone counts.
func zonesForPeers(zones []firewalldZoneInfo, defaultZone string, peers agentPeers) []firewalldZoneInfo {
	if !peers.known || len(peers.conns) == 0 {
		return zones
	}
	picked := map[string]bool{}
	for _, c := range peers.conns {
		name := ""
		for _, z := range zones {
			if len(z.sources) > 0 && prefixesContain(z.sources, c.remote) {
				name = z.name
				break
			}
		}
		if name == "" && c.iface != "" {
			for _, z := range zones {
				for _, i := range z.interfaces {
					if i == c.iface {
						name = z.name
					}
				}
			}
		}
		if name == "" {
			if c.iface == "" {
				return zones // cannot place it
			}
			name = defaultZone
		}
		picked[name] = true
	}
	var out []firewalldZoneInfo
	for _, z := range zones {
		if picked[z.name] {
			out = append(out, z)
		}
	}
	if len(out) == 0 {
		return zones
	}
	return out
}

// --- the controller's connections, as the server sees them ---

// agentPeersCommand lists, without changing anything, the established TCP
// connections and the server's addresses, for parseAgentPeers.
const agentPeersCommand = "echo '" + guardMarker + "peers'; (ss -tn state established 2>/dev/null || netstat -tn 2>/dev/null | grep ESTABLISHED); " +
	"echo '" + guardMarker + "addrs'; ip -o addr show 2>/dev/null; true"

// parseAgentPeers reads agentPeersCommand's sections: the connections whose
// local port is the agent's, and the interface each local address is on.
func parseAgentPeers(peersText, addrsText string, port int) agentPeers {
	ifaceOf := map[netip.Addr]string{}
	for _, line := range strings.Split(addrsText, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || (f[2] != "inet" && f[2] != "inet6") {
			continue
		}
		name, _, _ := strings.Cut(f[1], "@")
		addr, _, _ := strings.Cut(f[3], "/")
		if a, err := netip.ParseAddr(addr); err == nil {
			ifaceOf[a.Unmap().WithZone("")] = strings.TrimSuffix(name, ":")
		}
	}
	var peers agentPeers
	for _, line := range strings.Split(peersText, "\n") {
		var addrs []netip.AddrPort
		for _, tok := range strings.Fields(line) {
			if ap, ok := parseAddrPortToken(tok); ok {
				addrs = append(addrs, ap)
			}
		}
		if len(addrs) < 2 {
			continue
		}
		local, remote := addrs[0], addrs[1]
		if int(local.Port()) != port {
			continue
		}
		la := local.Addr().Unmap().WithZone("")
		peers.conns = append(peers.conns, agentConn{local: la, remote: remote.Addr().Unmap().WithZone(""), iface: ifaceOf[la]})
	}
	// Only a listing that shows the connection fleet is checking over counts.
	peers.known = len(peers.conns) > 0
	return peers
}

// parseAddrPortToken reads "10.0.0.5:2222", "[2001:db8::1]:2222",
// "::ffff:10.0.0.5:2222" (netstat) or "10.0.0.5.2222" (BSD netstat).
func parseAddrPortToken(tok string) (netip.AddrPort, bool) {
	if ap, err := netip.ParseAddrPort(tok); err == nil {
		return ap, true
	}
	i := strings.LastIndexAny(tok, ":.")
	if i <= 0 {
		return netip.AddrPort{}, false
	}
	host, portText := strings.Trim(tok[:i], "[]"), tok[i+1:]
	p, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(a, uint16(p)), true
}

// remotes lists the distinct addresses the connections come from.
func (p agentPeers) remotes() string {
	seen := map[netip.Addr]bool{}
	var out []string
	for _, c := range p.conns {
		if !seen[c.remote] {
			seen[c.remote] = true
			out = append(out, c.remote.String())
		}
	}
	return strings.Join(out, ", ")
}
