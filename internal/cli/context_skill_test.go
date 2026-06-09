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
		"fleet ui",
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
	if _, err := os.Stat(filepath.Join(dir, "commands", "fleet.md")); err != nil {
		t.Fatalf("slash command not written: %v", err)
	}

	// Re-install without --force must fail; with --force must succeed.
	if _, err := runFleet(t, "skill", "claude", "--dir", dir); err == nil {
		t.Fatalf("expected error re-installing without --force")
	}
	if _, err := runFleet(t, "skill", "claude", "--dir", dir, "--force"); err != nil {
		t.Fatalf("force reinstall failed: %v", err)
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
