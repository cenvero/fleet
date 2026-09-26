// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/cenvero/fleet/internal/core"
	tea "github.com/charmbracelet/bubbletea"
)

// Dashboard actions never mutate controller state in-process. Each one runs the
// SAME fleet binary as a child process with the dashboard's --config-dir and
// RBAC token, so every existing gate applies exactly as on the command line:
// token scope checks, cmd-policy (e.g. `fleet ssh` is refused while a
// cmd-policy is configured), host-key pinning (reconnect never passes
// --accept-new-host-key), and audit attribution. There is deliberately no
// free-form command entry: the argv is always one of the fixed shapes below.

// dashActionTimeout bounds a non-interactive action.
const dashActionTimeout = 2 * time.Minute

// dashAction is one fixed CLI invocation.
type dashAction struct {
	label       string   // present-progressive description, e.g. "acknowledging alert x"
	done        string   // past-tense flash, e.g. "acknowledged alert x"
	args        []string // CLI args after the global flags
	interactive bool     // suspend the TUI and hand the child the terminal
}

type dashChoice struct {
	key   string
	label string
	arg   string
}

// dashPrompt is a pending confirmation shown in the footer.
type dashPrompt struct {
	question string
	action   dashAction
	choices  []dashChoice // optional (suppress duration)
	def      int
	// argAt is the index in action.args replaced by the chosen choice's arg.
	argAt int
}

// dashActionMsg reports a finished action.
type dashActionMsg struct {
	action dashAction
	err    error
	output string
}

var dashSuppressChoices = []dashChoice{
	{key: "1", label: "1h", arg: "1h0m0s"},
	{key: "2", label: "6h", arg: "6h0m0s"},
	{key: "3", label: "24h", arg: "24h0m0s"},
	{key: "4", label: "7d", arg: "168h0m0s"},
}

// actionKey maps an action key on the active tab to a prompt or a command.
func (m *model) actionKey(key string) (tea.Cmd, bool) {
	switch key {
	case "s", "f":
		server := m.contextServer()
		if server == "" {
			return nil, false
		}
		if key == "s" {
			return m.runAction(dashAction{
				label: "opening shell on " + server, done: "shell on " + server + " closed",
				args: []string{"ssh", "--", server}, interactive: true,
			}), true
		}
		return m.runAction(dashAction{
			label: "opening file manager on " + server, done: "file manager closed",
			args: []string{"files", "--", server}, interactive: true,
		}), true
	case "c":
		if m.activeTab != tabServers {
			return nil, false
		}
		if s, _ := m.selectedServer(); s != nil {
			m.prompt = &dashPrompt{
				question: "Reconnect " + s.Name + " (re-probe the agent; a changed host key is refused)?",
				action: dashAction{label: "reconnecting " + s.Name, done: "reconnected " + s.Name,
					args: []string{"server", "reconnect", "--", s.Name}},
			}
			return nil, true
		}
	case "m":
		if m.activeTab != tabServers {
			return nil, false
		}
		if s, _ := m.selectedServer(); s != nil {
			return m.runAction(dashAction{label: "collecting metrics from " + s.Name, done: "collected metrics from " + s.Name,
				args: []string{"server", "metrics", "--", s.Name}}), true
		}
	case "R":
		if m.activeTab != tabServices {
			return nil, false
		}
		if r := m.selectedService(); r != nil {
			m.prompt = &dashPrompt{
				question: "Restart " + r.Service.Name + " on " + r.Server.Name + "?",
				action: dashAction{label: "restarting " + r.Service.Name + " on " + r.Server.Name,
					done: "restarted " + r.Service.Name + " on " + r.Server.Name,
					args: []string{"service", "restart", "--", r.Server.Name, r.Service.Name}},
			}
			return nil, true
		}
	case "L":
		server, service := "", ""
		switch m.activeTab {
		case tabServices:
			if r := m.selectedService(); r != nil {
				server, service = r.Server.Name, r.Service.Name
			}
		case tabLogs:
			if lp := m.selectedLog(); lp != nil {
				server, service = lp.Server, lp.Service
			}
		}
		if server == "" {
			return nil, false
		}
		return m.runAction(dashAction{
			label: "following " + service + " on " + server, done: "stopped following " + service,
			args: []string{"service", "logs", "--follow", "--", server, service}, interactive: true,
		}), true
	case "a", "z", "u":
		if m.activeTab != tabAlerts {
			return nil, false
		}
		a := m.selectedAlert()
		if a == nil {
			return nil, false
		}
		where := ""
		if a.Server != "" {
			where = " on " + a.Server
		}
		switch key {
		case "a":
			if a.AcknowledgedAt != nil {
				m.setFlash("alert "+a.ID+" is already acknowledged", false)
				return nil, true
			}
			m.prompt = &dashPrompt{
				question: "Acknowledge " + string(a.Severity) + " alert " + a.ID + where + "?",
				action: dashAction{label: "acknowledging " + a.ID, done: "acknowledged " + a.ID,
					args: []string{"alerts", "ack", "--", a.ID}},
			}
		case "z":
			m.prompt = &dashPrompt{
				question: "Suppress notifications for " + a.ID + where + " for",
				action: dashAction{label: "suppressing " + a.ID, done: "suppressed " + a.ID,
					args: []string{"alerts", "suppress", "--for", "", "--", a.ID}},
				choices: dashSuppressChoices, def: 1, argAt: 3,
			}
		case "u":
			if core.AlertState(*a, m.clock()) != "suppressed" {
				m.setFlash("alert "+a.ID+" is not suppressed", false)
				return nil, true
			}
			m.prompt = &dashPrompt{
				question: "Lift the suppression on " + a.ID + where + "?",
				action: dashAction{label: "unsuppressing " + a.ID, done: "unsuppressed " + a.ID,
					args: []string{"alerts", "unsuppress", "--", a.ID}},
			}
		}
		return nil, true
	}
	return nil, false
}

// contextServer is the server the selected row refers to, on any tab.
func (m *model) contextServer() string {
	switch m.activeTab {
	case tabServers:
		if s, _ := m.selectedServer(); s != nil {
			return s.Name
		}
	case tabServices:
		if r := m.selectedService(); r != nil {
			return r.Server.Name
		}
	case tabLogs:
		if lp := m.selectedLog(); lp != nil {
			return lp.Server
		}
	case tabAlerts:
		if a := m.selectedAlert(); a != nil {
			return a.Server
		}
	}
	return ""
}

func (m *model) handlePromptKey(key string) tea.Cmd {
	p := m.prompt
	switch key {
	case "esc", "n", "N", "q":
		m.prompt = nil
		m.setFlash("cancelled", false)
		return nil
	case "y", "Y", "enter":
		m.prompt = nil
		return m.runAction(p.withChoice(p.def))
	}
	for i, c := range p.choices {
		if c.key == key {
			m.prompt = nil
			return m.runAction(p.withChoice(i))
		}
	}
	return nil
}

func (p *dashPrompt) withChoice(i int) dashAction {
	a := p.action
	if len(p.choices) == 0 {
		return a
	}
	i = clamp(i, 0, len(p.choices)-1)
	a.args = append([]string(nil), a.args...)
	a.args[p.argAt] = p.choices[i].arg
	a.done += " for " + p.choices[i].label
	return a
}

// childArgv builds the argv for the child fleet process.
func (rt *dashRuntime) childArgv(args []string) []string {
	argv := make([]string, 0, len(args)+2)
	if rt.configDir != "" {
		argv = append(argv, "--config-dir", rt.configDir)
	}
	return append(argv, args...)
}

// childEnv is the parent environment with FLEET_TOKEN pinned to the token the
// dashboard was started with (the token is passed through the environment, not
// argv, so it does not show up in process listings).
func (rt *dashRuntime) childEnv() []string {
	environ := os.Environ
	if rt.environ != nil {
		environ = rt.environ
	}
	base := environ()
	env := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if rt.token != "" && strings.HasPrefix(kv, "FLEET_TOKEN=") {
			continue
		}
		env = append(env, kv)
	}
	if rt.token != "" {
		env = append(env, "FLEET_TOKEN="+rt.token)
	}
	return env
}

// command builds the child process for an interactive action (no timeout: the
// operator ends the session).
func (rt *dashRuntime) command(args []string) (*exec.Cmd, error) {
	if rt.exe == "" {
		return nil, errors.New("cannot locate the fleet executable")
	}
	cmd := exec.Command(rt.exe, rt.childArgv(args)...) // #nosec G204 -- our own binary with a fixed subcommand argv; names are passed after "--", no shell
	cmd.Env = rt.childEnv()
	return cmd, nil
}

// commandContext builds the child process for a background action.
func (rt *dashRuntime) commandContext(ctx context.Context, args []string) (*exec.Cmd, error) {
	if rt.exe == "" {
		return nil, errors.New("cannot locate the fleet executable")
	}
	cmd := exec.CommandContext(ctx, rt.exe, rt.childArgv(args)...) // #nosec G204 -- our own binary with a fixed subcommand argv; names are passed after "--", no shell
	cmd.Env = rt.childEnv()
	return cmd, nil
}

func (m *model) runAction(a dashAction) tea.Cmd {
	if m.rt == nil {
		return nil
	}
	if m.busy != "" && !a.interactive {
		m.setFlash("still "+m.busy+" — try again when it finishes", true)
		return nil
	}
	rt := m.rt
	if a.interactive {
		cmd, err := rt.command(a.args)
		if err != nil {
			m.setFlash(err.Error(), true)
			return nil
		}
		tail := &dashTailBuffer{max: 4096}
		cmd.Stderr = io.MultiWriter(os.Stderr, tail)
		return tea.ExecProcess(cmd, func(err error) tea.Msg {
			return dashActionMsg{action: a, err: err, output: tail.String()}
		})
	}
	m.busy = a.label
	m.setFlash(a.label+"…", false)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), dashActionTimeout)
		defer cancel()
		cmd, err := rt.commandContext(ctx, a.args)
		if err != nil {
			return dashActionMsg{action: a, err: err}
		}
		out := &dashTailBuffer{max: 16 * 1024}
		cmd.Stdout, cmd.Stderr = out, out
		cmd.Stdin = nil // never block on a prompt: the CLI fails closed without a TTY
		err = cmd.Run()
		return dashActionMsg{action: a, err: err, output: out.String()}
	}
}

func (m *model) applyAction(msg dashActionMsg) tea.Cmd {
	if !msg.action.interactive {
		m.busy = ""
	}
	if msg.err != nil {
		detail := dashLastLine(msg.output)
		if detail == "" {
			detail = msg.err.Error()
		}
		what := strings.TrimSuffix(msg.action.label, "…")
		m.setFlash(what+" failed: "+detail, true)
	} else {
		m.setFlash("✓ "+msg.action.done, false)
	}
	// Reflect the change (and anything else that moved while we were away).
	m.lastStart = time.Time{}
	return m.startRefresh()
}

// lastLine returns the last non-empty line of CLI output, without the
// "Error: " prefix cobra adds.
func dashLastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		return strings.TrimPrefix(l, "Error: ")
	}
	return ""
}

// tailBuffer keeps the last max bytes written to it.
type dashTailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (t *dashTailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if len(p) > t.max {
		p = p[len(p)-t.max:]
	}
	if t.buf.Len()+len(p) > t.max {
		keep := t.buf.Bytes()[t.buf.Len()+len(p)-t.max:]
		rest := append([]byte(nil), keep...)
		t.buf.Reset()
		t.buf.Write(rest)
	}
	t.buf.Write(p)
	return n, nil
}

func (t *dashTailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}
