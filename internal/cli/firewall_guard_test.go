// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
)

// cliFirewallHost is an in-process agent for the firewall CLI tests. Reverse
// servers reach it through ReverseRPCContext; direct servers cannot be dialled
// at all (each attempt is counted), so a refusal that happens before any RPC
// is visible as zero dials.
type cliFirewallHost struct {
	mu      sync.Mutex
	probe   string // what fleet's ufw probe prints
	armed   string // what the self-revert schedule command prints
	execs   []string
	actions []string
	dials   int
}

func (h *cliFirewallHost) call(_ context.Context, _ string, env proto.Envelope) (proto.Envelope, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch env.Action {
	case "shell.exec":
		p, _ := proto.DecodePayload[proto.ExecPayload](env.Payload)
		h.execs = append(h.execs, p.Command)
		out := ""
		switch {
		case strings.Contains(p.Command, "command -v 'ufw'"):
			out = "yes"
		case strings.HasPrefix(p.Command, "command -v"):
			out = "no"
		case strings.Contains(p.Command, "ufw status verbose"):
			out = h.probe
		case strings.Contains(p.Command, "systemd-run"):
			out = h.armed
		}
		return proto.Envelope{Payload: proto.ExecResult{Stdout: out}}, nil
	case "firewall.status":
	case "firewall.enable":
		h.actions = append(h.actions, "enable:true")
	case "firewall.add_rule":
		r, _ := proto.DecodePayload[proto.FirewallRulePayload](env.Payload)
		h.actions = append(h.actions, "rule:"+r.Rule)
	case "port.set":
		p, _ := proto.DecodePayload[proto.PortActionPayload](env.Payload)
		h.actions = append(h.actions, fmt.Sprintf("port:%d:%t", p.Port, p.Open))
	default:
		return proto.Envelope{Error: &proto.Error{Code: "unexpected", Message: env.Action}}, nil
	}
	return proto.Envelope{Payload: proto.FirewallInfo{}}, nil
}

func (h *cliFirewallHost) ranExec(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.execs {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func (h *cliFirewallHost) did(action string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Contains(h.actions, action)
}

func cliUFWProbe(beforeRulesCount, outputPolicy string) string {
	return "Status: inactive\n#fleet-guard:added\nAdded user rules (see 'ufw status' for running firewall):\n(None)\n" +
		"#fleet-guard:defaults\nDEFAULT_INPUT_POLICY=\"DROP\"\nDEFAULT_OUTPUT_POLICY=\"" + outputPolicy + "\"\n" +
		"#fleet-guard:before-rules\n" + beforeRulesCount + "\n"
}

// cliUFWActiveProbe is an active stock ufw that already allows 2222/tcp.
func cliUFWActiveProbe() string {
	return "Status: active\nDefault: deny (incoming), allow (outgoing), disabled (routed)\n\n" +
		"To                         Action      From\n--                         ------      ----\n" +
		"2222/tcp                   ALLOW IN    Anywhere\n" +
		"#fleet-guard:added\nAdded user rules (see 'ufw status' for running firewall):\nufw allow 2222/tcp\n" +
		"#fleet-guard:defaults\nDEFAULT_INPUT_POLICY=\"DROP\"\nDEFAULT_OUTPUT_POLICY=\"ACCEPT\"\n" +
		"#fleet-guard:before-rules\n1\n"
}

// setupFirewallCLI creates server "web" (agent port 2222) in the given mode.
func setupFirewallCLI(t *testing.T, mode transport.Mode) (string, *cliFirewallHost) {
	t.Helper()
	t.Setenv("FLEET_TOKEN", "")
	dir := newServerCommandTestConfig(t)
	app, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.AddServer(core.ServerRecord{Name: "web", Address: "127.0.0.1", Port: 2222, Mode: mode}); err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	host := &cliFirewallHost{probe: cliUFWProbe("1", "ACCEPT"), armed: "fleet-fw-safe-revert:armed:systemd-run"}
	openAppHook = func(a *core.App) {
		a.ReverseRPCContext = host.call
		a.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
			host.mu.Lock()
			host.dials++
			host.mu.Unlock()
			return nil, errors.New("test: no agent listening for web")
		}
	}
	t.Cleanup(func() { openAppHook = nil })
	return dir, host
}

func cliAudit(t *testing.T, dir, action string) []logs.AuditEntry {
	t.Helper()
	entries, err := logs.NewAuditLog(filepath.Join(dir, "logs", "_audit.log")).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var out []logs.AuditEntry
	for _, e := range entries {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func requireCLIRefusal(t *testing.T, r fleetRun, mustMention ...string) {
	t.Helper()
	if r.err == nil {
		t.Fatalf("expected a lockout refusal; stdout=%q stderr=%q", r.stdout, r.stderr)
	}
	msg := r.err.Error()
	for _, want := range append([]string{"refusing to", "--force-i-have-console"}, mustMention...) {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal should mention %q; got: %v", want, r.err)
		}
	}
}

func requireAudit(t *testing.T, dir, action, detail string) {
	t.Helper()
	for _, e := range cliAudit(t, dir, action) {
		if e.Target == "web" && e.Operator != "" && strings.Contains(e.Details, detail) {
			return
		}
	}
	t.Errorf("missing audit %s for web with details ~%q; have %+v", action, detail, cliAudit(t, dir, action))
}

func TestFirewallEnableRefusalAndOverrideCLI(t *testing.T) {
	dir, host := setupFirewallCLI(t, transport.ModeReverse)
	host.probe = cliUFWProbe("0", "ACCEPT") // before.rules lost its established/related accept

	requireCLIRefusal(t, runExecFleet(t, dir, "firewall", "enable", "web"), "established")
	if host.did("enable:true") {
		t.Fatal("refused enable reached the agent")
	}
	requireAudit(t, dir, "firewall.lockout.refused", "op=firewall.enable")

	r := runExecFleet(t, dir, "firewall", "enable", "web", "--force-i-have-console")
	if r.err != nil {
		t.Fatalf("enable with the override: %v", r.err)
	}
	if !host.did("enable:true") {
		t.Fatal("the override did not enable the firewall")
	}
	requireAudit(t, dir, "firewall.lockout.override", "op=firewall.enable")
}

func TestFirewallAddRefusalAndOverrideCLI(t *testing.T) {
	dir, host := setupFirewallCLI(t, transport.ModeReverse)

	requireCLIRefusal(t, runExecFleet(t, dir, "firewall", "add", "web", "reset"))
	if host.did("rule:reset") {
		t.Fatal("refused rule reached the agent")
	}
	requireAudit(t, dir, "firewall.lockout.refused", "op=firewall.rule.add")

	if r := runExecFleet(t, dir, "firewall", "add", "web", "reset", "--force-i-have-console"); r.err != nil {
		t.Fatalf("add with the override: %v", r.err)
	}
	if !host.did("rule:reset") {
		t.Fatal("the override did not send the rule")
	}
	requireAudit(t, dir, "firewall.lockout.override", "op=firewall.rule.add")
}

func TestPortCloseAgentPortRefusalAndOverrideCLI(t *testing.T) {
	dir, host := setupFirewallCLI(t, transport.ModeDirect)

	requireCLIRefusal(t, runExecFleet(t, dir, "port", "close", "web", "2222"), "2222")
	if host.dials != 0 {
		t.Fatalf("the refusal must happen before any connection; dials=%d", host.dials)
	}
	requireAudit(t, dir, "firewall.lockout.refused", "op=port.close")

	r := runExecFleet(t, dir, "port", "close", "web", "2222", "--force-i-have-console")
	if r.err == nil || !strings.Contains(r.err.Error(), "no agent listening") {
		t.Fatalf("with the override the close must go to the agent (which this test cannot reach); got %v", r.err)
	}
	requireAudit(t, dir, "firewall.lockout.override", "op=port.close")
}

// fw enable --safe prints either a human summary or JSON, never both, and the
// summary says what really happens: the timer fires unless cancelled.
func TestFwEnableHumanOutputIsTrueCLI(t *testing.T) {
	dir, host := setupFirewallCLI(t, transport.ModeReverse)
	host.probe = cliUFWActiveProbe()

	r := runExecFleet(t, dir, "fw", "enable", "web", "--safe")
	if r.err != nil {
		t.Fatalf("fw enable --safe: %v (stderr=%q)", r.err, r.stderr)
	}
	if strings.Contains(r.stdout, "{") {
		t.Errorf("human output must not include JSON:\n%s", r.stdout)
	}
	if strings.Contains(r.stdout, "check back in") {
		t.Errorf("nothing checks in or cancels the timer automatically:\n%s", r.stdout)
	}
	for _, want := range []string{"unless you cancel it", "Nothing cancels it automatically", "fleet exec web 'systemctl stop fleet-fw-safe-revert-"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("human output missing %q:\n%s", want, r.stdout)
		}
	}
	// Reverse mode: the agent dials out, so no inbound allow is injected.
	if host.ranExec("ufw allow") {
		t.Errorf("reverse mode must not open an inbound agent port; execs=%q", host.execs)
	}
	requireAudit(t, dir, "firewall.enable", "fw --safe")
}

func TestFwEnableJSONOutputCLI(t *testing.T) {
	dir, _ := setupFirewallCLI(t, transport.ModeReverse)
	r := runExecFleet(t, dir, "fw", "enable", "web", "--safe", "--json")
	if r.err != nil {
		t.Fatalf("fw enable --safe --json: %v", r.err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("--json stdout must be one JSON document: %v\n%s", err, r.stdout)
	}
	if got["revert_method"] != "systemd-run" || got["undo_scheduled"] != true {
		t.Errorf("JSON should report the armed revert; got %v", got)
	}
}

func TestFwEnableSetsidCancelInstructionCLI(t *testing.T) {
	dir, host := setupFirewallCLI(t, transport.ModeReverse)
	host.probe = cliUFWActiveProbe()
	host.armed = "fleet-fw-safe-revert:armed:setsid"
	r := runExecFleet(t, dir, "fw", "enable", "web", "--safe")
	if r.err != nil {
		t.Fatalf("fw enable --safe: %v", r.err)
	}
	if strings.Contains(r.stdout, "systemctl stop") {
		t.Errorf("the setsid path has no systemd unit to stop:\n%s", r.stdout)
	}
	if !strings.Contains(r.stdout, `fleet exec web 'pkill -f "[f]leet-fw-safe-revert-`) {
		t.Errorf("the setsid path must say how to kill the pending revert:\n%s", r.stdout)
	}
}

func TestFwEnableRefusalAndOverrideAuditedCLI(t *testing.T) {
	dir, host := setupFirewallCLI(t, transport.ModeReverse)
	host.probe = cliUFWProbe("0", "ACCEPT")

	requireCLIRefusal(t, runExecFleet(t, dir, "fw", "enable", "web", "--safe"), "established")
	if host.ranExec("ufw --force enable") {
		t.Fatal("refused fw enable changed the firewall")
	}
	requireAudit(t, dir, "firewall.lockout.refused", "op=fw.enable")

	if r := runExecFleet(t, dir, "fw", "enable", "web", "--safe", "--force-i-have-console"); r.err != nil {
		t.Fatalf("fw enable with the override: %v", r.err)
	}
	requireAudit(t, dir, "firewall.lockout.override", "op=fw.enable")
}
