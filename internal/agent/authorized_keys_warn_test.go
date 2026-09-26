// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `fleet-agent serve` warns at start-up when no controller could connect yet,
// instead of listening silently and refusing every connection.
func TestWarnAuthorizedKeys(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("# no keys yet\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{missing: "does not exist", empty: "holds no valid keys"} {
		var buf bytes.Buffer
		warnAuthorizedKeys(&buf, path)
		if !strings.Contains(buf.String(), want) {
			t.Errorf("%s: warning = %q, want %q", filepath.Base(path), buf.String(), want)
		}
	}
	var buf bytes.Buffer
	warnAuthorizedKeys(&buf, "")
	if buf.Len() != 0 {
		t.Errorf("no path: unexpected warning %q", buf.String())
	}
}

func TestAgentVersionFlag(t *testing.T) {
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "agent ") {
		t.Fatalf("--version printed %q", out.String())
	}
}
