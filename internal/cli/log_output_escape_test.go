// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
	"github.com/spf13/cobra"
)

// A log line is text anyone who can reach the logged service controls (an
// HTTP request path, a user name). Printed to the operator's terminal it must
// not be able to drive it: set the clipboard (OSC 52), retitle the window,
// erase or rewrite lines.
const hostileLogLine = "GET /\x1b]52;c;Y3VybCBldmlsfHNo\x07 \r\x1b[2Kok"

func TestLogOutputNeutralisesControlSequences(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	result := proto.LogReadResult{Lines: []proto.LogLine{{Number: 7, Text: hostileLogLine}, {Number: 8, Text: "plain\tline"}}}
	if err := writeLogOutput(cmd, result, ""); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.ContainsAny(got, "\x1b\x07\r") {
		t.Fatalf("log output passes control characters to the terminal: %q", got)
	}
	if !strings.Contains(got, `GET /\x1b]52;`) || !strings.Contains(got, "     8  plain\tline\n") {
		t.Fatalf("log output not rendered as expected: %q", got)
	}

	// An --export file keeps the log's bytes exactly.
	export := filepath.Join(t.TempDir(), "out.log")
	if err := writeLogOutput(cmd, result, export); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(export)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), hostileLogLine) {
		t.Fatalf("export changed the log bytes: %q", data)
	}
}

func TestJournalLinesNeutraliseControlSequences(t *testing.T) {
	var out bytes.Buffer
	printJournalLines(&out, []string{hostileLogLine}, "")
	if strings.ContainsAny(out.String(), "\x1b\x07\r") {
		t.Fatalf("journal output passes control characters to the terminal: %q", out.String())
	}
}
