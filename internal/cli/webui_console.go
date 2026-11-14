// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/cenvero/fleet/internal/webui"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The web UI's Control view runs fleet commands as child processes of this
// same binary (see internal/webui/console.go). This file builds the command
// catalog the browser sees from the live Cobra tree, and the launcher that
// starts each run, so new commands appear in the UI with no extra wiring.

// webUnavailable lists commands the browser cannot drive, and why. They need
// a terminal (full-screen UIs, an interactive shell, $EDITOR), or they would
// set up, replace or remove the controller this UI is running from.
var webUnavailable = map[string]string{
	"dashboard":      "it is a full-screen terminal UI. Run `fleet dashboard` in a terminal, or use the Fleet view here",
	"files":          "it is a full-screen terminal UI. Use the Files view here",
	"filemanager":    "it is a full-screen terminal UI. Use the Files view here",
	"filemanager ui": "this web UI is already running",
	"file ui":        "this web UI is already running",
	"ssh":            "it is an interactive shell. Open it from the Terminal tab",
	"config edit":    "it opens $EDITOR. Use `config set`, or run `fleet config edit` in a terminal",
	"daemon":         "it runs the daemon in the foreground. Use `start` / `stop` to run it in the background",
	"init":           "it sets up the controller this UI is already running from. Run it in a terminal",
	"recover":        "it re-attaches the controller this UI is running from. Run it in a terminal",
	"self-uninstall": "it removes the controller this UI is running from. Run it in a terminal",
}

// webDestructiveLeaves are command names that change or remove things in a
// way worth a confirmation click in the browser.
var webDestructiveLeaves = map[string]bool{
	"remove": true, "rm": true, "revoke": true, "rollback": true, "restore": true, "import": true,
	"disable": true, "stop": true, "rotate": true, "shift": true, "reject": true, "revert": true,
	"close": true, "approve": true, "move": true,
}

var webDestructivePaths = map[string]bool{
	"agent update": true, "update apply": true, "server mode": true, "firewall enable": true,
	"fw enable": true, "sync-agent": true, "key rotate": true, "restart": true, "service restart": true,
	"svc restart": true, "guard": true, "cmd-policy set": true, "policy set": true, "database shift": true,
}

// webStdinCommands read their main input from stdin when no flag supplies it.
var webStdinCommands = map[string]bool{"secret set": true, "automation set": true}

// webSensitiveFlags are flags whose values must not be echoed in the UI.
var webSensitiveFlags = map[string]bool{
	"value": true, "login-password": true, "password": true, "dsn": true, "db-dsn": true,
}

// webConsoleCatalog walks the command tree and describes every runnable,
// visible command for the browser.
func webConsoleCatalog(root *cobra.Command) []webui.CommandSpec {
	var out []webui.CommandSpec
	var walk func(c *cobra.Command, path []string)
	walk = func(c *cobra.Command, path []string) {
		for _, sub := range c.Commands() {
			if sub.Hidden || sub.Deprecated != "" {
				continue
			}
			// Cobra's own help and completion commands (added at run time)
			// mean nothing in a browser; the UI has its own help and the
			// autocomplete command covers shells.
			if len(path) == 0 && (sub.Name() == "help" || sub.Name() == "completion") {
				continue
			}
			p := append(append([]string(nil), path...), sub.Name())
			if sub.Runnable() {
				out = append(out, webCommandSpec(sub, p))
			}
			walk(sub, p)
		}
	}
	walk(root, nil)
	return out
}

func webCommandSpec(c *cobra.Command, path []string) webui.CommandSpec {
	key := strings.Join(path, " ")
	spec := webui.CommandSpec{
		Path:        path,
		Use:         c.Use,
		Short:       c.Short,
		Long:        c.Long,
		Example:     c.Example,
		Args:        parseUseArgs(c.Use, path),
		Flags:       []webui.CommandFlag{},
		Destructive: webDestructiveLeaves[path[len(path)-1]] || webDestructivePaths[key],
		Stdin:       webStdinCommands[key],
		Unavailable: webUnavailable[key],
	}
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Deprecated != "" || f.Name == "help" || f.Name == "token" || f.Name == "config-dir" {
			return
		}
		required := false
		if v, ok := f.Annotations[cobra.BashCompOneRequiredFlag]; ok && len(v) > 0 && v[0] == "true" {
			required = true
		}
		def := f.DefValue
		if def == "[]" || (f.Value.Type() == "bool" && def == "false") {
			def = ""
		}
		spec.Flags = append(spec.Flags, webui.CommandFlag{
			Name: f.Name, Shorthand: f.Shorthand, Type: f.Value.Type(), Default: def, Usage: f.Usage,
			Required: required, Sensitive: webSensitiveFlags[f.Name],
		})
	})
	return spec
}

// parseUseArgs turns a usage line ("exec <server> <command>", "tag [<server>
// [key=value...]]", "auto [on|off|status]") into positional argument fields.
// Flag syntax in the usage line ("--unit <name>", "[--all | --group EXPR]") is
// skipped, because flags get their own fields. For alternative forms
// ("a | b") only the first is used; an "extra arguments" box in the UI covers
// the rest.
func parseUseArgs(use string, path []string) []webui.CommandArg {
	fields := splitUseTokens(use)
	if len(fields) <= 1 {
		return []webui.CommandArg{}
	}
	var args []webui.CommandArg
	skipValue := false
	for _, tok := range fields[1:] {
		if tok == "|" {
			break // an alternative form follows
		}
		if skipValue {
			skipValue = false
			// "--unit <name>", "--group EXPR", "--paths /etc/a,/etc/b": the
			// token after a flag is its value, not a positional argument.
			if !strings.HasPrefix(tok, "-") && !strings.HasPrefix(tok, "[") {
				continue
			}
		}
		if strings.HasPrefix(tok, "-") {
			skipValue = true // "--unit <name>": the next token is the flag's value
			continue
		}
		required := true
		inner := tok
		if strings.HasPrefix(tok, "[") && strings.HasSuffix(tok, "]") {
			required = false
			inner = strings.TrimSpace(tok[1 : len(tok)-1])
			if strings.Contains(inner, "flags") {
				continue // "[scope flags]": described by the flag fields
			}
			// "[--all|<server>]": keep the first alternative that is not a
			// flag. "[on|off|status]" (no flags) stays one field with choices.
			if strings.Contains(inner, "-") || strings.Contains(inner, "<") {
				alt := ""
				for _, part := range strings.Split(inner, "|") {
					part = strings.TrimSpace(part)
					if part != "" && !strings.HasPrefix(part, "-") {
						alt = part
						break
					}
				}
				if alt == "" {
					continue // a group of flags only
				}
				inner = alt
			}
			// "[<server> [key=value...]]": a nested optional group.
			if strings.ContainsAny(inner, "<[") {
				if sub := splitUseTokens("x " + inner); len(sub) > 2 {
					for _, a := range parseUseArgs("x "+inner, path) {
						a.Required = false
						args = append(args, a)
					}
					continue
				}
			}
		}
		variadic := false
		if strings.HasSuffix(inner, "...") {
			variadic = true
			inner = strings.TrimSuffix(inner, "...")
		}
		name := inner
		if strings.HasPrefix(name, "<") && strings.HasSuffix(name, ">") && strings.Count(name, "<") == 1 {
			name = name[1 : len(name)-1]
		} else {
			name = strings.NewReplacer("<", "", ">", "").Replace(name)
		}
		if strings.HasSuffix(name, "...") {
			variadic = true
			name = strings.TrimSuffix(name, "...")
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		arg := webui.CommandArg{Name: name, Required: required, Variadic: variadic}
		if strings.Contains(name, "|") && !strings.Contains(name, " ") {
			arg.Suggest = strings.Split(name, "|")
		}
		lower := strings.ToLower(name)
		if strings.Contains(lower, "server") || (lower == "name" && len(path) > 0 && path[0] == "server") {
			arg.Server = true
		}
		args = append(args, arg)
		if variadic {
			break
		}
	}
	if args == nil {
		args = []webui.CommandArg{}
	}
	return args
}

// splitUseTokens splits a usage line on spaces, keeping bracketed groups
// ("[--all | --group EXPR]", "<risky command...>") together.
func splitUseTokens(use string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	for _, r := range use {
		switch r {
		case '[', '<':
			depth++
		case ']', '>':
			if depth > 0 {
				depth--
			}
		}
		if r == ' ' && depth == 0 {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// webConsoleLauncher starts each browser run as `<this binary> <args...>
// --config-dir <dir>`, with the RBAC token this UI was launched with. Before
// starting it, it checks that Cobra resolves the arguments to the requested
// command, so a run can never route into a different command (for example an
// unavailable subcommand).
func webConsoleLauncher(root *cobra.Command, configDir, tokenID string, catalog []webui.CommandSpec) webui.CommandLauncher {
	available := map[string]bool{}
	for _, spec := range catalog {
		if spec.Unavailable == "" {
			available[strings.Join(spec.Path, " ")] = true
		}
	}
	return func(ctx context.Context, args []string) (*exec.Cmd, error) {
		target, _, err := root.Find(args)
		if err != nil {
			return nil, err
		}
		resolved := strings.TrimSpace(strings.TrimPrefix(target.CommandPath(), root.Name()))
		if !available[resolved] {
			return nil, fmt.Errorf("fleet %s cannot run from the web UI", resolved)
		}
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate the fleet binary: %w", err)
		}
		if configDir == "" {
			return nil, errors.New("the controller config dir is unknown")
		}
		// --config-dir goes before any "--" the operator typed, so it is
		// always parsed as the flag.
		full := webInsertFlag(args, "--config-dir", configDir)
		cmd := exec.CommandContext(ctx, exe, full...) // #nosec G204 -- this binary, args checked above
		cmd.Env = webChildEnv(os.Environ(), tokenID)
		cmd.Dir = configDir
		return cmd, nil
	}
}

// webInsertFlag puts flag=value before the first "--" (or at the end).
func webInsertFlag(args []string, flag, value string) []string {
	out := make([]string, 0, len(args)+1)
	inserted := false
	for _, a := range args {
		if a == "--" && !inserted {
			out = append(out, flag+"="+value)
			inserted = true
		}
		out = append(out, a)
	}
	if !inserted {
		out = append(out, flag+"="+value)
	}
	return out
}

// webChildEnv is the parent's environment with the token pinned to the one
// this UI was launched with (or removed), the config dir left to the flag,
// and colour/pager output turned off for a non-terminal.
func webChildEnv(parent []string, tokenID string) []string {
	env := make([]string, 0, len(parent)+5)
	for _, kv := range parent {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "FLEET_TOKEN", "FLEET_CONFIG_DIR", "NO_COLOR", "TERM", "CLICOLOR", "CLICOLOR_FORCE", "FORCE_COLOR", "PAGER":
			continue
		}
		env = append(env, kv)
	}
	if tokenID != "" {
		env = append(env, "FLEET_TOKEN="+tokenID)
	}
	return append(env, "NO_COLOR=1", "CLICOLOR=0", "TERM=dumb", "PAGER=cat", "FLEET_WEBUI_CHILD=1")
}

// webTerminalLauncher starts a browser terminal session as `<this binary> ssh
// --config-dir <dir> -- <server>`, with the token this UI was launched with,
// so the session is exactly `fleet ssh` (RBAC, the command-policy refusal,
// host-key pinning and audit included). The web UI has already checked that
// the server exists; "--" keeps the name from ever being read as a flag.
func webTerminalLauncher(configDir, tokenID string) webui.TerminalLauncher {
	return func(ctx context.Context, server string) (*exec.Cmd, error) {
		if server == "" || strings.HasPrefix(server, "-") || strings.ContainsAny(server, "\x00\n\r") {
			return nil, errors.New("invalid server name")
		}
		if configDir == "" {
			return nil, errors.New("the controller config dir is unknown")
		}
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate the fleet binary: %w", err)
		}
		cmd := exec.CommandContext(ctx, exe, "ssh", "--config-dir="+configDir, "--", server) // #nosec G204 -- this binary; server checked against the server list
		cmd.Env = webTerminalEnv(os.Environ(), tokenID)
		cmd.Dir = configDir
		return cmd, nil
	}
}

// webTerminalEnv is the parent's environment with the token pinned (or
// removed) and a colour-capable terminal type, as a real terminal would have.
func webTerminalEnv(parent []string, tokenID string) []string {
	env := make([]string, 0, len(parent)+4)
	for _, kv := range parent {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "FLEET_TOKEN", "FLEET_CONFIG_DIR", "NO_COLOR", "TERM", "COLORTERM", "CLICOLOR", "CLICOLOR_FORCE", "FORCE_COLOR":
			continue
		}
		env = append(env, kv)
	}
	if tokenID != "" {
		env = append(env, "FLEET_TOKEN="+tokenID)
	}
	return append(env, "TERM=xterm-256color", "COLORTERM=truecolor", "FLEET_WEBUI_CHILD=1")
}
