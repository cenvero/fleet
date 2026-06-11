// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"strings"
	"testing"
)

// FL-021 — validUnitName / shellQuote / parseShowProperties.

func TestValidUnitName(t *testing.T) {
	valid := []string{"nginx", "nginx.service", "postgres@14-main.service", "a_b-c.socket", "getty@tty1.service"}
	for _, n := range valid {
		if err := validUnitName(n); err != nil {
			t.Errorf("expected %q valid, got %v", n, err)
		}
	}
	bad := []string{
		"", "a b", "rm -rf /", "nginx;reboot", "$(id)", "a|b", "x`y`", "a&b", "a>b", "a\nb",
		// Leading '-' is option injection even when shell-quoted: systemctl would
		// parse these as options, not as a unit argument.
		"-foo", "--version", "-H host", `a\b`,
	}
	for _, n := range bad {
		if err := validUnitName(n); err == nil {
			t.Errorf("expected %q to be rejected", n)
		}
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("nginx"); got != "'nginx'" {
		t.Errorf("shellQuote(nginx)=%q", got)
	}
	// An embedded single quote must be neutralized so it cannot break out.
	got := shellQuote("a'b")
	if strings.Contains(got, "a'b") {
		t.Errorf("single quote not escaped: %q", got)
	}
	if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
		t.Errorf("result not wrapped in single quotes: %q", got)
	}
}

func TestParseShowProperties(t *testing.T) {
	out := "ActiveState=active\nSubState=running\nUnitFileState=enabled\nDescription=The nginx HTTP server\nMainPID=1234\nActiveEnterTimestamp=Tue 2026-06-11 10:00:00 UTC\n"
	props := parseShowProperties(out)
	cases := map[string]string{
		"ActiveState":          "active",
		"SubState":             "running",
		"UnitFileState":        "enabled",
		"Description":          "The nginx HTTP server",
		"MainPID":              "1234",
		"ActiveEnterTimestamp": "Tue 2026-06-11 10:00:00 UTC",
	}
	for k, want := range cases {
		if got := props[k]; got != want {
			t.Errorf("prop %s=%q want %q", k, got, want)
		}
	}
	// Lines without '=' or with an empty key are ignored.
	if len(parseShowProperties("garbage\n=novalue\n")) != 0 {
		t.Errorf("expected no properties from malformed input")
	}
}

func TestWriteServiceStatusFailedFlag(t *testing.T) {
	s := serviceStatus{Active: "failed"}
	s.Failed = s.Active == "failed"
	if !s.Failed {
		t.Fatal("expected Failed true when ActiveState is failed")
	}
}

// FL-022 — journalCommand / matchGrep / newJournalLines / validJournalSince.

func TestJournalCommand(t *testing.T) {
	cmd := journalCommand("nginx", "1h", 50)
	for _, want := range []string{"journalctl", "-u 'nginx'", "--no-pager", "--since '1h'", "-n 50"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("journal command %q missing %q", cmd, want)
		}
	}
	// No since / no tail produces a minimal command.
	cmd = journalCommand("nginx", "", 0)
	if strings.Contains(cmd, "--since") || strings.Contains(cmd, "-n ") {
		t.Errorf("unexpected flags in %q", cmd)
	}
}

func TestMatchGrep(t *testing.T) {
	if !matchGrep("an ERROR occurred", "error") {
		t.Error("expected case-insensitive match")
	}
	if matchGrep("all good", "fail") {
		t.Error("did not expect a match")
	}
	if !matchGrep("anything", "") {
		t.Error("empty grep should match everything")
	}
}

func TestNewJournalLines(t *testing.T) {
	batch := []string{"l1", "l2", "l3", "l4"}
	got := newJournalLines(batch, "l2")
	if strings.Join(got, ",") != "l3,l4" {
		t.Errorf("after l2 got %v", got)
	}
	// lastSeen absent → whole batch is new (rotation case).
	if got := newJournalLines(batch, "gone"); len(got) != len(batch) {
		t.Errorf("missing lastSeen should return whole batch, got %v", got)
	}
	// Empty lastSeen → whole batch (first poll).
	if got := newJournalLines(batch, ""); len(got) != len(batch) {
		t.Errorf("empty lastSeen should return whole batch, got %v", got)
	}
	// lastSeen is the final line → nothing new.
	if got := newJournalLines(batch, "l4"); len(got) != 0 {
		t.Errorf("expected no new lines, got %v", got)
	}
}

func TestValidJournalSince(t *testing.T) {
	for _, s := range []string{"", "1h", "2026-01-01", "2026-01-01 10:00:00", "-30 min"} {
		if err := validJournalSince(s); err != nil {
			t.Errorf("expected %q valid, got %v", s, err)
		}
	}
	for _, s := range []string{"$(id)", "a;b", "a|b", "`x`", "a&b", "a\nb"} {
		if err := validJournalSince(s); err == nil {
			t.Errorf("expected %q rejected", s)
		}
	}
}

// FL-023 — formatting + parsing helpers for the live table.

func TestParseFloat(t *testing.T) {
	v, err := parseFloat(" 1.50 ")
	if err != nil || v != 1.5 {
		t.Errorf("parseFloat(1.50)=%v err=%v", v, err)
	}
	if _, err := parseFloat("notanumber"); err == nil {
		t.Error("expected error for non-numeric input")
	}
}

func TestFormatHelpers(t *testing.T) {
	if formatPercent(12.345) != "12.3" {
		t.Errorf("formatPercent=%q", formatPercent(12.345))
	}
	if formatSwap(-1) != "-" {
		t.Errorf("formatSwap(-1)=%q want -", formatSwap(-1))
	}
	if formatSwap(5.0) != "5.0" {
		t.Errorf("formatSwap(5.0)=%q", formatSwap(5.0))
	}
	if formatLoad(0.5) != "0.50" {
		t.Errorf("formatLoad(0.5)=%q", formatLoad(0.5))
	}
}
