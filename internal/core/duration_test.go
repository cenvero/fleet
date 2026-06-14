// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"testing"
	"time"
)

func TestParseFlexDuration(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	ok := map[string]time.Duration{
		"7d":   7 * day,
		"30d":  30 * day,
		"2w":   14 * day,
		"12h":  12 * time.Hour,
		"90m":  90 * time.Minute,
		"10m":  10 * time.Minute,
		"7":    7 * day, // bare integer => days
		"0":    0,
		"1.5d": 36 * time.Hour,
		" 5d ": 5 * day,
		"10M":  10 * time.Minute, // case-insensitive
	}
	for in, want := range ok {
		got, err := ParseFlexDuration(in)
		if err != nil {
			t.Errorf("ParseFlexDuration(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseFlexDuration(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "-5", "-3d", "d", "7x", "  "} {
		if _, err := ParseFlexDuration(bad); err == nil {
			t.Errorf("ParseFlexDuration(%q) expected error, got nil", bad)
		}
	}
}

func TestValidateRetention(t *testing.T) {
	t.Parallel()
	for _, good := range []string{"7d", "30d", "12h", "0", "off", "never", "disabled", "NEVER"} {
		if err := ValidateRetention(good); err != nil {
			t.Errorf("ValidateRetention(%q) unexpected error: %v", good, err)
		}
	}
	for _, bad := range []string{"", "abc", "-1", "5x"} {
		if err := ValidateRetention(bad); err == nil {
			t.Errorf("ValidateRetention(%q) expected error", bad)
		}
	}
}

func TestConfigDurationHelpers(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour

	// Empty falls back to defaults.
	var c Config
	if got := c.JobLogRetentionDuration(); got != 7*day {
		t.Errorf("default job-log retention = %v, want 7d", got)
	}
	if got := c.SessionReconnectGraceDuration(); got != 10*time.Minute {
		t.Errorf("default session grace = %v, want 10m", got)
	}

	// Explicit values.
	c.Runtime.JobLogRetention = "30d"
	c.Runtime.SessionReconnectGrace = "5m"
	if got := c.JobLogRetentionDuration(); got != 30*day {
		t.Errorf("job-log retention = %v, want 30d", got)
	}
	if got := c.SessionReconnectGraceDuration(); got != 5*time.Minute {
		t.Errorf("session grace = %v, want 5m", got)
	}

	// Disabled sentinels => 0 (pruning off).
	for _, off := range []string{"0", "off", "never", "disabled"} {
		c.Runtime.JobLogRetention = off
		if got := c.JobLogRetentionDuration(); got != 0 {
			t.Errorf("retention %q => %v, want 0 (disabled)", off, got)
		}
	}

	// Garbage grace falls back to the 10m default rather than 0.
	c.Runtime.SessionReconnectGrace = "garbage"
	if got := c.SessionReconnectGraceDuration(); got != 10*time.Minute {
		t.Errorf("garbage grace = %v, want 10m fallback", got)
	}
}
