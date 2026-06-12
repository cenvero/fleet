// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// FL-029 — firewall-safe.
//
// A SAFETY-CRITICAL, agent-safe firewall helper. The cardinal rule everywhere in
// this file: before applying ANY rule that could tighten the policy (enable a
// default-drop firewall, add a deny rule), we ALWAYS first inject an explicit
// allow rule for the agent's OWN management port. Locking the controller out of
// its own agent is unrecoverable without console access, so every code path that
// could close the door first nails it open for the agent port.
//
// All remote commands are built from shell-quoted, validated values. The engine
// logic is pure and exercised by firewallsafe_test.go through a fake exec
// function (FirewallExec); it does not depend on *App.

// FirewallBackend identifies a detected/normalized host firewall engine.
type FirewallBackend string

const (
	FirewallBackendUnknown   FirewallBackend = "unknown"
	FirewallBackendNftables  FirewallBackend = "nftables"
	FirewallBackendIptables  FirewallBackend = "iptables"
	FirewallBackendFirewalld FirewallBackend = "firewalld"
	FirewallBackendUFW       FirewallBackend = "ufw"
)

// FirewallExec runs a remote command and returns combined stdout, the exit code,
// and a transport-level error (nil unless the call itself failed). It mirrors the
// shape of App.ExecCommand so the engine can be driven by a fake in tests.
type FirewallExec func(command string) (stdout string, exitCode int, err error)

// FirewallProtocol is a normalized L4 protocol for an allow/deny rule.
type FirewallProtocol string

const (
	ProtoTCP FirewallProtocol = "tcp"
	ProtoUDP FirewallProtocol = "udp"
)

// PortSpec is a validated port/proto pair (e.g. 443/tcp).
type PortSpec struct {
	Port  int
	Proto FirewallProtocol
}

func (p PortSpec) String() string {
	return fmt.Sprintf("%d/%s", p.Port, p.Proto)
}

// ParsePortSpec parses "<port>" or "<port>/<proto>" into a validated PortSpec.
// A bare port defaults to tcp. Ports must be 1-65535; proto must be tcp or udp.
func ParsePortSpec(spec string) (PortSpec, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return PortSpec{}, fmt.Errorf("empty port spec")
	}
	portPart, protoPart, hasProto := strings.Cut(spec, "/")
	portPart = strings.TrimSpace(portPart)
	port, err := strconv.Atoi(portPart)
	if err != nil {
		return PortSpec{}, fmt.Errorf("invalid port %q: must be a number", portPart)
	}
	if port < 1 || port > 65535 {
		return PortSpec{}, fmt.Errorf("invalid port %d: must be 1-65535", port)
	}
	proto := ProtoTCP
	if hasProto {
		switch strings.ToLower(strings.TrimSpace(protoPart)) {
		case "tcp", "":
			proto = ProtoTCP
		case "udp":
			proto = ProtoUDP
		default:
			return PortSpec{}, fmt.Errorf("invalid protocol %q: must be tcp or udp", protoPart)
		}
	}
	return PortSpec{Port: port, Proto: proto}, nil
}

// FirewallEngine is a pure, testable firewall driver bound to a single server's
// agent port. It never depends on *App: callers pass an exec function.
type FirewallEngine struct {
	// AgentPort is the management port that MUST stay reachable. Every tightening
	// operation injects an allow rule for it first.
	AgentPort int
	// Exec runs a remote command. Required.
	Exec FirewallExec
}

// FirewallStatusReport is the normalized result of a status probe.
type FirewallStatusReport struct {
	Backend     FirewallBackend `json:"backend"`
	Active      bool            `json:"active"`
	DefaultDrop bool            `json:"default_drop"`
	AgentPort   int             `json:"agent_port"`
	// AgentPortAllowed is true when an explicit allow rule for the agent port was
	// observed in the live ruleset.
	AgentPortAllowed bool `json:"agent_port_allowed"`
	// Raw is the (trimmed) backend-native ruleset dump, for the operator's eyes.
	Raw string `json:"raw,omitempty"`
}

// DetectBackend probes the host and returns the first available, normalized
// firewall backend. It prefers higher-level managers (ufw/firewalld) over the
// raw engines so we drive the same tool the operator manages the host with.
func (e *FirewallEngine) DetectBackend() (FirewallBackend, error) {
	if e.Exec == nil {
		return FirewallBackendUnknown, fmt.Errorf("firewall engine: Exec is nil")
	}
	// Probe in priority order. `command -v <tool>` exits 0 when present.
	probes := []struct {
		tool    string
		backend FirewallBackend
	}{
		{"ufw", FirewallBackendUFW},
		{"firewall-cmd", FirewallBackendFirewalld},
		{"nft", FirewallBackendNftables},
		{"iptables", FirewallBackendIptables},
	}
	for _, p := range probes {
		cmd := "command -v " + shellQuote(p.tool) + " >/dev/null 2>&1 && echo yes || echo no"
		out, _, err := e.Exec(cmd)
		if err != nil {
			return FirewallBackendUnknown, err
		}
		if strings.TrimSpace(out) == "yes" {
			return p.backend, nil
		}
	}
	return FirewallBackendUnknown, nil
}

// Status detects the backend and returns a normalized status report.
func (e *FirewallEngine) Status() (FirewallStatusReport, error) {
	backend, err := e.DetectBackend()
	if err != nil {
		return FirewallStatusReport{}, err
	}
	report := FirewallStatusReport{Backend: backend, AgentPort: e.AgentPort}
	if backend == FirewallBackendUnknown {
		return report, nil
	}
	dump, err := e.dumpRuleset(backend)
	if err != nil {
		return report, err
	}
	report.Raw = strings.TrimSpace(dump)
	report.Active = backendActive(backend, dump)
	report.DefaultDrop = backendDefaultDrop(backend, dump)
	report.AgentPortAllowed = rulesetAllowsPort(dump, e.AgentPort)
	return report, nil
}

// dumpRuleset returns the backend-native ruleset text used for normalization.
func (e *FirewallEngine) dumpRuleset(backend FirewallBackend) (string, error) {
	var cmd string
	switch backend {
	case FirewallBackendUFW:
		cmd = "ufw status verbose 2>&1 || true"
	case FirewallBackendFirewalld:
		cmd = "firewall-cmd --list-all 2>&1; firewall-cmd --state 2>&1 || true"
	case FirewallBackendNftables:
		cmd = "nft list ruleset 2>&1 || true"
	case FirewallBackendIptables:
		cmd = "iptables -S 2>&1 || true"
	default:
		return "", nil
	}
	out, _, err := e.Exec(cmd)
	return out, err
}

// AllowPort builds and applies the backend-native command that opens a port for
// the given protocol. Opening a port never reduces reachability, so it does not
// require the agent-port guard — but the command is still fully shell-quoted.
func (e *FirewallEngine) AllowPort(backend FirewallBackend, spec PortSpec) (string, error) {
	cmd, err := allowPortCommand(backend, spec)
	if err != nil {
		return "", err
	}
	return e.run(cmd)
}

// EnableSafeResult reports the outcome of a guarded `enable --safe`.
type EnableSafeResult struct {
	Backend         FirewallBackend `json:"backend"`
	AgentPort       int             `json:"agent_port"`
	AgentAllowCmd   string          `json:"agent_allow_cmd"`
	EnableCmd       string          `json:"enable_cmd"`
	UndoCmd         string          `json:"undo_cmd"`
	UndoScheduled   bool            `json:"undo_scheduled"`
	UndoDelaySecs   int             `json:"undo_delay_secs"`
	UndoScheduleErr string          `json:"undo_schedule_err,omitempty"`
	// RevertUnit is the systemd unit name of the armed self-revert timer (unique
	// per invocation). The operator cancels the timer by stopping this unit's
	// .timer/.service. Empty when the timer was not armed.
	RevertUnit string `json:"revert_unit,omitempty"`
}

// EnableSafe enables a default-drop firewall WITHOUT locking out the agent.
//
// Order of operations (all guarded, all shell-quoted):
//  1. Inject an explicit allow rule for the agent's own management port. This
//     ALWAYS runs first so the door is nailed open before any tightening.
//  2. Refuse the operation unless either the agent port is now allowed, or the
//     operator passed forceIHaveConsole (they accept lockout risk knowingly).
//  3. Arm a self-reverting (dead-man) undo timer via systemd-run so a mistake
//     auto-rolls-back after undoDelaySecs. This MUST be confirmed armed before
//     the lockdown takes effect: if arming fails we fail CLOSED and do NOT apply
//     the firewall (returning the arming error), so a host that cannot schedule
//     the self-revert can never be locked down with no rollback.
//  4. Only then apply the enable.
//
// forceIHaveConsole bypasses BOTH safety refusals — the lockout-verification
// refusal (step 2) AND the dead-man-arming refusal (step 3) — because the
// operator has asserted out-of-band console access to recover. The agent-port
// allow is always injected regardless.
func (e *FirewallEngine) EnableSafe(backend FirewallBackend, forceIHaveConsole bool, undoDelaySecs int) (EnableSafeResult, error) {
	if e.Exec == nil {
		return EnableSafeResult{}, fmt.Errorf("firewall engine: Exec is nil")
	}
	if e.AgentPort < 1 || e.AgentPort > 65535 {
		return EnableSafeResult{}, fmt.Errorf("refusing to enable firewall: agent port %d is invalid; cannot guarantee the agent stays reachable", e.AgentPort)
	}
	if backend == FirewallBackendUnknown {
		return EnableSafeResult{}, fmt.Errorf("no supported firewall backend detected; refusing to enable")
	}
	if undoDelaySecs <= 0 {
		undoDelaySecs = 60
	}

	result := EnableSafeResult{Backend: backend, AgentPort: e.AgentPort, UndoDelaySecs: undoDelaySecs}

	// 1) ALWAYS allow the agent's own port first.
	agentSpec := PortSpec{Port: e.AgentPort, Proto: ProtoTCP}
	allowCmd, err := allowPortCommand(backend, agentSpec)
	if err != nil {
		return result, err
	}
	result.AgentAllowCmd = allowCmd
	if _, err := e.run(allowCmd); err != nil {
		return result, fmt.Errorf("failed to allow agent port %d before enabling firewall: %w", e.AgentPort, err)
	}

	// 2) Verify the agent port is actually open now (unless forced).
	//
	// Fail CLOSED: this guard must never let a default-drop firewall through
	// without a POSITIVE confirmation that the agent port survives. There are two
	// ways the verification can fail to confirm that — and both must refuse:
	//   a) the ruleset read errored, so we cannot see whether the port is allowed; or
	//   b) the ruleset read succeeded but does not show the agent port allowed.
	// Only --force-i-have-console (out-of-band console access) may bypass either.
	if !forceIHaveConsole {
		dump, derr := e.dumpRuleset(backend)
		switch {
		case derr != nil:
			return result, fmt.Errorf(
				"refusing to enable firewall: cannot verify the agent port %d is allowed (ruleset read failed: %v); "+
					"refusing to enable a default-drop firewall that might lock out the agent. "+
					"Re-run with --force-i-have-console only if you have out-of-band console access to recover",
				e.AgentPort, derr)
		case !rulesetAllowsPort(dump, e.AgentPort):
			return result, fmt.Errorf(
				"refusing to enable firewall: agent port %d does not appear to be allowed in the resulting ruleset, "+
					"which would drop the agent connection. Re-run with --force-i-have-console only if you have out-of-band "+
					"console access to recover", e.AgentPort)
		}
	}

	// 3) Arm the self-reverting (dead-man) undo BEFORE applying, so even an apply
	//    that cuts us off rolls back automatically.
	//
	// Fail CLOSED: the lockdown must NOT take effect unless the auto-revert is
	// confirmed armed. If arming fails (the schedule command could not be built, or
	// running it errored), applying the default-drop firewall anyway could lock the
	// operator out permanently with no automatic rollback. So on an arm failure we
	// refuse to enable and return the arming error — UNLESS the operator passed
	// --force-i-have-console, in which case they have accepted the lockout risk and
	// have out-of-band recovery. forceIHaveConsole is the ONLY bypass; without it,
	// no firewall is applied when the dead-man cannot be armed.
	undoCmd := disableCommand(backend)
	result.UndoCmd = undoCmd
	scheduleCmd, revertUnit, scheduleErr := scheduleSelfRevertCommand(undoCmd, undoDelaySecs)
	if scheduleErr != nil {
		result.UndoScheduleErr = scheduleErr.Error()
		if !forceIHaveConsole {
			return result, fmt.Errorf(
				"refusing to enable firewall: could not arm the auto-revert (dead-man) timer: %w; "+
					"applying a default-drop firewall without a confirmed self-revert risks a permanent lockout. "+
					"Re-run with --force-i-have-console only if you have out-of-band console access to recover",
				scheduleErr)
		}
	} else {
		result.RevertUnit = revertUnit
		if _, err := e.run(scheduleCmd); err != nil {
			result.UndoScheduleErr = err.Error()
			if !forceIHaveConsole {
				// Clear the unit we never actually armed so the result doesn't
				// advertise a non-existent revert timer to the operator.
				result.RevertUnit = ""
				return result, fmt.Errorf(
					"refusing to enable firewall: failed to arm the auto-revert (dead-man) timer (unit %s): %w; "+
						"applying a default-drop firewall without a confirmed self-revert risks a permanent lockout. "+
						"Re-run with --force-i-have-console only if you have out-of-band console access to recover",
					revertUnit, err)
			}
		} else {
			result.UndoScheduled = true
		}
	}

	// 4) Apply the enable. By this point the dead-man is confirmed armed
	//    (result.UndoScheduled), or the operator explicitly accepted the lockout
	//    risk with --force-i-have-console.
	enableCmd := enableCommand(backend)
	result.EnableCmd = enableCmd
	if _, err := e.run(enableCmd); err != nil {
		return result, fmt.Errorf("enable firewall failed: %w", err)
	}
	return result, nil
}

// run executes a command and turns a non-zero exit into an error with stderr.
func (e *FirewallEngine) run(command string) (string, error) {
	if e.Exec == nil {
		return "", fmt.Errorf("firewall engine: Exec is nil")
	}
	out, code, err := e.Exec(command)
	if err != nil {
		return out, err
	}
	if code != 0 {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = fmt.Sprintf("exit code %d", code)
		}
		return out, fmt.Errorf("%s", msg)
	}
	return out, nil
}

// --- command builders (pure, shell-quoted) ---

// allowPortCommand returns the backend-native command to allow a port/proto.
func allowPortCommand(backend FirewallBackend, spec PortSpec) (string, error) {
	portProto := spec.String() // "443/tcp" — digits + known proto, safe but quoted below
	switch backend {
	case FirewallBackendUFW:
		return "ufw allow " + shellQuote(portProto), nil
	case FirewallBackendFirewalld:
		return "firewall-cmd --permanent --add-port=" + shellQuote(portProto) + " && firewall-cmd --reload", nil
	case FirewallBackendNftables:
		// Ensure an inet filter input chain exists, then insert (highest priority)
		// an accept for the port so it precedes any drop policy.
		return "nft add table inet fleet 2>/dev/null; " +
			"nft add chain inet fleet input '{ type filter hook input priority 0 ; }' 2>/dev/null; " +
			"nft insert rule inet fleet input " + shellQuote(string(spec.Proto)) +
			" dport " + shellQuote(strconv.Itoa(spec.Port)) + " accept", nil
	case FirewallBackendIptables:
		// Insert at the top of INPUT so the accept beats a later drop.
		return "iptables -I INPUT 1 -p " + shellQuote(string(spec.Proto)) +
			" --dport " + shellQuote(strconv.Itoa(spec.Port)) + " -j ACCEPT", nil
	default:
		return "", fmt.Errorf("unsupported firewall backend %q", backend)
	}
}

// enableCommand returns the backend-native command to enable a default-drop policy.
func enableCommand(backend FirewallBackend) string {
	switch backend {
	case FirewallBackendUFW:
		// --force avoids the interactive y/n prompt; the agent-port allow already ran.
		return "ufw --force enable"
	case FirewallBackendFirewalld:
		return "systemctl enable --now firewalld"
	case FirewallBackendNftables:
		// Set the input chain policy to drop (the agent accept rule precedes it).
		// The chain was already created by allowPortCommand (step 1), so re-adding
		// it with `nft add chain ... { ... policy drop ; }` fails "File exists" and
		// makes enable return non-zero on every nftables host. Ensure the chain
		// exists idempotently (add is a no-op-on-exists thanks to 2>/dev/null), then
		// MODIFY the existing chain's policy with `nft chain ... { policy drop ; }`,
		// which is idempotent whether or not the chain already had a policy.
		return "nft add table inet fleet 2>/dev/null; " +
			"nft add chain inet fleet input '{ type filter hook input priority 0 ; }' 2>/dev/null; " +
			"nft chain inet fleet input '{ policy drop ; }'"
	case FirewallBackendIptables:
		return "iptables -P INPUT DROP"
	default:
		return "true"
	}
}

// disableCommand returns the backend-native UNDO that reopens the host. This is
// what the self-revert timer runs, and what `enable --safe` schedules.
func disableCommand(backend FirewallBackend) string {
	switch backend {
	case FirewallBackendUFW:
		return "ufw --force disable"
	case FirewallBackendFirewalld:
		return "systemctl stop firewalld"
	case FirewallBackendNftables:
		// Undo ONLY what EnableSafe added (the `inet fleet` table), never the whole
		// host ruleset. First flip the fleet chain's policy back to accept so the
		// host reopens immediately (idempotent; does not fail "File exists" the way a
		// re-`add` would), then delete the fleet table outright to remove the drop
		// policy and the agent-port rule we inserted. `delete table` also removes the
		// chain, so the host returns to exactly its pre-EnableSafe state for every
		// OTHER table. Both steps tolerate a missing table (2>/dev/null) so the undo
		// is safe to run even if the table is already gone. We deliberately do NOT
		// `nft flush ruleset` here — that would wipe every unrelated host firewall
		// rule, not just fleet's.
		return "nft chain inet fleet input '{ policy accept ; }' 2>/dev/null; " +
			"nft delete table inet fleet 2>/dev/null || true"
	case FirewallBackendIptables:
		return "iptables -P INPUT ACCEPT"
	default:
		return "true"
	}
}

// selfRevertSeq is a process-local counter that makes successive self-revert unit
// names unique within one controller process. Combined with the pid and the
// wall-clock nanosecond it yields a unit name that does not collide on a second
// run against the same host (the static name "fleet-fw-safe-revert" made
// systemd-run fail "unit already exists" on the 2nd enable).
var selfRevertSeq uint64

// selfRevertUnitName returns a charset-safe, per-invocation-unique systemd unit
// name for the self-revert timer. It uses only [a-z0-9-], so it is always a valid
// systemd unit and needs no quoting (it is shell-quoted anyway).
func selfRevertUnitName() string {
	n := atomic.AddUint64(&selfRevertSeq, 1)
	return fmt.Sprintf("fleet-fw-safe-revert-%d-%d-%d", os.Getpid(), time.Now().UnixNano(), n)
}

// scheduleSelfRevertCommand wraps an undo command in a one-shot self-reverting
// timer so the host auto-reverts after delaySecs unless the operator cancels it.
// This is the deadman: if `enable` cut us off, the timer fires and reopens the
// host.
//
// Two bugs are fixed here. (1) The unit name is now UNIQUE per invocation, so a
// second `enable --safe` against the same host no longer fails "unit already
// exists". (2) On hosts without systemd-run, we fall back to a detached
// `setsid sh -c 'sleep N; <undo>'` loop so the self-revert safety net still arms,
// mirroring core.BuildGuardArmCommand's schedule block.
//
// systemd-run executes the argv directly (no shell), so on the systemd path the
// only injection surface is undoCmd, which we shell-quote as a single argument to
// `/bin/sh -c`. The setsid fallback runs `sleep N; <undo>` as one quoted script.
//
// It returns the scheduling command AND the (unique) systemd unit name, so the
// caller can tell the operator exactly which unit to stop to cancel the timer.
func scheduleSelfRevertCommand(undoCmd string, delaySecs int) (cmd, unit string, err error) {
	if strings.TrimSpace(undoCmd) == "" {
		return "", "", fmt.Errorf("empty undo command")
	}
	if delaySecs <= 0 {
		delaySecs = 60
	}
	secs := strconv.Itoa(delaySecs)
	onActive := shellQuote(secs + "s")
	unit = selfRevertUnitName()
	systemdPath := "systemd-run --on-active=" + onActive + " --unit=" + shellQuote(unit) +
		" /bin/sh -c " + shellQuote(undoCmd)
	// Detached fallback: a session-independent sleep loop that runs the undo once
	// the delay elapses, fully detached from this SSH session's stdio.
	fallback := "setsid sh -c " + shellQuote("sleep "+secs+"; "+undoCmd) +
		" </dev/null >/dev/null 2>&1 &"
	cmd = "if command -v systemd-run >/dev/null 2>&1; then " +
		systemdPath + " >/dev/null 2>&1; else " +
		fallback + " fi"
	return cmd, unit, nil
}

// --- normalization helpers (pure) ---

// backendActive reports whether the dumped ruleset indicates the firewall is on.
func backendActive(backend FirewallBackend, dump string) bool {
	low := strings.ToLower(dump)
	switch backend {
	case FirewallBackendUFW:
		return strings.Contains(low, "status: active")
	case FirewallBackendFirewalld:
		return strings.Contains(low, "running")
	case FirewallBackendNftables:
		return strings.Contains(low, "policy drop") || strings.Contains(low, "chain input")
	case FirewallBackendIptables:
		return strings.Contains(low, "-p ") || strings.Contains(low, ":input ")
	default:
		return false
	}
}

// backendDefaultDrop reports whether the default inbound policy drops/denies.
func backendDefaultDrop(backend FirewallBackend, dump string) bool {
	low := strings.ToLower(dump)
	switch backend {
	case FirewallBackendUFW:
		return strings.Contains(low, "deny (incoming)") || strings.Contains(low, "deny incoming")
	case FirewallBackendNftables:
		return strings.Contains(low, "policy drop")
	case FirewallBackendIptables:
		return strings.Contains(low, ":input drop") || strings.Contains(low, "-p input drop")
	case FirewallBackendFirewalld:
		// firewalld zones are deny-by-default when running.
		return strings.Contains(low, "running")
	default:
		return false
	}
}

// rulesetAllowsPort reports whether the dumped ruleset appears to allow inbound
// traffic to `port` on any protocol. It matches common backend forms but uses
// WORD-BOUNDARY matching so that port N is never matched inside a longer number:
// a bare substring scan would find "--dport 22" inside "--dport 2222 -j ACCEPT",
// "22/tcp" inside "2222/tcp", and "ports: 22" inside "ports: 2222", producing a
// FALSE positive that could let EnableSafe's lockout guard pass while the agent
// port is actually blocked. This is SAFETY-CRITICAL: a wrong "allowed" here can
// lock the controller out, so each port token must be bounded by a non-digit
// (start/end of string or a non-digit neighbour).
//
// It remains permissive across the forms it recognizes — the caller still injects
// the explicit allow rule regardless of what this returns — but the digit
// boundaries make a longer-number false positive impossible.
func rulesetAllowsPort(dump string, port int) bool {
	if port < 1 {
		return false
	}
	low := strings.ToLower(dump)
	p := strconv.Itoa(port)
	// Each pattern is split into a prefix and an optional suffix that must surround
	// the exact port token. containsPortToken enforces that the digits immediately
	// before/after the port are non-digits (word boundary), so 22 != 2222.
	patterns := []struct {
		prefix, suffix string
		// requireAllow restricts the match to a line whose ACTION is allow/accept.
		// The bare space-delimited column form (" "+port+" ") matches the port
		// anywhere on a line, so a whole-dump substring scan would FALSELY report
		// the port as allowed when it merely appears inside a DENY/reject rule or
		// a comment — weakening the fail-closed positive confirmation. Pinning that
		// form to an allow-context line keeps a deny rule mentioning the port from
		// satisfying "port is allowed". The other forms are either self-evident
		// allow rules (nft "... accept", iptables "--dport ... -j ACCEPT") or only
		// appear in allow-lists (firewalld "ports:"/"port="); the per-line deny
		// skip below additionally guards those against a deny rule reusing the form.
		requireAllow bool
	}{
		{prefix: "dport ", suffix: " accept"}, // nft: "tcp dport 2222 accept"
		{prefix: "--dport ", suffix: ""},      // iptables: "--dport 2222 -j ACCEPT"
		{prefix: "", suffix: "/tcp"},          // ufw / firewalld port form "2222/tcp"
		{prefix: "", suffix: "/udp"},
		{prefix: "port=", suffix: ""},                  // firewalld: "port=2222"
		{prefix: "ports: ", suffix: ""},                // firewalld: "ports: 2222 443" (the zone's allow-list)
		{prefix: " ", suffix: " ", requireAllow: true}, // ufw column form "443  ALLOW ..."
	}
	// The iptables "--dport N" and firewalld "port=N" / "N/tcp" forms are emitted
	// in BOTH accept and drop/reject rules, so confirm the matching line is not a
	// deny rule before trusting it. We scan line-by-line so the allow/deny context
	// is evaluated against the SAME rule the port appears in, not the whole dump.
	for _, line := range strings.Split(low, "\n") {
		if lineDeniesTraffic(line) {
			continue // a deny/reject/drop rule never confirms "allowed"
		}
		lineAllows := lineAllowsTraffic(line)
		for _, pat := range patterns {
			if pat.requireAllow && !lineAllows {
				continue
			}
			if containsPortToken(line, pat.prefix, p, pat.suffix) {
				return true
			}
		}
		// ufw prints "443/tcp ALLOW" or "443 ALLOW IN"; catch the leading-token form.
		fields := strings.Fields(line)
		if len(fields) > 0 {
			token := strings.SplitN(fields[0], "/", 2)[0]
			if token == p && lineAllows {
				return true
			}
		}
	}
	return false
}

// lineDeniesTraffic reports whether a (lower-cased) ruleset line is a
// deny/reject/drop rule. Such a line must never be read as confirming a port is
// allowed, even if it mentions the port — that would weaken the fail-closed
// positive confirmation EnableSafe relies on.
func lineDeniesTraffic(line string) bool {
	for _, verb := range []string{"deny", "reject", "drop"} {
		if strings.Contains(line, verb) {
			return true
		}
	}
	return false
}

// lineAllowsTraffic reports whether a (lower-cased) ruleset line carries an
// allow/accept action verb. Used to pin the ambiguous port forms (bare column
// form, firewalld "ports:" list) to an actual allow rule rather than any line
// that happens to contain the port.
func lineAllowsTraffic(line string) bool {
	for _, verb := range []string{"allow", "accept"} {
		if strings.Contains(line, verb) {
			return true
		}
	}
	return false
}

// containsPortToken reports whether s contains prefix+port+suffix where the port
// is not part of a longer run of digits — i.e. the character immediately before
// the port (if any) and immediately after it (if no suffix pins it) is a
// non-digit. This is the word boundary that prevents matching 22 inside 2222.
func containsPortToken(s, prefix, port, suffix string) bool {
	needle := prefix + port + suffix
	from := 0
	for {
		idx := strings.Index(s[from:], needle)
		if idx < 0 {
			return false
		}
		idx += from
		// Position of the port digits within s.
		portStart := idx + len(prefix)
		portEnd := portStart + len(port)
		leftOK := portStart == 0 || !isASCIIDigit(s[portStart-1])
		// When a suffix is present it already pins the right side to a non-digit
		// boundary (e.g. "/tcp"); otherwise the next char must be a non-digit.
		rightOK := suffix != "" || portEnd >= len(s) || !isASCIIDigit(s[portEnd])
		if leftOK && rightOK {
			return true
		}
		from = idx + 1
	}
}

// isASCIIDigit reports whether b is an ASCII decimal digit.
func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// SortedBackends returns the supported backends in a stable order (for help/tests).
func SortedBackends() []FirewallBackend {
	bs := []FirewallBackend{
		FirewallBackendUFW,
		FirewallBackendFirewalld,
		FirewallBackendNftables,
		FirewallBackendIptables,
	}
	sort.Slice(bs, func(i, j int) bool { return bs[i] < bs[j] })
	return bs
}
