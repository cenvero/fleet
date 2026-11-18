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
	for _, want := range []string{"server add", "server list", "service restart", "exec", "alerts ack", "logs", "journal", "health", "update apply", "agent update", "secret set", "token create", "firewall status", "job run", "run", "cron add", "tag"} {
		spec, ok := byPath[want]
		if !ok {
			t.Fatalf("catalog is missing %q", want)
		}
		if spec.Unavailable != "" {
			t.Fatalf("%q should be runnable from the UI: %s", want, spec.Unavailable)
		}
	}
	for _, want := range []string{"dashboard", "ssh", "config edit", "file ui", "daemon", "self-uninstall"} {
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

	cmd, err := launch(context.Background(), []string{"server", "list", "--format", "json"})
	if err != nil {
		t.Fatal(err)
	}
	args := cmd.Args[1:]
	if args[len(args)-1] != "--config-dir="+cmd.Dir {
		t.Fatalf("config dir not pinned: %v", args)
	}
	env := strings.Join(cmd.Env, "\n")
	if !strings.Contains(env, "FLEET_TOKEN=tok-123") || strings.Count(env, "FLEET_TOKEN=") != 1 {
		t.Fatalf("token not pinned exactly once: %v", cmd.Env)
	}
	if !strings.Contains(env, "NO_COLOR=1") {
		t.Fatal("colour not disabled")
	}

	// "--" keeps the operator's text positional, and the pinned flag before it.
	cmd, err = launch(context.Background(), []string{"exec", "web-01", "--", "ls", "-la"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cmd.Args[1:], " "); !strings.HasPrefix(got, "exec web-01 --config-dir=") || !strings.HasSuffix(got, " -- ls -la") {
		t.Fatalf("args %q", got)
	}

	// Arguments that make Cobra resolve a different, unavailable command are refused.
	for _, args := range [][]string{{"filemanager", "ui"}, {"dashboard"}, {"config", "edit"}} {
		if _, err := launch(context.Background(), args); err == nil {
			t.Fatalf("%v should be refused", args)
		}
	}

	// Without a token the child is unscoped, even if this process has one in its env.
	t.Setenv("FLEET_TOKEN", "parent-token")
	launch = webConsoleLauncher(root, t.TempDir(), "", catalog)
	cmd, err = launch(context.Background(), []string{"version"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(cmd.Env, "\n"), "FLEET_TOKEN=") {
		t.Fatal("an inherited FLEET_TOKEN leaked into an unscoped run")
	}
}
