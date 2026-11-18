// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

// fakeUFW puts a fake `ufw` first on PATH. The in-process agent runs fleet's
// read-only probe through a real /bin/sh, so the probe reads whatever this
// host's files say. Tests using it cannot run in parallel (t.Setenv).
type fakeUFW struct{ dir string }

func newFakeUFW(t *testing.T) *fakeUFW {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the in-process agent's shell.exec and the fake ufw need a Unix /bin/sh")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$1 $2\" in\n" +
		"\"status verbose\") cat " + shellQuote(filepath.Join(dir, "status")) + " ;;\n" +
		"\"show added\") cat " + shellQuote(filepath.Join(dir, "added")) + " ;;\n" +
		"*) echo \"fake ufw: unexpected: $*\" >&2; exit 64 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "ufw"), []byte(script), 0o700); err != nil { // #nosec G306 -- test fake must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	f := &fakeUFW{dir: dir}
	f.set("Status: inactive\n", "(None)\n")
	return f
}

// set writes what `ufw status verbose` and `ufw show added` print. added is
// the rule list under ufw's header line.
func (f *fakeUFW) set(status, added string) {
	_ = os.WriteFile(filepath.Join(f.dir, "status"), []byte(status), 0o600)
	_ = os.WriteFile(filepath.Join(f.dir, "added"), []byte(ufwAddedHeader+added), 0o600)
}

// directFirewallHarness is a direct-mode server "loopback" (agent port 2222)
// served by an in-process agent whose firewall manager is a fake.
func directFirewallHarness(t *testing.T, manager *fakeFirewallManager) *App {
	t.Helper()
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeDirect,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	server := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
		FirewallManager:    manager,
	}
	errCh := make(chan error, 64)
	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40031", "127.0.0.1:2222")
		go func() { errCh <- server.ServeConn(serverConn) }()
		return clientConn, nil
	}
	if err := app.AddServer(ServerRecord{Name: "loopback", Address: "127.0.0.1", Port: 2222, Mode: transport.ModeDirect, User: "cenvero-agent"}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}
	t.Cleanup(func() {
		app.DisconnectPooledSessions()
		drainAgentServeErrs(t, errCh)
		_ = app.Close()
	})
	return app
}

func auditEntries(t *testing.T, app *App, action string) []logs.AuditEntry {
	t.Helper()
	entries, err := logs.NewAuditLog(filepath.Join(app.ConfigDir, "logs", "_audit.log")).ReadAll()
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	var out []logs.AuditEntry
	for _, e := range entries {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func requireRefusal(t *testing.T, err error, mustMention ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a lockout refusal, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "refusing to") || !strings.Contains(msg, "--force-i-have-console") {
		t.Fatalf("expected a lockout refusal naming the override, got: %v", err)
	}
	for _, want := range mustMention {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal should mention %q; got: %v", want, err)
		}
	}
}

// Direct mode: every legacy firewall/port entry point that can block traffic
// checks the agent port first and refuses with the command to run first.
func TestFirewallGuardDirectEntryPoints(t *testing.T) {
	ufw := newFakeUFW(t)
	manager := &fakeFirewallManager{status: proto.FirewallInfo{Enabled: false}}
	app := directFirewallHarness(t, manager)

	// port close <agent port>: refused outright; nothing reaches the agent.
	requireRefusal(t, app.SetPort("loopback", 2222, false), "2222")
	if slices.Contains(manager.actions, "port:2222:false") {
		t.Fatalf("the agent port was closed; actions=%v", manager.actions)
	}
	// Closing any other port is not gated.
	if err := app.SetPort("loopback", 443, false); err != nil {
		t.Fatalf("closing a non-agent port: %v", err)
	}

	// firewall add: rules that deny, reject or limit the agent port, and rules
	// fleet cannot read, are refused without reaching the agent.
	for _, rule := range []string{
		"deny 2222", "deny 2222/tcp", "reject 2222", "limit 2222/tcp",
		"deny from 10.0.0.0/8 to any port 2222", "deny 1000:3000/tcp", "deny 80,2222/tcp",
		"reset", "disable", "default deny incoming", "insert 1 deny 2222", "delete allow 2222", "deny ssh",
	} {
		requireRefusal(t, app.AddFirewallRule("loopback", rule))
		if slices.Contains(manager.actions, "rule:"+rule) {
			t.Fatalf("refused rule %q reached the agent; actions=%v", rule, manager.actions)
		}
	}
	// Rules that cannot block the agent port go through.
	for _, rule := range []string{"allow 2222", "allow 443/tcp", "deny 80", "deny 2222/udp", "limit 22/tcp", "deny from 10.0.0.0/8 to any port 80"} {
		if err := app.AddFirewallRule("loopback", rule); err != nil {
			t.Fatalf("rule %q must not be refused: %v", rule, err)
		}
	}

	// A broad block is safe only once the agent port is allowed ahead of it.
	requireRefusal(t, app.AddFirewallRule("loopback", "deny from 203.0.113.0/24"), "fleet port open loopback 2222")

	// firewall enable on an inactive ufw whose pending rules lack the agent
	// port: refused, naming the command to run first.
	requireRefusal(t, app.SetFirewall("loopback", true), "fleet port open loopback 2222")
	if slices.Contains(manager.actions, "enable:true") {
		t.Fatalf("firewall enabled despite the refusal; actions=%v", manager.actions)
	}

	// Remediate exactly as the refusal says, then retry.
	if err := app.SetPort("loopback", 2222, true); err != nil {
		t.Fatalf("port open 2222: %v", err)
	}
	ufw.set("Status: inactive\n", "ufw allow 2222\n")
	if err := app.SetFirewall("loopback", true); err != nil {
		t.Fatalf("enable after allowing the agent port: %v", err)
	}
	if !slices.Contains(manager.actions, "enable:true") {
		t.Fatalf("firewall was not enabled after the agent port was allowed; actions=%v", manager.actions)
	}
	if err := app.AddFirewallRule("loopback", "deny from 203.0.113.0/24"); err != nil {
		t.Fatalf("broad block after the agent port is allowed: %v", err)
	}

	// A rule that denies the agent port ahead of the allow keeps enable refused.
	ufw.set("Status: active\n\nTo                         Action      From\n--                         ------      ----\n2222/tcp                   DENY IN     Anywhere\n2222                       ALLOW IN    Anywhere\n", "ufw deny 2222/tcp\nufw allow 2222\n")
	requireRefusal(t, app.SetFirewall("loopback", true), "2222")

	// Fail closed: ufw state that cannot be read refuses.
	ufw.set("ERROR: You need to be root to run this script\n", "")
	requireRefusal(t, app.SetFirewall("loopback", true))

	if got := auditEntries(t, app, "firewall.lockout.refused"); len(got) < 5 {
		t.Fatalf("every refusal must be audit-logged; got %d entries: %+v", len(got), got)
	} else if got[0].Target != "loopback" || got[0].Operator == "" || !strings.Contains(got[0].Details, "port.close") {
		t.Errorf("refusal audit entry should name the server, operator and operation; got %+v", got[0])
	}
}

// A direct record with no valid port: fleet cannot tell which port must stay
// open, so tightening is refused even with the override (it could not allow the
// agent port first), while an allow rule still goes through.
func TestFirewallGuardUnknownAgentPort(t *testing.T) {
	newFakeUFW(t)
	manager := &fakeFirewallManager{status: proto.FirewallInfo{Enabled: false}}
	app := directFirewallHarness(t, manager)
	record, err := app.GetServer("loopback")
	if err != nil {
		t.Fatal(err)
	}
	record.Port = 0
	if err := app.SaveServer(record); err != nil {
		t.Fatal(err)
	}

	if err := app.AddFirewallRule("loopback", "allow 80"); err != nil {
		t.Fatalf("an allow rule needs no known agent port: %v", err)
	}
	force := FirewallGuardOptions{ForceIHaveConsole: true}
	for name, err := range map[string]error{
		"enable":    app.SetFirewallGuarded("loopback", true, force),
		"close 443": app.SetPortGuarded("loopback", 443, false, force),
		"deny 80":   app.AddFirewallRuleGuarded("loopback", "deny 80", force),
	} {
		if err == nil || !strings.Contains(err.Error(), "no valid agent port") || !strings.Contains(err.Error(), "cannot skip") {
			t.Errorf("%s: want an unskippable refusal naming the missing port; got %v", name, err)
		}
	}
	if slices.Contains(manager.actions, "enable:true") || slices.Contains(manager.actions, "port:443:false") || slices.Contains(manager.actions, "rule:deny 80") {
		t.Fatalf("a refused change reached the agent; actions=%v", manager.actions)
	}
	if got := auditEntries(t, app, "firewall.lockout.refused"); len(got) != 3 {
		t.Fatalf("each refusal must be audit-logged; got %+v", got)
	}
	if got := auditEntries(t, app, "firewall.lockout.override"); len(got) != 0 {
		t.Fatalf("nothing was overridden; got %+v", got)
	}
}

// A template that enables the firewall goes through the same gate.
func TestFirewallGuardTemplateApply(t *testing.T) {
	newFakeUFW(t) // inactive ufw, no pending rules
	manager := &fakeFirewallManager{status: proto.FirewallInfo{Enabled: false}}
	app := directFirewallHarness(t, manager)
	body := "name = \"locked\"\n[firewall]\nenabled = true\nopen_ports = [80]\n"
	if err := os.WriteFile(filepath.Join(app.ConfigDir, "templates", "locked.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	requireRefusal(t, app.ApplyTemplate("loopback", "locked.toml"), "fleet port open loopback 2222")
	if slices.Contains(manager.actions, "enable:true") {
		t.Fatalf("template enabled the firewall without the agent port allowed; actions=%v", manager.actions)
	}
}

// fakeReverseUFW is an in-process reverse-mode agent: it answers the firewall
// RPCs and the probe's shell.exec from fields the test sets.
type fakeReverseUFW struct {
	mu      sync.Mutex
	probe   string
	info    proto.FirewallInfo
	actions []string
}

func (f *fakeReverseUFW) call(_ context.Context, _ string, env proto.Envelope) (proto.Envelope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch env.Action {
	case "shell.exec":
		return proto.Envelope{Payload: proto.ExecResult{Stdout: f.probe}}, nil
	case "firewall.status":
	case "firewall.enable":
		f.info.Enabled = true
		f.actions = append(f.actions, "enable:true")
	case "firewall.disable":
		f.info.Enabled = false
		f.actions = append(f.actions, "enable:false")
	case "firewall.add_rule":
		rule, _ := proto.DecodePayload[proto.FirewallRulePayload](env.Payload)
		f.actions = append(f.actions, "rule:"+rule.Rule)
	case "port.set":
		p, _ := proto.DecodePayload[proto.PortActionPayload](env.Payload)
		f.actions = append(f.actions, fmt.Sprintf("port:%d:%t", p.Port, p.Open))
	default:
		return proto.Envelope{Error: &proto.Error{Code: "unexpected", Message: env.Action}}, nil
	}
	return proto.Envelope{Payload: f.info}, nil
}

func reverseUFWProbe(beforeRulesCount, outputPolicy string) string {
	return "Status: inactive\n#fleet-guard:added\n" + ufwAddedHeader + "(None)\n" +
		"#fleet-guard:defaults\nDEFAULT_INPUT_POLICY=\"DROP\"\nDEFAULT_OUTPUT_POLICY=\"" + outputPolicy + "\"\n" +
		"#fleet-guard:before-rules\n" + beforeRulesCount + "\n"
}

// Reverse mode: the agent dials out, so no inbound port is required; enabling
// must keep established/related traffic and leave outgoing traffic allowed.
func TestFirewallGuardReverseMode(t *testing.T) {
	t.Parallel()
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeReverse,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	host := &fakeReverseUFW{}
	app.ReverseRPCContext = host.call
	if err := app.AddServer(ServerRecord{Name: "edge", Address: "203.0.113.9", Port: 2222, Mode: transport.ModeReverse}); err != nil {
		t.Fatal(err)
	}

	// Stock ufw: before.rules accepts established/related, outgoing allowed.
	host.probe = reverseUFWProbe("1", "ACCEPT")
	if err := app.SetFirewall("edge", true); err != nil {
		t.Fatalf("stock ufw keeps the reverse session; enable must pass: %v", err)
	}

	// before.rules no longer accepts established/related: replies to the
	// agent's own outbound connection would be dropped.
	host.probe = reverseUFWProbe("0", "ACCEPT")
	requireRefusal(t, app.SetFirewall("edge", true), "established")

	// Outgoing default deny: the agent could not dial back in after a drop.
	host.probe = reverseUFWProbe("1", "DROP")
	requireRefusal(t, app.SetFirewall("edge", true), "fleet exec edge 'ufw default allow outgoing'")

	// Fail closed when before.rules cannot be read.
	host.probe = reverseUFWProbe("grep: /etc/ufw/before.rules: No such file or directory", "ACCEPT")
	requireRefusal(t, app.SetFirewall("edge", true))

	// The record's port is not an agent listener in reverse mode, so closing it
	// or denying it is not a lockout.
	if err := app.SetPort("edge", 2222, false); err != nil {
		t.Fatalf("reverse mode: closing port 2222 must not be refused: %v", err)
	}
	if err := app.AddFirewallRule("edge", "deny 2222"); err != nil {
		t.Fatalf("reverse mode: denying port 2222 must not be refused: %v", err)
	}
	if n := strings.Count(strings.Join(host.actions, ","), "enable:true"); n != 1 {
		t.Fatalf("only the stock-ufw enable should have reached the agent; actions=%v", host.actions)
	}
}
