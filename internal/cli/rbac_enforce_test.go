// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"os"
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
		// newly-scoped server-first commands (the cron/tunnel/sync bypass fix)
		{"tunnel", "tunnel", []string{"web-01", "8080:80"}, "web-01"},
		{"sync", "sync", []string{"web-01", "./a", "/b"}, "web-01"},
		{"cron add", "cron", []string{"web-01"}, "web-01"}, // sub consumed, server is args[0]
		{"cron list", "cron", []string{"web-01"}, "web-01"},
		// subcommand commands: server is args[0] because the sub was consumed
		{"file rm", "file", []string{"web-01", "/tmp/x"}, "web-01"},
		{"firewall enable", "firewall", []string{"web-01"}, "web-01"},
		{"fw enable", "fw", []string{"web-01"}, "web-01"},
		{"server remove", "server", []string{"web-01"}, "web-01"},
		{"port open", "port", []string{"web-01", "80"}, "web-01"},
		{"service start", "service", []string{"web-01", "nginx"}, "web-01"},
		// inventory: optional positional server flows into the scope check
		{"inventory server", "inventory", []string{"web-01"}, "web-01"},
		{"inventory bare", "inventory", nil, ""},
		// commands that do not take a server positional
		{"status", "status", nil, ""},
		{"health", "health", nil, ""},
		// no positional present -> empty (caller fail-closes)
		{"firewall no args", "firewall", nil, ""},
		{"server no args", "server", nil, ""},
		{"tunnel no args", "tunnel", nil, ""},
		{"cron no args", "cron", nil, ""},
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
		// cron/tunnel/sync take a server as the FIRST positional but were MISSING
		// from serverArgCommands, so a server-scoped token ran them against ANY
		// server. They must now be scope-denied out of scope (db-01).
		{"cron add out-of-scope", []string{"cron", "add", "db-01", "--name", "j", "--schedule", "0 3 * * *", "--cmd", "true"}},
		{"cron list out-of-scope", []string{"cron", "list", "db-01"}},
		{"tunnel out-of-scope", []string{"tunnel", "db-01", "8080:80"}},
		{"sync out-of-scope", []string{"sync", "db-01", "./a", "/b"}},
		// ssh/tag/doctor/template were ALSO missing from serverArgCommands (round 2
		// of the audit): ssh opened a root shell on any server, tag retagged any
		// server. They must be scope-denied out of scope now.
		{"ssh out-of-scope", []string{"ssh", "db-01"}},
		{"tag set out-of-scope", []string{"tag", "db-01", "role=admin"}},
		{"doctor out-of-scope", []string{"doctor", "db-01"}},
		{"template apply out-of-scope", []string{"template", "apply", "db-01", "hardening"}},
		// FAIL-CLOSED backstop: a controller-management / interactive command that
		// is neither a scoped server command nor an allowed local command must be
		// denied for a server-scoped token (it could reach out-of-scope servers).
		{"backup denied fail-closed", []string{"backup"}},
		{"dashboard denied fail-closed", []string{"dashboard"}},
		{"config show denied fail-closed", []string{"config", "show"}},
		// fan-out exec denied for a server-scoped token
		{"exec --all", []string{"exec", "--all", "uptime"}},
		{"exec --group", []string{"exec", "--group", "role=web", "uptime"}},
		// `run` (playbooks) targets multiple servers that can't be vetted by the
		// single-target check, so a server-scoped token is denied it outright. The
		// playbook path need not exist: the RBAC gate fires before the file is read.
		{"run playbook", []string{"run", "playbook.yaml"}},
		{"run playbook --group", []string{"run", "playbook.yaml", "--group", "role=web"}},
		// fleet-wide read fan-outs leak out-of-scope servers; denied for a scoped
		// token (top/health always; bare inventory).
		{"top fleet-wide", []string{"top", "--once"}},
		{"health fleet-wide", []string{"health"}},
		{"inventory fleet-wide", []string{"inventory"}},
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

	// Positive control: an IN-scope single-server inventory (web-01) must NOT be
	// scope-denied or hit the fleet-wide-read denial — only the bare fan-out is.
	out, _ = run("inventory", "web-01")
	if strings.Contains(strings.ToLower(out), "denied") {
		t.Errorf("in-scope 'inventory web-01' was wrongly denied: %q", out)
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

// TestPerTokenSecretAllowlist pins the FL-004 privesc fix: a SCOPED, exec-capable
// token may resolve `exec --secret VAR=@name` ONLY for secrets in its
// AllowSecrets allowlist — it can no longer read every stored secret by
// injecting an arbitrary `@name`. An UNSCOPED admin token is unrestricted.
func TestPerTokenSecretAllowlist(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess RBAC test skipped in -short mode")
	}
	bin := buildFleetBinary(t)
	dir := initConfigDir(t, bin)

	// Seed two secrets: one the scoped token may read, one it may not.
	secrets := core.NewSecretStore(dir)
	if err := secrets.Set("allowed_key", "av"); err != nil {
		t.Fatalf("set allowed secret: %v", err)
	}
	if err := secrets.Set("forbidden_key", "fv"); err != nil {
		t.Fatalf("set forbidden secret: %v", err)
	}

	store := core.NewTokenStore(dir)
	// Scoped (exec-capable) token allowed ONLY allowed_key. Not server-scoped, so
	// the single-server exec passes the RBAC server check and we isolate the
	// secret-authorization behavior.
	scoped, err := store.Create(core.Token{
		Name:          "ci",
		AllowCommands: []string{"exec"},
		AllowSecrets:  []string{"allowed_key"},
	})
	if err != nil {
		t.Fatalf("create scoped token: %v", err)
	}
	// Unscoped admin-equivalent token: unrestricted.
	admin, err := store.Create(core.Token{Name: "admin"})
	if err != nil {
		t.Fatalf("create admin token: %v", err)
	}

	run := func(token string, args ...string) (string, int) {
		full := append([]string{"--config-dir", dir, "--token", token}, args...)
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

	// The scoped token referencing a secret NOT in its allowlist must be denied at
	// resolution time (before any agent contact), with a non-zero exit.
	out, code := run(scoped.ID, "exec", "web-01", "--secret", "X=@forbidden_key", "echo hi")
	if code == 0 {
		t.Errorf("scoped token read a forbidden secret (privesc!); output: %q", out)
	}
	if !strings.Contains(strings.ToLower(out), "denied") || !strings.Contains(out, "allowed-secrets") {
		t.Errorf("forbidden-secret denial output %q does not report the secret allowlist", out)
	}

	// The scoped token referencing its ALLOWED secret must pass secret
	// authorization (it may still fail later with no live agent, but NOT with a
	// secret-allowlist denial).
	out, _ = run(scoped.ID, "exec", "web-01", "--secret", "X=@allowed_key", "echo hi")
	if strings.Contains(out, "allowed-secrets") {
		t.Errorf("scoped token was wrongly denied its allowed secret: %q", out)
	}

	// The unscoped admin token is unrestricted: referencing any secret must not be
	// denied by the secret allowlist.
	out, _ = run(admin.ID, "exec", "web-01", "--secret", "X=@forbidden_key", "echo hi")
	if strings.Contains(out, "allowed-secrets") {
		t.Errorf("unscoped admin token was wrongly secret-allowlist-denied: %q", out)
	}
}

// TestPlaybookCmdPolicyGate pins the FL-injection fix: `fleet run` (playbooks)
// now applies the cmd-policy deny gate to every step command, so an operator
// can no longer run a denied command through a playbook step. A playbook whose
// step apply matches a deny pattern must be refused before any step runs.
func TestPlaybookCmdPolicyGate(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess cmd-policy test skipped in -short mode")
	}
	bin := buildFleetBinary(t)
	dir := initConfigDir(t, bin)

	// Deny any command containing "rm -rf /".
	store, err := core.NewCmdPolicyStore(dir)
	if err != nil {
		t.Fatalf("NewCmdPolicyStore: %v", err)
	}
	if err := store.SetDenyPatterns([]string{"rm -rf /"}); err != nil {
		t.Fatalf("SetDenyPatterns: %v", err)
	}

	writePlaybook := func(name, apply string) string {
		path := filepath.Join(t.TempDir(), name)
		body := "name: pb\nsteps:\n  - name: do\n    apply: " + apply + "\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write playbook: %v", err)
		}
		return path
	}

	run := func(args ...string) (string, int) {
		full := append([]string{"--config-dir", dir}, args...)
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

	// A denied step command must refuse the whole run, even under --dry-run (so the
	// plan a denied playbook would print still reports the refusal).
	denied := writePlaybook("denied.yaml", "\"rm -rf /\"")
	out, code := run("run", denied, "--dry-run")
	if code == 0 {
		t.Errorf("playbook with a denied step ran (bypass!); output: %q", out)
	}
	if !strings.Contains(out, "cmd-policy deny pattern") {
		t.Errorf("playbook denial output %q does not mention the cmd-policy deny pattern", out)
	}

	// A playbook whose step does NOT match the deny pattern must pass the gate (the
	// dry-run plan prints without a policy refusal).
	ok := writePlaybook("ok.yaml", "uptime")
	out, _ = run("run", ok, "--dry-run")
	if strings.Contains(out, "cmd-policy deny pattern") {
		t.Errorf("a non-denied playbook was wrongly blocked by cmd-policy: %q", out)
	}
}

// TestCmdPolicyGatesNonExecCommands proves the cmd-policy deny gate that `exec`
// enforces also covers the other operator-supplied-command paths — `job run`,
// `guard`, and `cron add` — which previously bypassed it entirely.
func TestCmdPolicyGatesNonExecCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess cmd-policy test skipped in -short mode")
	}
	bin := buildFleetBinary(t)
	dir := initConfigDir(t, bin)

	// Seed a deny pattern that blocks any command containing "rm -rf /".
	store, err := core.NewCmdPolicyStore(dir)
	if err != nil {
		t.Fatalf("NewCmdPolicyStore: %v", err)
	}
	if err := store.SetDenyPatterns([]string{"rm -rf /"}); err != nil {
		t.Fatalf("SetDenyPatterns: %v", err)
	}

	run := func(args ...string) (string, int) {
		full := append([]string{"--config-dir", dir}, args...)
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

	// Each of these carries a denied command and must be refused by the cmd-policy
	// gate (non-zero exit + the deny-pattern message), NOT executed.
	cases := []struct {
		name string
		args []string
	}{
		{"job run", []string{"job", "run", "web-01", "rm -rf /"}},
		{"guard", []string{"guard", "web-01", "--revert-after", "1m", "rm -rf /"}},
		{"cron add", []string{"cron", "add", "web-01", "--name", "j", "--schedule", "0 3 * * *", "--cmd", "rm -rf /"}},
	}
	for _, c := range cases {
		out, code := run(c.args...)
		if code == 0 {
			t.Errorf("%s: exit 0, want non-zero (command should be blocked); output: %q", c.name, out)
		}
		if !strings.Contains(out, "cmd-policy deny pattern") {
			t.Errorf("%s: output %q does not mention the cmd-policy deny pattern", c.name, out)
		}
	}

	// Positive control: a command that does NOT match the deny pattern must pass
	// the cmd-policy gate (it may still fail later with no live agent, but NOT for
	// a policy reason).
	out, _ := run("job", "run", "web-01", "uptime")
	if strings.Contains(out, "cmd-policy deny pattern") {
		t.Errorf("non-denied 'job run uptime' was wrongly blocked by cmd-policy: %q", out)
	}
}

// TestCmdPolicyCorruptFailsClosed proves the HIGH fail-open fix: when the
// cmd-policy file EXISTS but cannot be parsed, exec (and the shared gate) refuse
// to run rather than treating it as "no policy" and executing anyway.
func TestCmdPolicyCorruptFailsClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess cmd-policy test skipped in -short mode")
	}
	bin := buildFleetBinary(t)
	dir := initConfigDir(t, bin)

	// Write a corrupt (unparseable) cmd-policy.json.
	if err := os.WriteFile(core.CmdPolicyPath(dir), []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("write corrupt cmd-policy: %v", err)
	}

	run := func(args ...string) (string, int) {
		full := append([]string{"--config-dir", dir}, args...)
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

	// exec must FAIL CLOSED: refuse to run with a clear message telling the
	// operator to fix/remove the policy.
	out, code := run("exec", "web-01", "uptime")
	if code == 0 {
		t.Fatalf("exec ran despite a corrupt cmd-policy (fail-open!); output: %q", out)
	}
	if !strings.Contains(out, "refusing to run") || !strings.Contains(out, "command policy") {
		t.Fatalf("exec error %q does not clearly report the corrupt command policy", out)
	}

	// job run goes through the same shared gate and must also refuse.
	out, code = run("job", "run", "web-01", "uptime")
	if code == 0 {
		t.Fatalf("job run ran despite a corrupt cmd-policy (fail-open!); output: %q", out)
	}
	if !strings.Contains(out, "refusing to run") {
		t.Fatalf("job run error %q does not report fail-closed refusal", out)
	}
}
