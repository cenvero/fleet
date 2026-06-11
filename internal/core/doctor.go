// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// FL-003 — doctor.
//
// `fleet doctor <server>` runs a fixed set of health checks against a managed
// server over the agent channel (App.ExecCommand) and reports each as a
// pass/warn/fail line. The engine logic lives here and is driven by a tiny exec
// interface so it can be unit-tested with a fake exec function — it does not
// hard-depend on *App.

// DoctorStatus is the outcome of a single check.
type DoctorStatus string

const (
	DoctorOK   DoctorStatus = "ok"
	DoctorWarn DoctorStatus = "warn"
	DoctorFail DoctorStatus = "fail"
)

// DoctorCheck is a single line of the checklist.
type DoctorCheck struct {
	Name   string       `json:"name"`
	Status DoctorStatus `json:"status"`
	Detail string       `json:"detail,omitempty"`
}

// DoctorReport is the full result for one server.
type DoctorReport struct {
	Server string        `json:"server"`
	Checks []DoctorCheck `json:"checks"`
}

// OK reports whether every check passed (no warn, no fail).
func (r DoctorReport) OK() bool {
	for _, c := range r.Checks {
		if c.Status != DoctorOK {
			return false
		}
	}
	return true
}

// Failed reports whether any check is a hard failure.
func (r DoctorReport) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == DoctorFail {
			return true
		}
	}
	return false
}

// doctorExec is the minimal exec surface the doctor engine needs. It returns the
// same shape as proto.ExecResult (mirrored by ExecResultLike, defined in
// inventory.go) so the CLI can adapt *App without this file importing pkg/proto,
// and tests can supply a fake.
type doctorExec func(command string) (ExecResultLike, error)

// DoctorProbe carries the inputs the engine needs beyond the exec function:
// the server name (for the report) and the agent SSH port (from ServerRecord)
// used by the reachability check and clock-skew baseline.
type DoctorProbe struct {
	Server    string
	AgentPort int
	// Now is the controller's reference time for the clock-skew check. When
	// zero, time.Now() is used. Tests set it for determinism.
	Now time.Time
}

// RunDoctor executes the full checklist using exec and returns a report. The
// order is fixed so output is stable. A nil exec function fails every remote
// check rather than panicking.
func RunDoctor(probe DoctorProbe, exec doctorExec) DoctorReport {
	now := probe.Now
	if now.IsZero() {
		now = time.Now()
	}
	report := DoctorReport{Server: probe.Server}
	add := func(c DoctorCheck) { report.Checks = append(report.Checks, c) }

	// 1. Agent online — a trivial exec must succeed with exit 0. If this fails,
	// the agent channel is down and the remaining remote checks are unreliable,
	// so we still run them but they will naturally report fail/warn.
	online := false
	if exec != nil {
		res, err := exec("echo fleet-doctor-ok")
		switch {
		case err != nil:
			add(DoctorCheck{Name: "agent online", Status: DoctorFail, Detail: err.Error()})
		case res.ExitCode != 0:
			add(DoctorCheck{Name: "agent online", Status: DoctorFail, Detail: fmt.Sprintf("trivial command exited %d", res.ExitCode)})
		case strings.TrimSpace(res.Stdout) != "fleet-doctor-ok":
			// An unexpected response means the channel is answering but not with
			// what we sent — it is NOT reliable-online, so leave online false.
			add(DoctorCheck{Name: "agent online", Status: DoctorWarn, Detail: "unexpected probe output"})
		default:
			add(DoctorCheck{Name: "agent online", Status: DoctorOK})
			online = true
		}
	} else {
		add(DoctorCheck{Name: "agent online", Status: DoctorFail, Detail: "no exec channel"})
	}

	// 2. Agent port reachable — verify something is listening on the recorded
	// agent SSH port from the server's side (loopback). We test the port from
	// the remote host so a controller-side firewall doesn't taint the result.
	add(checkAgentPort(probe.AgentPort, exec))

	// 3. sshd reachable — a listener on port 22 (or sshd active).
	add(checkSSHD(exec))

	// 4. Disk usage — warn when the root filesystem is >90% used.
	add(checkDisk(exec))

	// 5. Swap — warn when no swap is configured.
	add(checkSwap(exec))

	// 6. Reboot required.
	add(checkRebootRequired(exec))

	// 7. Clock skew between the remote host and the controller.
	add(checkClockSkew(now, exec))

	// When the agent did not answer the trivial probe reliably, the remote
	// checks above (ports, disk, swap, reboot, clock) may be based on a flaky or
	// wrong channel. Annotate them so the operator does not over-trust the
	// results, without changing the fixed checklist shape.
	if !online {
		for i := range report.Checks {
			if report.Checks[i].Name == "agent online" {
				continue
			}
			if report.Checks[i].Detail == "" {
				report.Checks[i].Detail = "agent not reliably online — result may be unreliable"
			} else {
				report.Checks[i].Detail += " (agent not reliably online)"
			}
		}
	}
	return report
}

// runExec is a nil-safe helper that funnels every remote check through one path.
func runExec(exec doctorExec, command string) (ExecResultLike, error) {
	if exec == nil {
		return ExecResultLike{}, fmt.Errorf("no exec channel")
	}
	return exec(command)
}

func checkAgentPort(port int, exec doctorExec) DoctorCheck {
	const name = "agent port reachable"
	if port <= 0 {
		port = 2222
	}
	// Bash /dev/tcp is widely available and needs no extra tooling; fall back to
	// nc when bash is absent. We probe loopback from the agent host itself.
	p := strconv.Itoa(port)
	cmd := "if (echo > /dev/tcp/127.0.0.1/" + shellQuote(p) + ") >/dev/null 2>&1; then echo open; " +
		"elif command -v nc >/dev/null 2>&1 && nc -z 127.0.0.1 " + shellQuote(p) + " >/dev/null 2>&1; then echo open; " +
		"else echo closed; fi"
	res, err := runExec(exec, cmd)
	if err != nil {
		return DoctorCheck{Name: name, Status: DoctorWarn, Detail: err.Error()}
	}
	if strings.TrimSpace(res.Stdout) == "open" {
		return DoctorCheck{Name: name, Status: DoctorOK, Detail: "port " + p}
	}
	return DoctorCheck{Name: name, Status: DoctorWarn, Detail: "nothing listening on port " + p}
}

func checkSSHD(exec doctorExec) DoctorCheck {
	const name = "sshd reachable"
	cmd := "if (echo > /dev/tcp/127.0.0.1/22) >/dev/null 2>&1; then echo open; " +
		"elif command -v ss >/dev/null 2>&1 && ss -tlnH 2>/dev/null | grep -q ':22 '; then echo open; " +
		"elif command -v systemctl >/dev/null 2>&1 && systemctl is-active ssh sshd >/dev/null 2>&1; then echo open; " +
		"else echo closed; fi"
	res, err := runExec(exec, cmd)
	if err != nil {
		return DoctorCheck{Name: name, Status: DoctorWarn, Detail: err.Error()}
	}
	if strings.TrimSpace(res.Stdout) == "open" {
		return DoctorCheck{Name: name, Status: DoctorOK}
	}
	return DoctorCheck{Name: name, Status: DoctorFail, Detail: "no sshd listener on port 22"}
}

func checkDisk(exec doctorExec) DoctorCheck {
	const name = "disk usage"
	// df -P / gives portable output; the 5th field of the data row is "NN%".
	res, err := runExec(exec, "df -P / 2>/dev/null | awk 'NR==2{print $5}'")
	if err != nil {
		return DoctorCheck{Name: name, Status: DoctorWarn, Detail: err.Error()}
	}
	pctStr := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(res.Stdout), "%"))
	if pctStr == "" {
		return DoctorCheck{Name: name, Status: DoctorWarn, Detail: "could not read df output"}
	}
	pct, perr := strconv.Atoi(pctStr)
	if perr != nil {
		return DoctorCheck{Name: name, Status: DoctorWarn, Detail: "unparseable df output: " + pctStr}
	}
	detail := fmt.Sprintf("root filesystem %d%% used", pct)
	if pct > 90 {
		return DoctorCheck{Name: name, Status: DoctorWarn, Detail: detail}
	}
	return DoctorCheck{Name: name, Status: DoctorOK, Detail: detail}
}

func checkSwap(exec doctorExec) DoctorCheck {
	const name = "swap configured"
	// free outputs a "Swap:" row whose 2nd field is total swap. 0 => no swap.
	res, err := runExec(exec, "free 2>/dev/null | awk '/^Swap:/{print $2}'")
	if err != nil {
		return DoctorCheck{Name: name, Status: DoctorWarn, Detail: err.Error()}
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		return DoctorCheck{Name: name, Status: DoctorWarn, Detail: "could not read swap total"}
	}
	total, perr := strconv.ParseInt(out, 10, 64)
	if perr != nil {
		return DoctorCheck{Name: name, Status: DoctorWarn, Detail: "unparseable swap total: " + out}
	}
	if total == 0 {
		return DoctorCheck{Name: name, Status: DoctorWarn, Detail: "no swap configured"}
	}
	return DoctorCheck{Name: name, Status: DoctorOK, Detail: out + " KiB swap"}
}

func checkRebootRequired(exec doctorExec) DoctorCheck {
	const name = "reboot required"
	// Debian/Ubuntu drop /var/run/reboot-required; RHEL has needs-restarting -r.
	cmd := "if [ -f /var/run/reboot-required ]; then echo yes; " +
		"elif command -v needs-restarting >/dev/null 2>&1; then if needs-restarting -r >/dev/null 2>&1; then echo no; else echo yes; fi; " +
		"else echo no; fi"
	res, err := runExec(exec, cmd)
	if err != nil {
		return DoctorCheck{Name: name, Status: DoctorWarn, Detail: err.Error()}
	}
	if strings.TrimSpace(res.Stdout) == "yes" {
		return DoctorCheck{Name: name, Status: DoctorWarn, Detail: "host needs a reboot"}
	}
	return DoctorCheck{Name: name, Status: DoctorOK}
}

func checkClockSkew(now time.Time, exec doctorExec) DoctorCheck {
	const name = "clock skew"
	res, err := runExec(exec, "date +%s")
	if err != nil {
		return DoctorCheck{Name: name, Status: DoctorWarn, Detail: err.Error()}
	}
	out := strings.TrimSpace(res.Stdout)
	remoteEpoch, perr := strconv.ParseInt(out, 10, 64)
	if perr != nil {
		return DoctorCheck{Name: name, Status: DoctorWarn, Detail: "unparseable remote time: " + out}
	}
	skew := now.Unix() - remoteEpoch
	abs := skew
	if abs < 0 {
		abs = -abs
	}
	detail := fmt.Sprintf("%ds vs controller", skew)
	switch {
	case abs >= 60:
		return DoctorCheck{Name: name, Status: DoctorWarn, Detail: detail}
	default:
		return DoctorCheck{Name: name, Status: DoctorOK, Detail: detail}
	}
}

// AnalyzeCommandSafety inspects a command the operator is about to run on an
// agent host and returns human-readable warnings when it could sever the
// controller's own connectivity: dropping the agent SSH port, flushing the
// firewall to a default-deny posture, or flushing routes / the default route.
//
// It is intentionally conservative — it pattern-matches common firewall and
// routing tools (iptables/nft/firewalld/ufw and `ip route flush`) rather than
// fully parsing them. The main loop wires `fleet exec --guard` to this so the
// operator gets a confirmation prompt before a self-lockout. An empty slice
// means no concern was detected.
func AnalyzeCommandSafety(command string, agentPort int) []string {
	cmd := strings.ToLower(command)
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return nil
	}
	// norm collapses runs of whitespace to single spaces so policy detection is
	// not defeated by extra spacing (e.g. "-P  INPUT  DROP").
	norm := strings.Join(fields, " ")
	if agentPort <= 0 {
		agentPort = 2222
	}
	portStr := strconv.Itoa(agentPort)

	var warnings []string
	seen := map[string]bool{}
	warn := func(msg string) {
		if seen[msg] {
			return
		}
		seen[msg] = true
		warnings = append(warnings, msg)
	}

	mentions := func(needles ...string) bool {
		for _, n := range needles {
			if strings.Contains(cmd, n) {
				return true
			}
		}
		return false
	}

	// Default-deny / policy DROP without an explicit allow for the agent port is
	// the classic self-lockout. We flag any policy-drop or flush, then separately
	// check whether the agent port is referenced as an allow.
	allowsAgentPort := mentions(
		"dport "+portStr, "--dport "+portStr, "dport="+portStr,
		"port "+portStr, "allow "+portStr, ":"+portStr,
	) || strings.Contains(cmd, " "+portStr+" ")
	allowsSSH := mentions("dport 22", "--dport 22", "port 22", "allow 22", "ssh", "dport=22")

	// acceptsAgentPort is true when the agent port appears in an explicit ACCEPT
	// rule (e.g. "--dport 2222 -j ACCEPT"). Such a rule is the opposite of a
	// lockout, so it must not be mistaken for a DROP/REJECT of the port.
	acceptsAgentPort := allowsAgentPort && mentions(
		"-j accept", "--jump accept", "-j allow", "accept",
	) && !strings.Contains(norm, portStr+" -j drop") && !strings.Contains(norm, portStr+" -j reject")

	// iptables / ip6tables.
	if mentions("iptables", "ip6tables") {
		// Policy DROP detection tolerates extra whitespace via the normalized form.
		if mentions("-p drop", "--policy drop", "policy drop") ||
			strings.Contains(norm, "-p input drop") {
			warn("sets the default INPUT policy to DROP — this can lock out the controller if the agent port " + portStr + " is not explicitly allowed first")
		}
		if mentions("-f", "--flush") {
			warn("flushes iptables rules — combined with a DROP policy this can drop the agent port " + portStr + " and the controller connection")
		}
		// Only flag a rule that DROPs/REJECTs the agent port. An ACCEPT of the
		// agent port (even alongside a policy DROP) is not itself a drop of it.
		if mentions("drop", "reject") && allowsAgentPort && !acceptsAgentPort {
			// An explicit rule targeting the agent port is the most dangerous.
			warn("appears to DROP/REJECT traffic on the agent port " + portStr + " — the controller would lose its connection")
		}
	}

	// nftables.
	if mentions("nft ", "nftables") {
		if mentions("flush ruleset", "flush table", "flush chain") {
			warn("flushes nftables ruleset — if the resulting policy is drop and the agent port " + portStr + " is not re-allowed, the controller can be locked out")
		}
		if mentions("policy drop") && !allowsAgentPort && !allowsSSH {
			warn("sets an nftables policy of drop without allowing the agent port " + portStr + " — possible self-lockout")
		}
	}

	// firewalld.
	if mentions("firewall-cmd") {
		if mentions("--set-default-zone=drop", "--panic-on", "--set-target=drop") {
			warn("switches firewalld to a drop posture — confirm the agent port " + portStr + " stays open or the controller will be locked out")
		}
		if mentions("--remove-port") && allowsAgentPort {
			warn("removes the agent port " + portStr + " from firewalld — the controller would lose its connection")
		}
	}

	// ufw.
	if mentions("ufw") {
		if mentions("default deny", "default reject") && !allowsAgentPort && !allowsSSH {
			warn("sets ufw default to deny without allowing the agent port " + portStr + " — possible self-lockout")
		}
		if mentions("deny "+portStr, "reject "+portStr) || (mentions("delete") && allowsAgentPort) {
			warn("denies or deletes the rule for the agent port " + portStr + " in ufw — the controller would lose its connection")
		}
		if mentions("disable") {
			// Disabling ufw opens everything; not a lockout but worth surfacing
			// only when combined with nothing else risky — skip to avoid noise.
			_ = portStr
		}
	}

	// Route changes — flushing routes or deleting the default route severs
	// reachability for the controller regardless of firewall state.
	if mentions("ip route flush", "route flush", "ip route del default", "ip route delete default", "route del default") {
		warn("flushes or deletes the default route — the controller will lose its connection to this host until the route is restored")
	}

	return warnings
}
