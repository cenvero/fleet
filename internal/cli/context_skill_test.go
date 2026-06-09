// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runFleet(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRootCommand()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	// Pin an isolated, uninitialized config dir so these tests never depend on
	// (or pollute) the machine's real ~/.cenvero-fleet — and so they verify that
	// context/skill work pre-init.
	root.SetArgs(append([]string{"--config-dir", t.TempDir()}, args...))
	err := root.Execute()
	return buf.String(), err
}

func TestContextMarkdownIncludesCommandsAndConcepts(t *testing.T) {
	out, err := runFleet(t, "context")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	for _, want := range []string{
		"# Cenvero Fleet — Agent Context",
		"## Concepts",
		"### `fleet file`",
		"### `fleet server`",
		"`fleet context`",
		"fleet file ui",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("context output missing %q", want)
		}
	}
}

func TestContextJSONIsValidAndHasFileGroup(t *testing.T) {
	out, err := runFleet(t, "context", "--json")
	if err != nil {
		t.Fatalf("context --json: %v", err)
	}
	var doc struct {
		Binary   string `json:"binary"`
		Commands []struct {
			Path string `json:"path"`
		} `json:"commands"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if doc.Binary == "" || len(doc.Commands) == 0 {
		t.Fatalf("unexpected context JSON: %+v", doc)
	}
	found := false
	for _, c := range doc.Commands {
		if c.Path == "fleet file" {
			found = true
		}
	}
	if !found {
		t.Fatalf("context JSON missing the file group")
	}
}

func TestAICommandPerCommandHelp(t *testing.T) {
	// Markdown for a single command includes its usage, full Long help, and flags.
	out, err := runFleet(t, "ai", "file", "upload")
	if err != nil {
		t.Fatalf("ai file upload: %v", err)
	}
	for _, want := range []string{"`fleet file upload`", "Usage:", "resumable", "--parallel"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ai file upload missing %q", want)
		}
	}

	// JSON for a single command node.
	jout, err := runFleet(t, "ai", "sync", "--json")
	if err != nil {
		t.Fatalf("ai sync --json: %v", err)
	}
	var node struct {
		Path  string `json:"path"`
		Usage string `json:"usage"`
		Long  string `json:"long"`
	}
	if err := json.Unmarshal([]byte(jout), &node); err != nil {
		t.Fatalf("ai sync --json invalid: %v", err)
	}
	if node.Path != "fleet sync" || node.Long == "" {
		t.Fatalf("unexpected ai node: %+v", node)
	}

	// Unknown command errors.
	if _, err := runFleet(t, "ai", "definitely-not-a-command"); err == nil {
		t.Fatalf("expected error for unknown command")
	}
}

func TestSkillClaudeInstall(t *testing.T) {
	dir := t.TempDir()
	if _, err := runFleet(t, "skill", "claude", "--dir", dir); err != nil {
		t.Fatalf("skill claude: %v", err)
	}
	skill, err := os.ReadFile(filepath.Join(dir, "skills", "cenvero-fleet", "SKILL.md"))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	s := string(skill)
	if !strings.HasPrefix(s, "---\nname: cenvero-fleet\n") {
		t.Fatalf("SKILL.md missing frontmatter: %q", s[:min(40, len(s))])
	}
	if !strings.Contains(s, "fleet context") {
		t.Fatalf("SKILL.md should tell the agent to run fleet context")
	}
	cmdMD, err := os.ReadFile(filepath.Join(dir, "commands", "fleet.md"))
	if err != nil {
		t.Fatalf("slash command not written: %v", err)
	}
	if !strings.Contains(string(cmdMD), "fleet context") || !strings.Contains(string(cmdMD), "$ARGUMENTS") {
		t.Fatalf("/fleet command should run fleet context and accept arguments")
	}

	// Re-installing just overrides the existing files (idempotent, no --force).
	out, err := runFleet(t, "skill", "claude", "--dir", dir)
	if err != nil {
		t.Fatalf("re-install should succeed by overriding: %v", err)
	}
	if !strings.Contains(out, "/fleet") {
		t.Fatalf("re-install should print the /fleet install message, got: %q", out)
	}
}

func TestSkillAgentsAppendIdempotent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(target, []byte("# Existing\n\nnotes\n"), 0o644); err != nil {
		t.Fatalf("seed AGENTS.md: %v", err)
	}
	if _, err := runFleet(t, "skill", "agents", "--dir", dir); err != nil {
		t.Fatalf("skill agents: %v", err)
	}
	first, _ := os.ReadFile(target)
	if !strings.Contains(string(first), "# Existing") || !strings.Contains(string(first), "Cenvero Fleet") {
		t.Fatalf("append did not preserve existing + add fleet section")
	}
	// Second run must not duplicate the section.
	if _, err := runFleet(t, "skill", "agents", "--dir", dir); err != nil {
		t.Fatalf("skill agents (2nd): %v", err)
	}
	second, _ := os.ReadFile(target)
	if strings.Count(string(second), agentsMarker) != 1 {
		t.Fatalf("fleet section duplicated on second run")
	}
}
