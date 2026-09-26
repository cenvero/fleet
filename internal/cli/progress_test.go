// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// TestProgressGoesToStderrAsJSONLines is the regression for transfer progress
// being printed on stdout (mixed into what scripts parse) as plain text, while
// the help promised periodic JSON.
func TestProgressGoesToStderrAsJSONLines(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	report, finish := newProgressReporter(cmd, "upload")
	report(core.ProgressUpdate{BytesDone: 10, TotalBytes: 100, RatePerSec: 5, ActiveStreams: 2})
	report(core.ProgressUpdate{BytesDone: 20, TotalBytes: 100}) // throttled: within a second of the last line
	report(core.ProgressUpdate{BytesDone: 100, TotalBytes: 100, Done: true})
	finish()

	if stdout.Len() != 0 {
		t.Fatalf("progress leaked onto stdout: %q", stdout.String())
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want the first update and the final one, got %d lines:\n%s", len(lines), stderr.String())
	}
	var first, last map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("progress line is not JSON: %q: %v", lines[0], err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &last); err != nil {
		t.Fatalf("progress line is not JSON: %q: %v", lines[1], err)
	}
	if first["op"] != "upload" || first["percent"] != float64(10) || first["bytes_done"] != float64(10) ||
		first["total_bytes"] != float64(100) || first["active_streams"] != float64(2) || first["done"] != false {
		t.Fatalf("first progress line = %v", first)
	}
	if last["percent"] != float64(100) || last["done"] != true {
		t.Fatalf("final progress line = %v", last)
	}
}

func TestProgressBarOnTerminal(t *testing.T) {
	var out bytes.Buffer
	report, finish := newProgressWriter(&out, "copy", true)
	report(core.ProgressUpdate{BytesDone: 50, TotalBytes: 100, Done: true})
	finish()
	if got := out.String(); !strings.HasPrefix(got, "\rcopy [") || !strings.Contains(got, " 50%") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("terminal progress = %q", got)
	}
}
