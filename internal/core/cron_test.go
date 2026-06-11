// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"strings"
	"testing"
)

// TestCronWriteCommandNormalContent confirms ordinary crontab content produces a
// quoted-heredoc write whose body is passed through verbatim.
func TestCronWriteCommandNormalContent(t *testing.T) {
	content := UpsertManagedCron("", CronJob{
		Name:     "backup",
		Schedule: "0 3 * * *",
		Command:  "/usr/bin/backup --now $HOME",
	})
	cmd := CronWriteCommand(content)
	if !strings.Contains(cmd, "crontab - <<'"+cronHeredocDelim+"'") {
		t.Fatalf("expected a quoted heredoc with the fleet delimiter, got:\n%s", cmd)
	}
	// The command line (including the literal $HOME) must survive verbatim inside
	// the heredoc body.
	if !strings.Contains(cmd, "/usr/bin/backup --now $HOME") {
		t.Fatalf("heredoc body did not preserve the command verbatim:\n%s", cmd)
	}
	// The body must be terminated by the delimiter on its own line.
	if !strings.Contains(cmd, "\n"+cronHeredocDelim+"\n") {
		t.Fatalf("heredoc not terminated by the delimiter line:\n%s", cmd)
	}
}

// TestCronWriteCommandRejectsDelimiterLine is the hardening regression: content
// containing a line equal to the heredoc delimiter must NOT yield a heredoc that
// can be broken out of. Instead CronWriteCommand emits a remote command that
// fails (non-zero exit) and writes nothing.
func TestCronWriteCommandRejectsDelimiterLine(t *testing.T) {
	// A crafted crontab whose preserved (non-fleet) content includes a bare line
	// equal to the delimiter, plus a payload that would run if the heredoc broke.
	malicious := "0 0 * * * /bin/true\n" + cronHeredocDelim + "\nrm -rf /tmp/should-not-run\n"

	if !ContentBreaksCronHeredoc(malicious) {
		t.Fatal("ContentBreaksCronHeredoc should detect the delimiter line")
	}

	cmd := CronWriteCommand(malicious)
	if strings.Contains(cmd, "crontab - <<'") {
		t.Fatalf("CronWriteCommand emitted a breakable heredoc for delimiter-laden content:\n%s", cmd)
	}
	if !strings.Contains(cmd, "exit 1") {
		t.Fatalf("CronWriteCommand should fail loudly (exit 1) for delimiter-laden content, got:\n%s", cmd)
	}
	// The injected payload must NOT appear in a position where the shell would run
	// it (the refusal command must not embed it as code).
	if strings.Contains(cmd, "rm -rf /tmp/should-not-run") {
		t.Fatalf("refusal command leaked the injected payload:\n%s", cmd)
	}
}

// TestContentBreaksCronHeredocTrailingCR confirms a delimiter line with a
// trailing carriage return (CRLF content) is still detected.
func TestContentBreaksCronHeredocTrailingCR(t *testing.T) {
	if !ContentBreaksCronHeredoc("a\n" + cronHeredocDelim + "\r\nb") {
		t.Fatal("delimiter line with trailing CR should be detected")
	}
	if ContentBreaksCronHeredoc("0 0 * * * echo " + cronHeredocDelim + "-suffix") {
		t.Fatal("a line merely CONTAINING the delimiter as a substring must not be rejected")
	}
}
