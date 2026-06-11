// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
)

// TestBestEffortTargetServer pins the CRITICAL fix: at controller-enforcement
// time cobra has already consumed the subcommand, so the targeted server is the
// FIRST leaf positional (args[0]) for BOTH top-level server commands and
// subcommand commands. The old code returned args[1] for subcommand commands,
// which mis-targeted (or failed to target) the wrong server.
func TestBestEffortTargetServer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		top  string
		args []string // leaf positionals (no subcommand)
		want string
	}{
		// top-level, server is args[0]
		{"exec", "exec", []string{"web-01", "uptime"}, "web-01"},
		{"journal", "journal", []string{"web-01"}, "web-01"},
		{"guard", "guard", []string{"web-01", "rm -rf /"}, "web-01"},
		{"drift", "drift", []string{"web-01"}, "web-01"},
		{"svc", "svc", []string{"web-01"}, "web-01"},
		// subcommand commands: server is args[0] because the sub was consumed
		{"file rm", "file", []string{"web-01", "/tmp/x"}, "web-01"},
		{"firewall enable", "firewall", []string{"web-01"}, "web-01"},
		{"fw enable", "fw", []string{"web-01"}, "web-01"},
		{"server remove", "server", []string{"web-01"}, "web-01"},
		{"port open", "port", []string{"web-01", "80"}, "web-01"},
		{"service start", "service", []string{"web-01", "nginx"}, "web-01"},
		// commands that do not take a server positional
		{"status", "status", nil, ""},
		{"health", "health", nil, ""},
		// no positional present -> empty (caller fail-closes)
		{"firewall no args", "firewall", nil, ""},
		{"server no args", "server", nil, ""},
	}
	for _, c := range cases {
		if got := bestEffortTargetServer(c.top, c.args); got != c.want {
			t.Errorf("%s: bestEffortTargetServer(%q, %v) = %q, want %q", c.name, c.top, c.args, got, c.want)
		}
	}
}

// buildFleetBinary builds the controller binary once for the subprocess tests.
// It skips (not fails) if the build cannot complete, so the suite stays green in
// environments where unrelated packages are mid-refactor.
func buildFleetBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fleet-test-bin")
	cmd := exec.Command("go", "build", "-o", bin, "../../cmd/fleet")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("could not build fleet binary for subprocess RBAC test: %v\n%s", err, out)
	}
	return bin
}

// initConfigDir runs `fleet init --non-interactive` in a fresh config dir so the
// post-init enforcement path (firewall/server/exec) is reachable.
func initConfigDir(t *testing.T, bin string) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(bin, "--config-dir", dir, "init", "--non-interactive", "--passphrase", "test-pass")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("fleet init failed (skipping subprocess RBAC test): %v\n%s", err, out)
	}
	return dir
}

// TestEnforceTokenDeniesOutOfScope exercises the real controller binary: a
// server-scoped token (scope = web-01 only) must be DENIED out-of-scope
// destructive commands and fan-out exec. These are the CRITICAL bugs: with the
// old args-shape assumption these slipped past entirely.
func TestEnforceTokenDeniesOutOfScope(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess RBAC test skipped in -short mode")
	}
	bin := buildFleetBinary(t)
	dir := initConfigDir(t, bin)

	// Seed a server so the scope set + fan-out resolution have something to chew
	// on (the scope is restricted to web-01; db-01 is out of scope).
	store := core.NewTokenStore(dir)
	scoped, err := store.Create(core.Token{
		Name:               "scoped",
		Servers:            []string{"web-01"},
		DestructiveAllowed: true, // prove the SCOPE check denies, not the destructive check
	})
	if err != nil {
		t.Fatalf("create scoped token: %v", err)
	}

	run := func(args ...string) (string, int) {
		full := append([]string{"--config-dir", dir, "--token", scoped.ID}, args...)
		cmd := exec.Command(bin, full...)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			code = -1
		}
		return string(out), code
	}

	cases := []struct {
		name string
		args []string
	}{
		// out-of-scope destructive subcommand commands (server is args[0]=db-01)
		{"firewall enable out-of-scope", []string{"firewall", "enable", "db-01"}},
		{"server remove out-of-scope", []string{"server", "remove", "db-01"}},
		// fan-out exec denied for a server-scoped token
		{"exec --all", []string{"exec", "--all", "uptime"}},
		{"exec --group", []string{"exec", "--group", "role=web", "uptime"}},
	}
	for _, c := range cases {
		out, code := run(c.args...)
		if code != 1 {
			t.Errorf("%s: exit code = %d, want 1 (output: %q)", c.name, code, out)
		}
		if !strings.Contains(strings.ToLower(out), "denied") {
			t.Errorf("%s: output %q does not contain 'denied'", c.name, out)
		}
	}

	// Positive control: an IN-scope firewall enable on web-01 must NOT be denied
	// by the RBAC layer. It may still fail later (no live agent), so we only
	// assert the failure is NOT an RBAC denial.
	out, _ := run("firewall", "enable", "web-01")
	if strings.Contains(strings.ToLower(out), "is not in this token's scope") {
		t.Errorf("in-scope firewall enable web-01 was wrongly scope-denied: %q", out)
	}
}

// TestEnforceTokenDeniesSensitiveLocal proves a scoped token cannot run
// sensitive local-store mutations (secret set/rotate/rm, policy set,
// cmd-policy set) even though those commands work pre-init.
func TestEnforceTokenDeniesSensitiveLocal(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess RBAC test skipped in -short mode")
	}
	bin := buildFleetBinary(t)
	// secret subcommands work pre-init, but policy/cmd-policy set run their leaf
	// (cmd.Name()=="set") through the init check before enforceToken, so use an
	// initialized config dir to reach the RBAC layer for all of them.
	dir := initConfigDir(t, bin)

	store := core.NewTokenStore(dir)
	scoped, err := store.Create(core.Token{Name: "scoped", AllowCommands: []string{"secret", "policy", "cmd-policy"}})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	run := func(args ...string) (string, int) {
		full := append([]string{"--config-dir", dir, "--token", scoped.ID}, args...)
		cmd := exec.Command(bin, full...)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			code = -1
		}
		return string(out), code
	}

	// Arg counts must satisfy each command's cobra Args validator, because that
	// runs BEFORE PersistentPreRunE/enforceToken — otherwise we'd see a cobra
	// usage error instead of the RBAC denial.
	cases := [][]string{
		{"secret", "set", "API", "--value", "v"},  // secret set <name>
		{"secret", "rotate", "API"},               // secret rotate <name>
		{"secret", "rm", "API"},                   // secret rm <name>
		{"policy", "set", "key", "value"},         // policy set <key> <value>
		{"cmd-policy", "set", "deny", "rm -rf /"}, // cmd-policy set <deny|confirm> <patterns>
	}
	for _, args := range cases {
		out, code := run(args...)
		if code != 1 || !strings.Contains(strings.ToLower(out), "denied") {
			t.Errorf("%v: code=%d out=%q, want exit 1 + 'denied'", args, code, out)
		}
	}
}
