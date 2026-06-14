// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Default retention/grace values used when the config field is empty.
const (
	DefaultJobLogRetention       = "7d"
	DefaultSessionReconnectGrace = "10m"
)

// ParseFlexDuration parses a human-friendly retention/grace duration. In
// addition to Go's units (h, m, s) it accepts a 'd' (days) or 'w' (weeks)
// suffix, and a bare integer is interpreted as DAYS (so "7" == "7d"). The value
// must be non-negative. Examples: "7d", "30d", "2w", "12h", "90m", "10m".
//
// It exists because operators think in days for log retention, but Go's
// time.ParseDuration tops out at hours and rejects "d".
func ParseFlexDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	// A bare integer means days.
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("duration must be non-negative: %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	mul := time.Duration(0)
	switch {
	case strings.HasSuffix(s, "d"):
		mul = 24 * time.Hour
		s = strings.TrimSuffix(s, "d")
	case strings.HasSuffix(s, "w"):
		mul = 7 * 24 * time.Hour
		s = strings.TrimSuffix(s, "w")
	}
	if mul != 0 {
		n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n * float64(mul)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use forms like 7d, 12h, 10m)", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration must be non-negative: %q", s)
	}
	return d, nil
}

// ValidateRetention reports whether v is a valid job-log-retention value: a
// disabled sentinel ("0"/"off"/"never"/"disabled") or any flexible duration
// ParseFlexDuration accepts.
func ValidateRetention(v string) error {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "off", "never", "disabled":
		return nil
	}
	_, err := ParseFlexDuration(v)
	return err
}

// JobLogRetentionDuration returns the configured job-log retention as a duration,
// falling back to the default when unset and to 0 (pruning disabled) when the
// value is an explicit "0"/"off"/"never" or unparseable.
func (c Config) JobLogRetentionDuration() time.Duration {
	raw := strings.TrimSpace(c.Runtime.JobLogRetention)
	if raw == "" {
		raw = DefaultJobLogRetention
	}
	switch strings.ToLower(raw) {
	case "0", "off", "never", "disabled":
		return 0
	}
	d, err := ParseFlexDuration(raw)
	if err != nil {
		return 0
	}
	return d
}

// SessionReconnectGraceDuration returns the configured reconnect grace as a
// duration, falling back to the 10-minute default when unset or unparseable.
func (c Config) SessionReconnectGraceDuration() time.Duration {
	raw := strings.TrimSpace(c.Runtime.SessionReconnectGrace)
	if raw == "" {
		raw = DefaultSessionReconnectGrace
	}
	d, err := ParseFlexDuration(raw)
	if err != nil {
		d, _ = ParseFlexDuration(DefaultSessionReconnectGrace)
	}
	return d
}
