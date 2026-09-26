// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bytes"
	"strings"
	"testing"
)

// A mistyped subcommand of a command group must fail with exit 1 and a
// suggestion. Cobra otherwise prints the group's help and exits 0, so a typo
// in a script (`fleet server lst`) looked like success.
func TestUnknownSubcommandOfGroupExitsNonZero(t *testing.T) {
	exitCode := -1
	orig := exitProcess
	exitProcess = func(code int) { exitCode = code }
	defer func() { exitProcess = orig }()

	root := NewRootCommand()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--config-dir", t.TempDir(), "server", "lst"})
	_ = root.Execute()

	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr: %q)", exitCode, stderr.String())
	}
	errOut := stderr.String()
	for _, want := range []string{`unknown command "lst" for "fleet server"`, "Did you mean this?\n\tlist", "Run 'fleet server --help' for usage."} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(stdout.String(), "Available Commands") {
		t.Errorf("help was printed for an unknown subcommand:\n%s", stdout.String())
	}
}

// The bare group, `--help` and `help <group>` keep showing the help normally.
func TestGroupHelpStillWorks(t *testing.T) {
	exitCode := -1
	orig := exitProcess
	exitProcess = func(code int) { exitCode = code }
	defer func() { exitProcess = orig }()

	for _, args := range [][]string{{"server"}, {"server", "--help"}, {"help", "server"}} {
		exitCode = -1
		root := NewRootCommand()
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		root.SetArgs(append([]string{"--config-dir", t.TempDir()}, args...))
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if exitCode != -1 {
			t.Fatalf("%v: exited with %d", args, exitCode)
		}
		if !strings.Contains(stdout.String(), "Available Commands") {
			t.Fatalf("%v: help not printed:\n%s%s", args, stdout.String(), stderr.String())
		}
	}
}

// Groups that also run on their own (`approvals`, `alerts`) used to ignore a
// stray word; it is now an error, reported once and without the usage block.
func TestRunnableGroupRejectsUnknownSubcommand(t *testing.T) {
	for _, group := range []string{"approvals", "alerts"} {
		root := NewRootCommand()
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		root.SetArgs([]string{"--config-dir", t.TempDir(), group, "bogus"})
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), `unknown command "bogus" for "fleet `+group+`"`) {
			t.Fatalf("%s bogus: err = %v", group, err)
		}
		out := stdout.String() + stderr.String()
		if n := strings.Count(out, "unknown command"); n != 1 {
			t.Errorf("%s bogus: error printed %d times:\n%s", group, n, out)
		}
		if strings.Contains(out, "Usage:") {
			t.Errorf("%s bogus: usage block printed:\n%s", group, out)
		}
	}
}
