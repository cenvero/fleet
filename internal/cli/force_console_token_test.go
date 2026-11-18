// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// The agent-lockout override is for a person at the server's console. A
// token (here even an unscoped one) is refused it before anything runs, so
// an AI agent or script holding a credential cannot skip the check.
func TestForceIHaveConsoleRefusedUnderAToken(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	tok, err := core.NewTokenStore(configDir).Create(core.Token{Name: "agent", DestructiveAllowed: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"firewall", "enable", "web", "--force-i-have-console"},
		{"firewall", "add", "web", "deny 2222/tcp", "--force-i-have-console"},
		{"port", "close", "web", "2222", "--force-i-have-console"},
		{"fw", "enable", "web", "--safe", "--force-i-have-console"},
	} {
		cmd := exec.Command(os.Args[0], append([]string{"--config-dir", configDir}, args...)...) // #nosec G204 -- the test binary itself
		cmd.Env = append(os.Environ(), "FLEET_TEST_CLI_MAIN=1", "FLEET_TOKEN="+tok.ID)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "denied: --force-i-have-console cannot be used with a token") {
			t.Fatalf("%v under a token: err=%v\n%s", args, err, out)
		}
	}
}

// The web console's Control view never offers or accepts the override.
func TestForceIHaveConsoleNotAvailableFromTheWebUI(t *testing.T) {
	root := NewRootCommand()
	catalog := webConsoleCatalog(root)
	for _, spec := range catalog {
		for _, f := range spec.Flags {
			if f.Name == webTerminalOnlyFlag {
				t.Fatalf("fleet %s offers --%s in the browser", strings.Join(spec.Path, " "), webTerminalOnlyFlag)
			}
		}
	}
	launch := webConsoleLauncher(root, t.TempDir(), "", catalog)
	for _, args := range [][]string{{"web", "--force-i-have-console"}, {"web", "--force-i-have-console=true"}} {
		if _, err := launch(context.Background(), []string{"firewall", "enable"}, args); err == nil || !strings.Contains(err.Error(), "cannot be used from the web UI") {
			t.Fatalf("%v: err = %v, want refused", args, err)
		}
	}
	if _, err := launch(context.Background(), []string{"firewall", "enable"}, []string{"web"}); err != nil {
		t.Fatalf("plain firewall enable refused: %v", err)
	}
}

// The SSH login password never goes on a command line from the browser: the
// launcher refuses --login-password, and server add reads it from stdin
// with --login-password-stdin.
func TestLoginPasswordOnlyThroughStdinFromTheWebUI(t *testing.T) {
	root := NewRootCommand()
	catalog := webConsoleCatalog(root)
	launch := webConsoleLauncher(root, t.TempDir(), "", catalog)
	for _, args := range [][]string{{"web", "10.0.0.5", "--login-password", "hunter2"}, {"web", "10.0.0.5", "--login-password=hunter2"}} {
		if _, err := launch(context.Background(), []string{"server", "add"}, args); err == nil || !strings.Contains(err.Error(), "--login-password-stdin") {
			t.Fatalf("%v: err = %v, want refused", args, err)
		}
	}
	for _, spec := range catalog {
		if strings.Join(spec.Path, " ") == "server add" && !spec.Stdin {
			t.Fatal("server add does not take stdin in the web UI")
		}
	}
	cmd := &cobra.Command{}
	cmd.Flags().Bool("login-password-stdin", true, "")
	_ = cmd.Flags().Set("login-password-stdin", "true")
	cmd.SetIn(strings.NewReader("s3cret pass\nignored\n"))
	if pw, err := loginPasswordFromStdin(cmd, ""); err != nil || pw != "s3cret pass" {
		t.Fatalf("stdin password = %q, %v", pw, err)
	}
	if _, err := loginPasswordFromStdin(cmd, "also-a-flag"); err == nil {
		t.Fatal("both --login-password and --login-password-stdin accepted")
	}
}

// A reverse-mode add prints a join secret, so the browser cannot start one.
func TestReverseServerAddRefusedFromTheWebUI(t *testing.T) {
	root := NewRootCommand()
	catalog := webConsoleCatalog(root)
	launch := webConsoleLauncher(root, t.TempDir(), "", catalog)
	for _, args := range [][]string{{"edge", "x", "--mode=reverse"}, {"edge", "x", "--mode", "reverse"}, {"edge", "x", "--mode=per-node"}} {
		if _, err := launch(context.Background(), []string{"server", "add"}, args); err == nil || !strings.Contains(err.Error(), "terminal") {
			t.Fatalf("%v: err = %v, want refused", args, err)
		}
	}
	if _, err := launch(context.Background(), []string{"server", "add"}, []string{"web", "10.0.0.5", "--mode=direct", "--no-agent"}); err != nil {
		t.Fatalf("direct add refused: %v", err)
	}
}

// The login password is read without buffering past its line, so the answers
// to later prompts can follow it on stdin.
func TestLoginPasswordLineLeavesTheRestOfStdin(t *testing.T) {
	r := strings.NewReader("s3cret\nyes\n")
	pw, err := readStdinLine(r, 4096)
	if err != nil || pw != "s3cret" {
		t.Fatalf("readStdinLine = %q, %v", pw, err)
	}
	rest, _ := io.ReadAll(r)
	if string(rest) != "yes\n" {
		t.Fatalf("left on stdin: %q, want %q", rest, "yes\n")
	}
	if _, err := readStdinLine(strings.NewReader(strings.Repeat("x", 5000)), 4096); err == nil {
		t.Fatal("an over-long line was accepted")
	}
	if pw, err := readStdinLine(strings.NewReader("last"), 4096); err != nil || pw != "last" {
		t.Fatalf("a line without a newline = %q, %v", pw, err)
	}
}
