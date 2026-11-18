// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/webui"
)

func TestParseUseArgs(t *testing.T) {
	type a = webui.CommandArg
	cases := []struct {
		use  string
		path []string
		want []a
	}{
		{"exec <server> <command>", []string{"exec"}, []a{{Name: "server", Required: true, Server: true}, {Name: "command", Required: true}}},
		{"list", []string{"server", "list"}, []a{}},
		{"show <name>", []string{"server", "show"}, []a{{Name: "name", Required: true, Server: true}}},
		{"add [name] [ip]", []string{"server", "add"}, []a{{Name: "name", Server: true}, {Name: "ip"}}},
		{"journal <server> --unit <name>", []string{"journal"}, []a{{Name: "server", Required: true, Server: true}}},
		{"update [--all | --group EXPR] [--canary N]", []string{"agent", "update"}, []a{}},
		{"version [--all|<server>]", []string{"agent", "version"}, []a{{Name: "server", Server: true}}},
		{"auto [on|off|status]", []string{"sync-agent", "auto"}, []a{{Name: "on|off|status", Suggest: []string{"on", "off", "status"}}}},
		{"set <deny|confirm> <patterns>", []string{"cmd-policy", "set"}, []a{{Name: "deny|confirm", Required: true, Suggest: []string{"deny", "confirm"}}, {Name: "patterns", Required: true}}},
		{"compress <server> <archive> <item>...", []string{"file", "compress"}, []a{{Name: "server", Required: true, Server: true}, {Name: "archive", Required: true}, {Name: "item", Required: true, Variadic: true}}},
		{"guard <server> <risky command...>", []string{"guard"}, []a{{Name: "server", Required: true, Server: true}, {Name: "risky command", Required: true, Variadic: true}}},
		{"tag [<server> [key=value...]]", []string{"tag"}, []a{{Name: "server", Server: true}, {Name: "key=value", Variadic: true}}},
		{"download <server> <remote> [local] | <server:remote> [local]", []string{"file", "download"}, []a{{Name: "server", Required: true, Server: true}, {Name: "remote", Required: true}, {Name: "local"}}},
		{"create --name <n> [scope flags]", []string{"token", "create"}, []a{}},
		{"tunnel <server> <localPort>:<targetHost>:<targetPort>", []string{"tunnel"}, []a{{Name: "server", Required: true, Server: true}, {Name: "localPort:targetHost:targetPort", Required: true}}},
		{"capture <server> --paths /etc/ssh/sshd_config,/etc/fstab", []string{"drift", "capture"}, []a{{Name: "server", Required: true, Server: true}}},
		{"enable <server> --safe", []string{"fw", "enable"}, []a{{Name: "server", Required: true, Server: true}}},
		{"ai [command...]", []string{"ai"}, []a{{Name: "command", Variadic: true}}},
	}
	for _, c := range cases {
		got := parseUseArgs(c.use, c.path)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseUseArgs(%q)\n got %+v\nwant %+v", c.use, got, c.want)
		}
	}
}

func TestWebConsoleCatalogCoversTheCLI(t *testing.T) {
	root := NewRootCommand()
	catalog := webConsoleCatalog(root)
	byPath := map[string]webui.CommandSpec{}
	for _, spec := range catalog {
		byPath[strings.Join(spec.Path, " ")] = spec
	}
	for _, want := range []string{"server add", "server list", "service restart", "exec", "alerts ack", "logs", "journal", "health", "update apply", "agent update", "secret list", "token list", "firewall status", "job run", "run", "cron add", "tag"} {
		spec, ok := byPath[want]
		if !ok {
			t.Fatalf("catalog is missing %q", want)
		}
		if spec.Unavailable != "" {
			t.Fatalf("%q should be runnable from the UI: %s", want, spec.Unavailable)
		}
	}
	for _, want := range []string{"dashboard", "ssh", "config edit", "file ui", "daemon", "self-uninstall", "secret set", "token create"} {
		if spec, ok := byPath[want]; !ok || spec.Unavailable == "" {
			t.Fatalf("%q must be listed as unavailable", want)
		}
	}
	for path, spec := range byPath {
		for _, f := range spec.Flags {
			if f.Name == "token" || f.Name == "config-dir" || f.Name == "help" {
				t.Fatalf("%s exposes the reserved flag --%s", path, f.Name)
			}
		}
	}
	if !byPath["server remove"].Destructive || !byPath["update apply"].Destructive || byPath["server list"].Destructive {
		t.Fatal("destructive marking is off")
	}
	if !byPath["secret set"].Stdin {
		t.Fatal("secret set should accept stdin")
	}
	var sensitive bool
	for _, f := range byPath["server add"].Flags {
		if f.Name == "login-password" {
			sensitive = f.Sensitive
		}
	}
	if !sensitive {
		t.Fatal("server add --login-password must be marked sensitive")
	}
}

func TestWebConsoleLauncherPinsConfigDirAndToken(t *testing.T) {
	root := NewRootCommand()
	catalog := webConsoleCatalog(root)
	launch := webConsoleLauncher(root, t.TempDir(), "tok-123", catalog)

	cmd, err := launch(context.Background(), []string{"server", "list"}, []string{"--format", "json"})
	if err != nil {
		t.Fatal(err)
	}
	args := cmd.Args[1:]
	if args[0] != "--config-dir="+cmd.Dir || strings.Join(args[1:], " ") != "server list --format json" {
		t.Fatalf("config dir not pinned first: %v", args)
	}
	env := strings.Join(cmd.Env, "\n")
	if !strings.Contains(env, "FLEET_TOKEN=tok-123") || strings.Count(env, "FLEET_TOKEN=") != 1 {
		t.Fatalf("token not pinned exactly once: %v", cmd.Env)
	}
	if !strings.Contains(env, "FLEET_CONFIG_DIR="+cmd.Dir) || strings.Count(env, "FLEET_CONFIG_DIR=") != 1 {
		t.Fatalf("config dir not pinned in the env exactly once: %v", cmd.Env)
	}
	if !strings.Contains(env, "NO_COLOR=1") {
		t.Fatal("colour not disabled")
	}

	// A trailing value-taking flag cannot swallow the pinned config dir: it is
	// first, so "--format" just has no value (and the child reports that).
	cmd, err = launch(context.Background(), []string{"server", "list"}, []string{"--format"})
	if err != nil {
		t.Fatal(err)
	}
	if got := cmd.Args[1:]; got[0] != "--config-dir="+cmd.Dir || got[len(got)-1] != "--format" {
		t.Fatalf("args %v", got)
	}

	// "--" keeps the operator's text positional.
	cmd, err = launch(context.Background(), []string{"exec"}, []string{"web-01", "--", "ls", "-la"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cmd.Args[1:], " "); !strings.HasPrefix(got, "--config-dir=") || !strings.HasSuffix(got, " exec web-01 -- ls -la") {
		t.Fatalf("args %q", got)
	}

	// Unavailable commands are refused, and so are arguments that make Cobra
	// resolve a command other than the one picked (a subcommand named in the
	// arguments would otherwise run under the parent's name and confirmation).
	for _, c := range []struct{ path, args []string }{
		{[]string{"filemanager"}, []string{"ui"}},
		{[]string{"dashboard"}, nil},
		{[]string{"config", "edit"}, nil},
		{[]string{"token", "create"}, []string{"--name", "x"}},
		{[]string{"sync-agent"}, []string{"auto", "off"}},
	} {
		if _, err := launch(context.Background(), c.path, c.args); err == nil {
			t.Fatalf("%v %v should be refused", c.path, c.args)
		}
	}

	// Without a token the child is unscoped, even if this process has one in its env.
	t.Setenv("FLEET_TOKEN", "parent-token")
	launch = webConsoleLauncher(root, t.TempDir(), "", catalog)
	cmd, err = launch(context.Background(), []string{"version"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(cmd.Env, "\n"), "FLEET_TOKEN=") {
		t.Fatal("an inherited FLEET_TOKEN leaked into an unscoped run")
	}
}

// Commands that read or write the controller's keys, tokens or secrets, or
// loosen its guardrails, stay terminal-only even with the Control view on.
func TestWebConsoleCatalogKeepsCredentialCommandsOff(t *testing.T) {
	byPath := map[string]webui.CommandSpec{}
	for _, spec := range webConsoleCatalog(NewRootCommand()) {
		byPath[strings.Join(spec.Path, " ")] = spec
	}
	for _, key := range []string{
		"backup", "config backup", "config restore", "config import", "config export", "config show",
		"database show", "database shift", "token create", "token revoke", "secret set", "secret rotate",
		"secret rm", "key rotate", "server enroll-token", "automation set", "automation rm",
		"cmd-policy set", "policy set", "approve", "adjust-init",
	} {
		spec, ok := byPath[key]
		if !ok {
			t.Fatalf("%q is missing from the catalog; the web UI's block list names a command that does not exist", key)
		}
		if spec.Unavailable == "" {
			t.Fatalf("%q must be unavailable in the web UI", key)
		}
	}
	// Reading names and fingerprints is fine.
	for _, key := range []string{"token list", "secret list", "key fingerprint", "server list", "approvals"} {
		if spec, ok := byPath[key]; ok && spec.Unavailable != "" {
			t.Fatalf("%q should stay available: %s", key, spec.Unavailable)
		}
	}
}

// The Control and Terminal views are opt-in: `file ui` and `filemanager ui`
// start without them unless asked.
func TestWebUIControlAndTerminalAreOptIn(t *testing.T) {
	root := NewRootCommand()
	for _, path := range [][]string{{"file", "ui"}, {"filemanager", "ui"}} {
		cmd, _, err := root.Find(path)
		if err != nil || strings.Join(path, " ") != strings.TrimPrefix(cmd.CommandPath(), root.Name()+" ") {
			t.Fatalf("find %v: %v (%s)", path, err, cmd.CommandPath())
		}
		for _, name := range []string{"control", "terminal"} {
			f := cmd.Flags().Lookup(name)
			if f == nil || f.Value.Type() != "bool" || f.DefValue != "false" {
				t.Fatalf("%v --%s must be a bool flag that defaults to off: %+v", path, name, f)
			}
		}
	}
}

// The terminal child is exactly `fleet --config-dir=<dir> ssh -- <server>`,
// with this UI's token (or none) and config dir pinned in its environment.
func TestWebTerminalLauncherPinsConfigDirAndToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLEET_TOKEN", "parent-token")
	t.Setenv("FLEET_CONFIG_DIR", "/somewhere/else")
	cmd, err := webTerminalLauncher(dir, "tok-9")(context.Background(), "web-01")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cmd.Args[1:], " "); got != "--config-dir="+dir+" ssh -- web-01" {
		t.Fatalf("args %q", got)
	}
	env := strings.Join(cmd.Env, "\n")
	if strings.Count(env, "FLEET_TOKEN=") != 1 || !strings.Contains(env, "FLEET_TOKEN=tok-9") {
		t.Fatalf("token not pinned exactly once: %v", cmd.Env)
	}
	if strings.Count(env, "FLEET_CONFIG_DIR=") != 1 || !strings.Contains(env, "FLEET_CONFIG_DIR="+dir) {
		t.Fatalf("config dir not pinned exactly once: %v", cmd.Env)
	}
	cmd, err = webTerminalLauncher(dir, "")(context.Background(), "web-01")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(cmd.Env, "\n"), "FLEET_TOKEN=") {
		t.Fatal("an inherited FLEET_TOKEN leaked into an unscoped terminal")
	}
	for _, bad := range []string{"", "-x", "--config-dir=/tmp", "a\nb"} {
		if _, err := webTerminalLauncher(dir, "")(context.Background(), bad); err == nil {
			t.Fatalf("server name %q should be refused", bad)
		}
	}
}
