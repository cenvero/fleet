// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cenvero/fleet/internal/transport"
)

// FL-029 — firewall-safe, and the agent-lockout gate.
//
// SAFETY-CRITICAL. Locking the controller out of its own agent is unrecoverable
// without console access, so the cardinal rule everywhere in this file is: a
// change that could block traffic runs only after fleet has READ the host's
// rules and confirmed the agent connection survives it. When it would not, or
// when the state cannot be read, the change is refused (fail closed) with an
// AgentLockoutError naming the exact command to run first. Only the deliberate
// --force-i-have-console override goes past a refusal, and callers audit-log
// every refusal and every override.
//
// What "survives" means depends on how the controller reaches the agent:
//   - direct mode: the agent listens on ServerRecord.Port, which must stay
//     allowed inbound (TCP) with no rule denying it;
//   - reverse mode: the agent dials out, so no inbound port matters, but the
//     input policy must keep established/related traffic (the replies on the
//     agent's own outbound connection) and outgoing traffic must stay allowed
//     so the agent can dial again after a disconnect.
//
// Two stacks share the gate: `fleet fw` (this file's FirewallEngine, four
// backends over shell.exec) and the legacy `fleet firewall` / `fleet port`
// RPCs (ufw on the agent), which App wires through the same decisions in
// app.go.
//
// All remote commands are built from shell-quoted, validated values. The engine
// logic is pure and exercised through a fake exec function (FirewallExec); it
// does not depend on *App.

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
// agent. It never depends on *App: callers pass an exec function.
type FirewallEngine struct {
	// AgentPort is the direct agent's listening port, which must stay reachable.
	AgentPort int
	// Exec runs a remote command. Required.
	Exec FirewallExec
	// Server names the server in refusals and remediation commands.
	Server string
	// Mode is how the controller reaches the agent. Anything but reverse is
	// treated as direct.
	Mode transport.Mode
	// BeforeOverride, when set, runs before --force-i-have-console skips a
	// refusal (callers audit-log it there). An error stops the change.
	BeforeOverride func(*AgentLockoutError) error
}

func (e *FirewallEngine) link() AgentLink {
	return AgentLink{Server: e.Server, Mode: e.Mode, Port: e.AgentPort}
}

func (e *FirewallEngine) mode() string {
	if e.Mode == transport.ModeReverse {
		return transport.ModeReverse.String()
	}
	return transport.ModeDirect.String()
}

// FirewallStatusReport is the normalized result of a status probe.
type FirewallStatusReport struct {
	Backend     FirewallBackend `json:"backend"`
	Active      bool            `json:"active"`
	DefaultDrop bool            `json:"default_drop"`
	AgentPort   int             `json:"agent_port"`
	// AgentPortAllowed is true when the agent port is allowed, and not denied,
	// in the rules `fleet fw enable` checks (for ufw, including rules pending
	// while it is inactive).
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
	st := parseGuardState(backend, dump, nil, e.AgentPort)
	report.AgentPortAllowed = st.readErr == "" && st.agentAllowed && st.agentDenied == ""
	return report, nil
}

// ufwDumpCommand reads, without changing anything, what ufw enforces now and
// will enforce once enabled: status, the pending rule list (`ufw status` shows
// no rules while ufw is inactive), the default policies, and whether
// before.rules still accepts established/related traffic.
const ufwDumpCommand = "ufw status verbose 2>&1; " +
	"echo '" + guardMarker + "added'; ufw show added 2>&1; " +
	"echo '" + guardMarker + "defaults'; grep -E '^DEFAULT_(INPUT|OUTPUT)_POLICY=' /etc/default/ufw 2>&1; " +
	"echo '" + guardMarker + "before-rules'; grep -c -E -e '^-A ufw-before-input .*--(ct)?state RELATED,ESTABLISHED -j ACCEPT' /etc/ufw/before.rules 2>&1; " +
	"true"

// firewalldDumpCommand reads the running and permanent default zone when
// firewalld runs, or the permanent zone offline when it is stopped (the daemon
// is needed for firewall-cmd), plus whether the unit is enabled at boot.
const firewalldDumpCommand = "if firewall-cmd --state >/dev/null 2>&1; then " +
	"echo '" + guardMarker + "firewalld-running'; firewall-cmd --list-all 2>&1; " +
	"echo '" + guardMarker + "permanent'; firewall-cmd --permanent --list-all 2>&1; " +
	"else echo '" + guardMarker + "firewalld-stopped'; echo '" + guardMarker + "permanent'; firewall-offline-cmd --list-all 2>&1; fi; " +
	"echo \"" + guardMarker + "unit $(systemctl is-enabled firewalld 2>&1)\""

// dumpRuleset returns the backend-native, read-only state dump the gate and
// the status report parse.
func (e *FirewallEngine) dumpRuleset(backend FirewallBackend) (string, error) {
	var cmd string
	switch backend {
	case FirewallBackendUFW:
		cmd = ufwDumpCommand
	case FirewallBackendFirewalld:
		cmd = firewalldDumpCommand
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
// the given protocol. Opening a port never reduces reachability, and none of
// the commands replaces the policy (see allowPortCommand), so it needs no gate.
func (e *FirewallEngine) AllowPort(backend FirewallBackend, spec PortSpec) (string, error) {
	cmd, err := allowPortCommand(backend, spec)
	if err != nil {
		return "", err
	}
	return e.run(cmd)
}

// Self-revert methods reported by the host when the timer is armed.
const (
	RevertMethodSystemd = "systemd-run"
	RevertMethodSetsid  = "setsid"
	revertArmedPrefix   = "fleet-fw-safe-revert:armed:"
)

// EnableSafeResult reports the outcome of a guarded `enable --safe`.
type EnableSafeResult struct {
	Backend   FirewallBackend `json:"backend"`
	Mode      string          `json:"mode"`
	AgentPort int             `json:"agent_port"`
	// AgentPortConfirmed is true when the agent port was already allowed before
	// anything changed (direct mode).
	AgentPortConfirmed bool `json:"agent_port_confirmed"`
	// AgentAllowCmd is the agent-port allow enable ran itself, which happens
	// only under --force-i-have-console when the port was not confirmed.
	AgentAllowCmd   string `json:"agent_allow_cmd,omitempty"`
	EnableCmd       string `json:"enable_cmd"`
	UndoCmd         string `json:"undo_cmd"`
	UndoScheduled   bool   `json:"undo_scheduled"`
	UndoDelaySecs   int    `json:"undo_delay_secs"`
	UndoScheduleErr string `json:"undo_schedule_err,omitempty"`
	// RevertMethod is how the host armed the self-revert: "systemd-run" or
	// "setsid". RevertID names the timer: the systemd unit, or the setsid job's
	// $0. CancelCmd, run on the server, cancels it. Nothing cancels it
	// automatically.
	RevertMethod string `json:"revert_method,omitempty"`
	RevertID     string `json:"revert_id,omitempty"`
	CancelCmd    string `json:"cancel_cmd,omitempty"`
	// RevertUnit is the systemd unit of the timer, set only on the systemd-run
	// path (the setsid path has no unit).
	RevertUnit string `json:"revert_unit,omitempty"`
	// Overrode lists the refusals --force-i-have-console skipped.
	Overrode []string `json:"overrode,omitempty"`
}

// EnableSafe enables a default-drop firewall WITHOUT locking out the agent.
//
// Order of operations:
//  1. Read the host's state. Nothing has changed yet.
//  2. The gate: refuse unless the agent connection survives (direct: the agent
//     port is allowed and not denied; reverse: established/related traffic is
//     kept and outgoing traffic allowed). The refusal names the command to run
//     first (`fleet fw allow`). The state is read in full or the call refuses.
//  3. Only under --force-i-have-console, with the agent port not confirmed,
//     allow it now (idempotently) before anything tightens.
//  4. Arm the self-reverting (dead-man) undo, built from the snapshot taken in
//     step 1, and require the host to confirm which path armed it. If arming
//     fails we fail CLOSED and do not apply the firewall.
//  5. Only then apply the enable.
//
// forceIHaveConsole skips the refusals in steps 2 and 4 because the operator
// has asserted out-of-band console access; BeforeOverride runs first.
func (e *FirewallEngine) EnableSafe(backend FirewallBackend, forceIHaveConsole bool, undoDelaySecs int) (EnableSafeResult, error) {
	if e.Exec == nil {
		return EnableSafeResult{}, fmt.Errorf("firewall engine: Exec is nil")
	}
	reverse := e.Mode == transport.ModeReverse
	if !reverse && (e.AgentPort < 1 || e.AgentPort > 65535) {
		refusal := lockout("enable the firewall", fmt.Sprintf("the agent port %d is invalid, so fleet cannot tell which port must stay open; "+
			"set the server record's port to the port the agent listens on", e.AgentPort), "")
		refusal.Server, refusal.Op, refusal.Unskippable = e.Server, "fw.enable", true
		if refusal.Server == "" {
			refusal.Server = "<server>"
		}
		return EnableSafeResult{}, refusal
	}
	if backend == FirewallBackendUnknown {
		return EnableSafeResult{}, fmt.Errorf("no supported firewall backend detected; refusing to enable")
	}
	if undoDelaySecs <= 0 {
		undoDelaySecs = 60
	}
	result := EnableSafeResult{Backend: backend, Mode: e.mode(), UndoDelaySecs: undoDelaySecs}
	if !reverse {
		result.AgentPort = e.AgentPort
	}

	// 1) Read the state before changing anything.
	dump, derr := e.dumpRuleset(backend)
	state := parseGuardState(backend, dump, derr, e.AgentPort)
	result.AgentPortConfirmed = !reverse && state.readErr == "" && state.agentAllowed && state.agentDenied == ""

	// 2) The gate.
	if refusal := enableLockout(e.link(), state, FixFwAllowCommand(e.Server, e.AgentPort)); refusal != nil {
		if err := e.overrideOrRefuse(forceIHaveConsole, refusal, &result); err != nil {
			return result, err
		}
	}

	// 3) The agent port is unconfirmed only under the override: allow it before
	//    anything tightens. Skipped when it was confirmed, so an operator's
	//    narrower allow (one source address, say) is never widened.
	if !reverse && !result.AgentPortConfirmed {
		allowCmd, err := allowPortCommand(backend, PortSpec{Port: e.AgentPort, Proto: ProtoTCP})
		if err != nil {
			return result, err
		}
		result.AgentAllowCmd = allowCmd
		if _, err := e.run(allowCmd); err != nil {
			return result, fmt.Errorf("failed to allow agent port %d before enabling firewall: %w", e.AgentPort, err)
		}
	}

	// 4) Arm the self-revert BEFORE applying, so even an apply that cuts us off
	//    rolls back on its own.
	undoCmd := undoCommand(backend, state)
	result.UndoCmd = undoCmd
	scheduleCmd, revertID, armErr := scheduleSelfRevertCommand(undoCmd, undoDelaySecs)
	if armErr == nil {
		out, err := e.run(scheduleCmd)
		method := revertMethodFrom(out)
		switch {
		case err != nil:
			armErr = err
		case method == "":
			armErr = fmt.Errorf("the host did not confirm the timer was armed (output: %s)", firstLine(out))
		default:
			result.UndoScheduled = true
			result.RevertMethod = method
			result.RevertID = revertID
			result.CancelCmd = cancelRevertCommand(method, revertID)
			if method == RevertMethodSystemd {
				result.RevertUnit = revertID
			}
		}
	}
	if armErr != nil {
		result.UndoScheduleErr = armErr.Error()
		refusal := lockout("enable the firewall", fmt.Sprintf("the auto-revert (dead-man) timer could not be armed (%v), and a default-deny "+
			"firewall without a confirmed self-revert risks a permanent lockout", armErr), "")
		if err := e.overrideOrRefuse(forceIHaveConsole, refusal, &result); err != nil {
			return result, err
		}
	}

	// 5) Apply. The dead-man is confirmed armed, or the operator explicitly
	//    accepted the risk with --force-i-have-console.
	enableCmd := enableCommand(backend)
	result.EnableCmd = enableCmd
	if _, err := e.run(enableCmd); err != nil {
		return result, fmt.Errorf("enable firewall failed: %w", err)
	}
	return result, nil
}

// overrideOrRefuse returns the refusal unless force is set; with force it runs
// BeforeOverride (the audit hook) and records what was skipped.
func (e *FirewallEngine) overrideOrRefuse(force bool, refusal *AgentLockoutError, result *EnableSafeResult) error {
	refusal.Server, refusal.Op = e.Server, "fw.enable"
	if refusal.Server == "" {
		refusal.Server = "<server>"
	}
	if !force {
		return refusal
	}
	if e.BeforeOverride != nil {
		if err := e.BeforeOverride(refusal); err != nil {
			return fmt.Errorf("not using --force-i-have-console without an audit record: %w", err)
		}
	}
	result.Overrode = append(result.Overrode, refusal.Reason)
	return nil
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

// nftEnsureChain makes sure fleet's own input chain exists. Creating a base
// chain without a policy gives it "accept"; re-adding an existing chain without
// a policy leaves its policy unchanged.
const nftEnsureChain = "nft add table inet fleet 2>/dev/null; " +
	"nft add chain inet fleet input '{ type filter hook input priority 0 ; }' 2>/dev/null; "

// nftEnsureRule inserts a rule into the fleet input chain unless the chain
// already lists `match`.
func nftEnsureRule(match, insert string) string {
	return "{ nft list chain inet fleet input 2>/dev/null | grep -qF -- " + shellQuote(match) +
		" || nft insert rule inet fleet input " + insert + "; }"
}

// iptablesEnsure inserts the INPUT rule `match -j ACCEPT`, tagged so the undo
// can remove exactly it, unless the rule (tagged or not) already exists.
func iptablesEnsure(match string) string {
	plain := match + " -j ACCEPT"
	tagged := match + " -m comment --comment " + fleetSafeTag + " -j ACCEPT"
	return "{ iptables -C INPUT " + plain + " 2>/dev/null || iptables -C INPUT " + tagged +
		" 2>/dev/null || iptables -I INPUT 1 " + tagged + "; }"
}

// fleetSafeTag marks the rules `enable --safe` adds, so its undo can delete
// exactly those.
const fleetSafeTag = "fleet-fw-safe"

// allowPortCommand returns the backend-native, idempotent command to allow a
// port/proto. None of them replaces the policy: in particular firewalld is not
// `--reload`ed, because a reload swaps the running rules for the permanent
// ones and drops any runtime-only allow (possibly the agent's).
func allowPortCommand(backend FirewallBackend, spec PortSpec) (string, error) {
	portProto := spec.String() // "443/tcp" — digits + known proto, safe but quoted below
	proto, port := string(spec.Proto), strconv.Itoa(spec.Port)
	switch backend {
	case FirewallBackendUFW:
		return "ufw allow " + shellQuote(portProto), nil
	case FirewallBackendFirewalld:
		// firewall-cmd needs the daemon; while it is stopped, write the
		// permanent config offline (it loads when firewalld starts).
		return "if firewall-cmd --state >/dev/null 2>&1; then " +
			"firewall-cmd --permanent --add-port=" + shellQuote(portProto) + " && firewall-cmd --add-port=" + shellQuote(portProto) + "; " +
			"else firewall-offline-cmd --add-port=" + shellQuote(portProto) + "; fi", nil
	case FirewallBackendNftables:
		// Insert (highest priority) an accept so it precedes the drop policy.
		return nftEnsureChain + nftEnsureRule(proto+" dport "+port+" accept",
			shellQuote(proto)+" dport "+shellQuote(port)+" accept"), nil
	case FirewallBackendIptables:
		// Insert at the top of INPUT so the accept beats a later drop.
		rule := "-p " + shellQuote(proto) + " --dport " + shellQuote(port) + " -j ACCEPT"
		return "iptables -C INPUT " + rule + " 2>/dev/null || iptables -I INPUT 1 " + rule, nil
	default:
		return "", fmt.Errorf("unsupported firewall backend %q", backend)
	}
}

// enableCommand returns the backend-native command to enable a default-drop
// policy. For nft and iptables it first accepts established/related traffic
// (replies on connections the host opened, including a reverse agent's own)
// and loopback, idempotently, and sets the drop policy only once both accepts
// are in place. ufw and firewalld accept both in their stock rules.
func enableCommand(backend FirewallBackend) string {
	switch backend {
	case FirewallBackendUFW:
		// --force avoids the interactive y/n prompt.
		return "ufw --force enable"
	case FirewallBackendFirewalld:
		return "systemctl enable --now firewalld"
	case FirewallBackendNftables:
		// `nft chain ... { policy drop ; }` MODIFIES the existing chain's policy;
		// re-adding it with `nft add chain ... { ... policy drop ; }` would fail
		// "File exists".
		tag := "comment '\"" + fleetSafeTag + "\"'"
		return nftEnsureChain +
			nftEnsureRule(`iif "lo" accept`, "iif lo accept "+tag) + " && " +
			nftEnsureRule("ct state established,related accept", "ct state established,related accept "+tag) + " && " +
			"nft chain inet fleet input '{ policy drop ; }'"
	case FirewallBackendIptables:
		return iptablesEnsure("-i lo") + " && " +
			iptablesEnsure("-m conntrack --ctstate ESTABLISHED,RELATED") + " && " +
			"iptables -P INPUT DROP"
	default:
		return "true"
	}
}

// disableCommand returns the backend-native command that reopens the host. It
// is the undo when fleet could not read what the host had before (only under
// --force-i-have-console); otherwise undoCommand restores that state.
func disableCommand(backend FirewallBackend) string {
	switch backend {
	case FirewallBackendUFW:
		return "ufw --force disable"
	case FirewallBackendFirewalld:
		return "systemctl stop firewalld"
	case FirewallBackendNftables:
		// Undo ONLY fleet's `inet fleet` table, never the whole host ruleset:
		// flip its policy back to accept first so the host reopens at once, then
		// delete it. Both steps tolerate a missing table. We deliberately do NOT
		// `nft flush ruleset` — that would wipe every unrelated host rule.
		return "nft chain inet fleet input '{ policy accept ; }' 2>/dev/null; " +
			"nft delete table inet fleet 2>/dev/null || true"
	case FirewallBackendIptables:
		return "iptables -P INPUT ACCEPT"
	default:
		return "true"
	}
}

// nftDeleteTaggedRules deletes, by handle, the fleet input chain rules that
// enableCommand tagged.
const nftDeleteTaggedRules = "for h in $(nft -a list chain inet fleet input 2>/dev/null | " +
	"sed -n 's/.*comment \"" + fleetSafeTag + "\".*# handle \\([0-9][0-9]*\\).*/\\1/p'); do " +
	"nft delete rule inet fleet input handle \"$h\"; done"

// undoCommand is what the self-revert runs: it reverses exactly what
// enableCommand changes, using the state read before the change. It restores
// the policy first, so the host reopens before anything else runs.
//
// What it cannot restore:
//   - It never removes an allow rule, including an agent-port allow made under
//     --force-i-have-console: removing an allow can only cut access, and the
//     undo exists to restore it.
//   - Changes anyone made between the enable and the undo are kept.
//   - ufw: the undo only turns ufw back off; rules stay in its config.
//   - firewalld: the undo restores the service's running/boot state only;
//     zone changes stay.
//   - nft and iptables: enable's changes are not persisted (nftables.conf,
//     iptables-save or netfilter-persistent are left alone), so a reboot also
//     drops them; ip6tables is never touched.
//   - When the earlier state could not be read, it falls back to
//     disableCommand, which reopens the host rather than restoring it.
func undoCommand(backend FirewallBackend, st firewallGuardState) string {
	if st.readErr != "" {
		return disableCommand(backend)
	}
	switch backend {
	case FirewallBackendUFW:
		if st.active {
			return "true" // ufw was already on: enable changed nothing to revert
		}
		return "ufw --force disable"
	case FirewallBackendFirewalld:
		switch {
		case st.active && st.firewalldBootEnabled == triNo:
			return "systemctl disable firewalld" // keep it running, as it was
		case st.active:
			return "true"
		case st.firewalldBootEnabled == triYes:
			return "systemctl stop firewalld"
		default:
			return "systemctl disable --now firewalld"
		}
	case FirewallBackendNftables:
		switch {
		case !st.nftTable:
			return "nft delete table inet fleet 2>/dev/null || true"
		case !st.nftChain:
			return "nft flush chain inet fleet input 2>/dev/null; nft delete chain inet fleet input 2>/dev/null || true"
		}
		policy := "accept"
		if st.nftPolicy == "drop" {
			policy = "drop"
		}
		return "nft chain inet fleet input '{ policy " + policy + " ; }' 2>/dev/null; " + nftDeleteTaggedRules + "; true"
	case FirewallBackendIptables:
		policy := "ACCEPT"
		if st.iptPolicy == "DROP" {
			policy = "DROP"
		}
		var b strings.Builder
		b.WriteString("iptables -P INPUT " + policy + "; ")
		for _, match := range []string{"-m conntrack --ctstate ESTABLISHED,RELATED", "-i lo"} {
			b.WriteString("while iptables -D INPUT " + match + " -m comment --comment " + fleetSafeTag + " -j ACCEPT 2>/dev/null; do :; done; ")
		}
		b.WriteString("true")
		return b.String()
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

// selfRevertUnitName returns a charset-safe, per-invocation-unique name for the
// self-revert timer: the systemd unit, or the setsid job's $0. It uses only
// [a-z0-9-], so it is a valid systemd unit and safe in a pkill pattern.
func selfRevertUnitName() string {
	n := atomic.AddUint64(&selfRevertSeq, 1)
	return fmt.Sprintf("fleet-fw-safe-revert-%d-%d-%d", os.Getpid(), time.Now().UnixNano(), n)
}

// scheduleSelfRevertCommand wraps an undo command in a one-shot self-reverting
// timer so the host reverts after delaySecs unless the operator cancels it.
// This is the dead-man: if `enable` cut us off, the timer fires and reopens the
// host. It fires just the same after a good enable unless cancelled.
//
// systemd-run is preferred (it executes the argv directly; undoCmd is quoted as
// one argument to `/bin/sh -c`). Without it, or when it fails, a detached
// `setsid sh -c 'sleep N; <undo>' <id>` runs the undo; the id as $0 is what
// cancelRevertCommand kills. The command prints which path armed the timer;
// with neither tool it exits 1, since a backgrounded missing `setsid` would
// still exit 0 and report a timer that does not exist.
//
// It returns the scheduling command and the revert id (the systemd unit name on
// that path).
func scheduleSelfRevertCommand(undoCmd string, delaySecs int) (cmd, id string, err error) {
	if strings.TrimSpace(undoCmd) == "" {
		return "", "", fmt.Errorf("empty undo command")
	}
	if delaySecs <= 0 {
		delaySecs = 60
	}
	secs := strconv.Itoa(delaySecs)
	id = selfRevertUnitName()
	systemdPath := "systemd-run --on-active=" + shellQuote(secs+"s") + " --unit=" + shellQuote(id) +
		" /bin/sh -c " + shellQuote(undoCmd) + " >/dev/null 2>&1"
	setsidPath := "setsid sh -c " + shellQuote("sleep "+secs+"; "+undoCmd) + " " + shellQuote(id) +
		" </dev/null >/dev/null 2>&1 &"
	cmd = "if command -v systemd-run >/dev/null 2>&1 && " + systemdPath + "; then " +
		"echo " + shellQuote(revertArmedPrefix+RevertMethodSystemd) + "; " +
		"elif command -v setsid >/dev/null 2>&1; then " + setsidPath + " " +
		"echo " + shellQuote(revertArmedPrefix+RevertMethodSetsid) + "; " +
		"else echo 'fleet-fw-safe-revert: neither systemd-run nor setsid is available' >&2; exit 1; fi"
	return cmd, id, nil
}

// revertMethodFrom returns the arming path the schedule command reported, or
// "" when it reported none.
func revertMethodFrom(out string) string {
	for _, line := range strings.Split(out, "\n") {
		switch strings.TrimSpace(line) {
		case revertArmedPrefix + RevertMethodSystemd:
			return RevertMethodSystemd
		case revertArmedPrefix + RevertMethodSetsid:
			return RevertMethodSetsid
		}
	}
	return ""
}

// cancelRevertCommand is the command, run on the server, that cancels an armed
// self-revert. On the setsid path the bracket keeps pkill from matching the
// shell that runs it.
func cancelRevertCommand(method, id string) string {
	if method == RevertMethodSetsid && id != "" {
		return `pkill -f "[` + id[:1] + `]` + id[1:] + `"`
	}
	return "systemctl stop " + id + ".timer"
}

// --- the agent-lockout gate (pure) ---

// AgentLink is how the controller reaches one server's agent: the facts the
// lockout gate protects.
type AgentLink struct {
	Server string
	Mode   transport.Mode
	// Port is the agent's listening port (ServerRecord.Port). Only a direct
	// agent listens; a reverse agent dials out and listens on nothing.
	Port int
}

func (l AgentLink) reverse() bool { return l.Mode == transport.ModeReverse }

// ResolveAgentLink returns the link the gate protects for a server record.
// Every mode other than reverse is treated as direct, the stricter case. A
// direct record must carry a real port: guarding a guessed one could allow the
// wrong port and drop the real agent connection, so fleet refuses instead.
func ResolveAgentLink(record ServerRecord) (AgentLink, error) {
	link := AgentLink{Server: record.Name, Mode: record.Mode, Port: record.Port}
	if link.reverse() {
		return link, nil
	}
	if record.Port < 1 || record.Port > 65535 {
		return link, fmt.Errorf("the server record for %q has no valid agent port (%d), so fleet cannot tell which port must stay open; "+
			"set the record's port to the port the agent listens on and retry", record.Name, record.Port)
	}
	return link, nil
}

// AgentLockoutError is the gate's refusal of a firewall change that could cut
// fleet off from a server's agent. Fix, when set, is the exact command that
// makes the change safe: run it first, then retry.
type AgentLockoutError struct {
	Server string
	// Op names the refused operation for the audit log: firewall.enable,
	// firewall.rule.add, port.close or fw.enable.
	Op string
	// Change is what was refused, in words ("enable the firewall").
	Change string
	Reason string
	Fix    string
	// Unskippable marks a refusal --force-i-have-console does not skip: fleet
	// does not know which port the agent listens on, so it could not even
	// allow that port first.
	Unskippable bool
}

func (e *AgentLockoutError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to %s on %s: %s", e.Change, e.Server, e.Reason)
	if e.Fix != "" {
		fmt.Fprintf(&b, ". Run this first, then retry: %s", e.Fix)
	}
	if e.Unskippable {
		b.WriteString(". --force-i-have-console cannot skip this check")
		return b.String()
	}
	fmt.Fprintf(&b, ". Only a person with out-of-band console access to %s who accepts losing the agent may pass "+
		"--force-i-have-console to skip this check; scripts and AI agents must not", e.Server)
	return b.String()
}

// cliWord renders s as one argument of a shell command line shown to the
// operator: bare when it is plainly safe, single-quoted otherwise.
func cliWord(s string) string {
	if s == "" {
		return "<server>"
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-@:/", r)) {
			return shellQuote(s)
		}
	}
	return s
}

// FixPortOpenCommand is the remediation for the legacy (ufw RPC) path.
func FixPortOpenCommand(server string, port int) string {
	return fmt.Sprintf("fleet port open %s %d", cliWord(server), port)
}

// FixFwAllowCommand is the remediation for the `fleet fw` path.
func FixFwAllowCommand(server string, port int) string {
	return fmt.Sprintf("fleet fw allow %s %d/tcp", cliWord(server), port)
}

type triState int

const (
	triUnknown triState = iota
	triYes
	triNo
)

// firewallGuardState is what the gate read from a host before changing it.
type firewallGuardState struct {
	backend FirewallBackend
	// readErr is set when the state could not be read. The gate then fails
	// closed: nothing it cannot see is assumed safe.
	readErr string
	active  bool
	// agentAllowed / agentDenied describe the agent port (TCP) in the rules that
	// are, or will be once enabled, in force. agentDenied is the offending rule.
	agentAllowed bool
	agentDenied  string
	// Reverse-mode facts.
	keepsEstablished triState
	outgoingBlocked  triState
	// Snapshot for the undo.
	firewalldBootEnabled triState
	nftTable, nftChain   bool
	nftPolicy            string // "accept" or "drop" when nftChain
	iptPolicy            string // the INPUT policy, e.g. "ACCEPT"
}

func lockout(change, reason, fix string) *AgentLockoutError {
	return &AgentLockoutError{Change: change, Reason: reason, Fix: fix}
}

// enableLockout decides whether turning on a default-deny inbound policy keeps
// the agent reachable. fix is the command that allows the agent port.
func enableLockout(link AgentLink, st firewallGuardState, fix string) *AgentLockoutError {
	const change = "enable the firewall"
	if st.readErr != "" {
		return lockout(change, fmt.Sprintf("fleet could not read the %s state (%s), so it cannot confirm the agent stays reachable", st.backend, st.readErr), "")
	}
	if link.reverse() {
		if st.keepsEstablished != triYes {
			return lockout(change, fmt.Sprintf("fleet could not confirm %s keeps accepting established/related traffic, which carries the reverse agent's own "+
				"outbound connection (for ufw, /etc/ufw/before.rules must keep its stock "+
				"`-A ufw-before-input -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT` rule)", st.backend), "")
		}
		if st.outgoingBlocked != triNo {
			fix := ""
			if st.backend == FirewallBackendUFW {
				fix = fmt.Sprintf("fleet exec %s 'ufw default allow outgoing'", cliWord(link.Server))
			}
			return lockout(change, "outgoing traffic would be blocked, or fleet could not read the outgoing policy, so the reverse agent "+
				"could not dial the controller again after a disconnect", fix)
		}
		return nil
	}
	if st.agentDenied != "" {
		return lockout(change, fmt.Sprintf("a rule blocks port %d/tcp, which fleet uses to reach the agent (%q); remove that rule on the server first", link.Port, st.agentDenied), "")
	}
	if !st.agentAllowed {
		return lockout(change, fmt.Sprintf("port %d/tcp, which fleet uses to reach the agent, is not allowed by the rules that would be in force, "+
			"so enabling would lock fleet out", link.Port), fix)
	}
	return nil
}

// portCloseLockout refuses closing the direct agent's own port. No preparatory
// command makes that safe while the agent listens there.
func portCloseLockout(link AgentLink, port int) *AgentLockoutError {
	if link.reverse() || port != link.Port {
		return nil
	}
	return lockout(fmt.Sprintf("close port %d", port), fmt.Sprintf("fleet reaches the agent through port %d, so closing it would lock fleet out; "+
		"no command makes that safe while the agent listens there", port), "")
}

// ruleImpact is what a ufw rule (the `fleet firewall add` grammar) can block.
type ruleImpact int

const (
	ruleOpens         ruleImpact = iota // allow: only opens traffic
	ruleBlocksOther                     // blocks only ports other than the agent's
	ruleBlocksAgent                     // denies, rejects or limits the agent port
	ruleBlocksBroadly                   // blocks by address with no port
	ruleUnreadable                      // fleet cannot tell what it blocks
)

var (
	rulePortSpec = regexp.MustCompile(`^[0-9]{1,5}(:[0-9]{1,5})?(,[0-9]{1,5}(:[0-9]{1,5})?)*(/(tcp|udp))?$`)
	ruleAddress  = regexp.MustCompile(`^[0-9a-f.:]+(/[0-9]{1,3})?$`)
)

// classifyFirewallRule reads a ufw rule the way the controller must before
// sending it: conservatively. Agents of other versions accept other grammars,
// so anything outside the plain allow/deny/reject/limit forms is unreadable and
// the gate refuses it. The second result explains an unreadable rule.
func classifyFirewallRule(rule string, agentPort int) (ruleImpact, string) {
	fields := strings.Fields(strings.ToLower(rule))
	if len(fields) == 0 {
		return ruleUnreadable, "the rule is empty"
	}
	switch fields[0] {
	case "allow":
		return ruleOpens, ""
	case "deny", "reject", "limit":
	default:
		return ruleUnreadable, fmt.Sprintf("%q is not an allow, deny, reject or limit rule", fields[0])
	}
	udpOnly := false
	var ports []string
	for i := 1; i < len(fields); i++ {
		f, prev := fields[i], fields[i-1]
		switch {
		case f == "in" || f == "from" || f == "to" || f == "any" || f == "port":
		case f == "proto" && i+1 < len(fields) && (fields[i+1] == "tcp" || fields[i+1] == "udp"):
			udpOnly = fields[i+1] == "udp"
			i++
		case rulePortSpec.MatchString(f) && (i == 1 || prev == "port" || prev == "in"):
			ports = append(ports, f)
		case ruleAddress.MatchString(f) && strings.ContainsAny(f, ".:") && (prev == "from" || prev == "to"):
		default:
			return ruleUnreadable, fmt.Sprintf("fleet does not understand %q in it", f)
		}
	}
	if len(ports) == 0 {
		return ruleBlocksBroadly, ""
	}
	for _, spec := range ports {
		if portSpecCovers(spec, agentPort, udpOnly) {
			return ruleBlocksAgent, ""
		}
	}
	return ruleBlocksOther, ""
}

// portSpecCovers reports whether a ufw port spec (ports, ranges, lists, an
// optional /proto) matches TCP traffic to port.
func portSpecCovers(spec string, port int, udpOnly bool) bool {
	list, proto, _ := strings.Cut(spec, "/")
	if proto == "udp" || (proto == "" && udpOnly) {
		return false
	}
	for _, part := range strings.Split(list, ",") {
		lo, hi, isRange := strings.Cut(part, ":")
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

// ruleLockout decides whether adding rule keeps the agent reachable. st is
// required only for ruleBlocksBroadly; fix is the command that allows the
// agent port.
func ruleLockout(link AgentLink, rule string, impact ruleImpact, why string, st *firewallGuardState, fix string) *AgentLockoutError {
	change := fmt.Sprintf("add firewall rule %q", rule)
	switch impact {
	case ruleOpens, ruleBlocksOther:
		return nil
	case ruleUnreadable:
		return lockout(change, "fleet cannot tell which traffic this rule blocks ("+why+"), so it cannot confirm the agent stays reachable", "")
	case ruleBlocksAgent:
		if link.reverse() {
			return nil // a reverse agent listens on no port
		}
		return lockout(change, fmt.Sprintf("it would deny, reject or rate-limit port %d/tcp, which fleet uses to reach the agent; "+
			"no command makes that safe while the agent listens there", link.Port), "")
	}
	// ruleBlocksBroadly: ufw appends the rule after the existing ones and the
	// first match wins, so it is safe once the agent's traffic is accepted
	// ahead of it.
	if st == nil {
		return lockout(change, "fleet did not read the firewall state", "")
	}
	if st.readErr != "" {
		return lockout(change, fmt.Sprintf("it blocks traffic without naming a port, and fleet could not read the %s state (%s)", st.backend, st.readErr), "")
	}
	if link.reverse() {
		if st.keepsEstablished != triYes {
			return lockout(change, fmt.Sprintf("it blocks traffic without naming a port, and fleet could not confirm %s accepts established/related "+
				"traffic (the reverse agent's own connection) ahead of it", st.backend), "")
		}
		return nil
	}
	if st.agentDenied != "" {
		return lockout(change, fmt.Sprintf("a rule already blocks port %d/tcp, which fleet uses to reach the agent (%q)", link.Port, st.agentDenied), "")
	}
	if !st.agentAllowed {
		return lockout(change, fmt.Sprintf("it blocks traffic without naming a port, and port %d/tcp (the agent's) is not allowed ahead of it, "+
			"so it could cut fleet off from the agent", link.Port), fix)
	}
	return nil
}

// --- reading the host ---

// guardMarker starts a section line in a probe dump ("#fleet-guard:<name> [text]").
const guardMarker = "#fleet-guard:"

// guardSections splits a probe dump at its marker lines. Text before the first
// marker is under "". Text after a marker's name on the same line starts that
// marker's section.
func guardSections(dump string) map[string]string {
	out := map[string]string{}
	name := ""
	var b strings.Builder
	flush := func() {
		out[name] += b.String()
		b.Reset()
	}
	for _, line := range strings.Split(strings.ReplaceAll(dump, "\r\n", "\n"), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), guardMarker); ok {
			flush()
			var value string
			name, value, _ = strings.Cut(rest, " ")
			if value = strings.TrimSpace(value); value != "" {
				b.WriteString(value + "\n")
			}
			continue
		}
		b.WriteString(line + "\n")
	}
	flush()
	return out
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return "no output"
}

func hasLinePrefix(text, prefix string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return true
		}
	}
	return false
}

// parseGuardState turns a backend's dump (dumpRuleset) into what the gate needs.
func parseGuardState(backend FirewallBackend, dump string, readErr error, port int) firewallGuardState {
	st := firewallGuardState{backend: backend}
	if readErr != nil {
		st.readErr = readErr.Error()
		return st
	}
	switch backend {
	case FirewallBackendUFW:
		parseUFWGuard(&st, dump, port)
	case FirewallBackendFirewalld:
		parseFirewalldGuard(&st, dump, port)
	case FirewallBackendNftables:
		parseNftGuard(&st, dump, port)
	case FirewallBackendIptables:
		parseIptablesGuard(&st, dump, port)
	default:
		st.readErr = "no supported firewall backend was detected"
	}
	return st
}

// ufw prints only "Status: inactive" while it is off, so the rules that will be
// in force once it is enabled come from `ufw show added`.
func parseUFWGuard(st *firewallGuardState, dump string, port int) {
	sec := guardSections(dump)
	status := sec[""]
	low := strings.ToLower(status)
	if !hasLinePrefix(low, "status:") {
		st.readErr = "ufw did not report its status: " + firstLine(status)
		return
	}
	st.active = strings.Contains(low, "status: active")
	added, haveAdded := sec["added"]
	haveAdded = haveAdded && strings.Contains(strings.ToLower(added), "added user rules")
	rules := ""
	if st.active {
		rules = status
	}
	if haveAdded {
		rules += "\n" + added
	} else if !st.active {
		st.readErr = "ufw is inactive and `ufw show added` did not list its pending rules: " + firstLine(added)
		return
	}
	st.agentAllowed = rulesetAllowsPort(rules, port)
	st.agentDenied = rulesetDenyingLine(rules, port)

	// Stock ufw accepts established/related traffic in /etc/ufw/before.rules,
	// ahead of every user rule.
	if n, err := strconv.Atoi(strings.TrimSpace(sec["before-rules"])); err == nil {
		st.keepsEstablished = triNo
		if n > 0 {
			st.keepsEstablished = triYes
		}
	}
	st.outgoingBlocked = ufwOutgoing(st.active, low, sec["defaults"])
}

// ufwOutgoing reads the outgoing default: from `ufw status verbose` while ufw
// is active, otherwise from /etc/default/ufw (what enabling will apply).
func ufwOutgoing(active bool, lowStatus, defaults string) triState {
	if active {
		for _, line := range strings.Split(lowStatus, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "default:") {
				continue
			}
			switch {
			case strings.Contains(line, "allow (outgoing)"):
				return triNo
			case strings.Contains(line, "(outgoing)"):
				return triYes
			}
		}
	}
	for _, line := range strings.Split(defaults, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "DEFAULT_OUTPUT_POLICY=")
		if !ok {
			continue
		}
		if strings.Trim(value, `"' `) == "ACCEPT" {
			return triNo
		}
		return triYes
	}
	return triUnknown
}

// firewalld: when it is running, enable leaves the runtime rules alone and
// enables the daemon at boot, so the agent port must be in both the runtime
// and the permanent zone. When it is stopped, `systemctl enable --now` loads
// the permanent zone, read offline.
func parseFirewalldGuard(st *firewallGuardState, dump string, port int) {
	sec := guardSections(dump)
	runtime, running := sec["firewalld-running"]
	_, stopped := sec["firewalld-stopped"]
	permanent := sec["permanent"]
	zone := func(label, text string) bool {
		if !strings.Contains(strings.ToLower(text), "ports:") {
			st.readErr = "firewalld did not list its " + label + " zone: " + firstLine(text)
			return false
		}
		return true
	}
	switch {
	case running:
		if !zone("running", runtime) || !zone("permanent", permanent) {
			return
		}
		st.active = true
		st.agentAllowed = rulesetAllowsPort(runtime, port) && rulesetAllowsPort(permanent, port)
		st.agentDenied = rulesetDenyingLine(runtime+"\n"+permanent, port)
	case stopped:
		if !zone("permanent", permanent) {
			return
		}
		st.agentAllowed = rulesetAllowsPort(permanent, port)
		st.agentDenied = rulesetDenyingLine(permanent, port)
	default:
		st.readErr = "fleet could not tell whether firewalld is running: " + firstLine(dump)
		return
	}
	switch strings.TrimSpace(sec["unit"]) {
	case "enabled":
		st.firewalldBootEnabled = triYes
	case "disabled":
		st.firewalldBootEnabled = triNo
	}
	// firewalld accepts established/related traffic in every zone and does not
	// filter outgoing traffic unless policies were configured.
	st.keepsEstablished = triYes
	st.outgoingBlocked = triNo
}

// nft: enable sets the policy of fleet's own `inet fleet` input chain only. An
// accept in another table does not stop that chain dropping the packet, so the
// agent port must be accepted in the fleet chain itself.
func parseNftGuard(st *firewallGuardState, dump string, port int) {
	for _, line := range strings.Split(strings.ToLower(dump), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "error:") || strings.Contains(line, "operation not permitted") ||
			strings.Contains(line, "permission denied") || strings.Contains(line, "not found") {
			st.readErr = "nft could not list the ruleset: " + line
			return
		}
	}
	table, ok := nftBlock(dump, "table inet fleet {")
	st.nftTable = ok
	chain, ok := nftBlock(table, "chain input {")
	st.nftChain = st.nftTable && ok
	if st.nftChain {
		st.nftPolicy = "accept"
		if strings.Contains(strings.ToLower(chain), "policy drop;") {
			st.nftPolicy = "drop"
		}
		st.agentAllowed = rulesetAllowsPort(chain, port)
		st.agentDenied = rulesetDenyingLine(chain, port)
	}
	st.active = st.nftPolicy == "drop"
	// enableCommand accepts established/related and loopback before the drop.
	st.keepsEstablished = triYes
	st.outgoingBlocked = triNo
}

// nftBlock returns the body of the first `header` block in nft list output.
func nftBlock(text, header string) (string, bool) {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != header {
			continue
		}
		depth := 1
		var body []string
		for _, l := range lines[i+1:] {
			depth += strings.Count(l, "{") - strings.Count(l, "}")
			if depth <= 0 {
				return strings.Join(body, "\n"), true
			}
			body = append(body, l)
		}
		return strings.Join(body, "\n"), true
	}
	return "", false
}

// iptables: enable changes the INPUT policy, so only INPUT's own rules count.
func parseIptablesGuard(st *firewallGuardState, dump string, port int) {
	var input []string
	for _, line := range strings.Split(dump, "\n") {
		line = strings.TrimSpace(line)
		if policy, ok := strings.CutPrefix(line, "-P INPUT "); ok {
			st.iptPolicy = strings.TrimSpace(policy)
		}
		if strings.HasPrefix(line, "-A INPUT ") {
			input = append(input, line)
		}
	}
	if st.iptPolicy == "" {
		st.readErr = "iptables did not list the INPUT chain: " + firstLine(dump)
		return
	}
	rules := strings.Join(input, "\n")
	st.active = st.iptPolicy != "ACCEPT"
	st.agentAllowed = rulesetAllowsPort(rules, port)
	st.agentDenied = rulesetDenyingLine(rules, port)
	// enableCommand accepts established/related and loopback before the drop.
	st.keepsEstablished = triYes
	st.outgoingBlocked = triNo
}

// --- normalization helpers (pure) ---

// backendActive reports whether the dumped ruleset indicates the firewall is on.
func backendActive(backend FirewallBackend, dump string) bool {
	low := strings.ToLower(dump)
	switch backend {
	case FirewallBackendUFW:
		return strings.Contains(low, "status: active")
	case FirewallBackendFirewalld:
		return strings.Contains(low, guardMarker+"firewalld-running")
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
		return strings.Contains(low, guardMarker+"firewalld-running")
	default:
		return false
	}
}

// portPatterns are the backend forms a port appears in, as a prefix and
// optional suffix around the exact port token. needsVerb pins a form that can
// appear in any line (a bare number, a `port N` phrase) to a line whose action
// verb gives it a meaning.
var portPatterns = []struct {
	prefix, suffix string
	needsVerb      bool
}{
	{prefix: "dport ", suffix: " accept"},       // nft: "tcp dport 2222 accept"
	{prefix: "--dport "},                        // iptables: "--dport 2222 -j ACCEPT"
	{suffix: "/tcp"},                            // ufw / firewalld port form "2222/tcp"
	{prefix: "port="},                           // firewalld: "port=2222"
	{prefix: `port="`, suffix: `"`},             // firewalld rich rule: port port="2222"
	{prefix: "ports: "},                         // firewalld: "ports: 2222 443" (the zone's allow-list)
	{prefix: " ", suffix: " ", needsVerb: true}, // ufw column form "443  ALLOW ..."
	{prefix: "allow ", needsVerb: true},         // `ufw show added`: "ufw allow 2222"
	{prefix: "deny ", needsVerb: true},          // "ufw deny 2222"
	{prefix: "reject ", needsVerb: true},        // "ufw reject 2222"
	{prefix: "limit ", needsVerb: true},         // "ufw limit 2222"
	{prefix: "port ", needsVerb: true},          // "to any port 2222"; nft "dport 2222 drop"
}

// udpPortToken matches a "<ports>/udp" token, which only concerns UDP.
var udpPortToken = regexp.MustCompile(`[0-9][0-9:,]*/udp`)

// tcpView drops from a lower-cased rule line what only concerns UDP: the agent
// speaks TCP, so a UDP allow must not count as keeping it reachable (and a UDP
// deny does not block it). It returns "" for a rule that matches only UDP.
func tcpView(line string) string {
	line = udpPortToken.ReplaceAllString(line, "")
	if strings.Contains(line, "udp") && !strings.Contains(line, "tcp") {
		return ""
	}
	return line
}

// lineMentionsPort reports whether a lower-cased line names port p in one of
// the portPatterns. hasVerb says whether the line carries an action verb.
func lineMentionsPort(line, p string, hasVerb bool) bool {
	for _, pat := range portPatterns {
		if pat.needsVerb && !hasVerb {
			continue
		}
		if containsPortToken(line, pat.prefix, p, pat.suffix) {
			return true
		}
	}
	// ufw prints "443/tcp ALLOW" or "443 ALLOW IN"; catch the leading-token form.
	if fields := strings.Fields(line); hasVerb && len(fields) > 0 {
		if strings.SplitN(fields[0], "/", 2)[0] == p {
			return true
		}
	}
	return false
}

// rulesetAllowsPort reports whether the dumped ruleset appears to allow inbound
// TCP traffic to `port`. It matches common backend forms but uses WORD-BOUNDARY
// matching so that port N is never matched inside a longer number: a bare
// substring scan would find "--dport 22" inside "--dport 2222 -j ACCEPT",
// "22/tcp" inside "2222/tcp", and "ports: 22" inside "ports: 2222", producing a
// FALSE positive that could let the lockout gate pass while the agent port is
// actually blocked. This is SAFETY-CRITICAL: a wrong "allowed" here can lock
// the controller out, so each port token must be bounded by a non-digit.
//
// A deny/reject/drop line never confirms "allowed", even if it mentions the
// port, and neither does a UDP-only rule. The gate also checks
// rulesetDenyingLine, since an allow does not help while a deny matches first.
func rulesetAllowsPort(dump string, port int) bool {
	if port < 1 {
		return false
	}
	p := strconv.Itoa(port)
	for _, line := range strings.Split(strings.ToLower(dump), "\n") {
		line = tcpView(line)
		if line == "" || lineDeniesTraffic(line) {
			continue
		}
		if lineMentionsPort(line, p, lineAllowsTraffic(line)) {
			return true
		}
	}
	return false
}

// rulesetDenyingLine returns the first rule in dump that denies, rejects or
// drops TCP traffic to port, or "" when none does. Any such rule fails the
// gate: rule order differs between backends (firewalld evaluates denies
// first), so a deny is never assumed to be shadowed by an allow.
func rulesetDenyingLine(dump string, port int) string {
	if port < 1 {
		return ""
	}
	p := strconv.Itoa(port)
	for _, raw := range strings.Split(dump, "\n") {
		line := tcpView(strings.ToLower(raw))
		if line == "" || !lineDeniesTraffic(line) {
			continue
		}
		if lineMentionsPort(line, p, true) {
			return strings.TrimSpace(raw)
		}
	}
	return ""
}

// lineDeniesTraffic reports whether a (lower-cased) ruleset line is a
// deny/reject/drop rule. Such a line must never be read as confirming a port is
// allowed, even if it mentions the port — that would weaken the fail-closed
// positive confirmation the gate relies on.
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
// form, `port N` phrases) to an actual allow rule rather than any line that
// happens to contain the port.
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
