// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package agent

import (
	"strings"
	"testing"
	"time"
)

func TestExtractSessionGrace(t *testing.T) {
	t.Parallel()

	// Present: parsed to a duration AND stripped from the shell env.
	grace, cleaned := extractSessionGrace([]string{"FOO=bar", "FLEET_SESSION_GRACE=600", "BAZ=qux"})
	if grace != 600*time.Second {
		t.Fatalf("grace = %v, want 600s", grace)
	}
	if len(cleaned) != 2 {
		t.Fatalf("cleaned env len = %d, want 2", len(cleaned))
	}
	for _, kv := range cleaned {
		if strings.HasPrefix(kv, sessionGraceEnvVar+"=") {
			t.Fatalf("%s leaked into the shell env: %v", sessionGraceEnvVar, cleaned)
		}
	}

	// Absent: zero grace, env unchanged.
	if g, c := extractSessionGrace([]string{"A=1"}); g != 0 || len(c) != 1 {
		t.Fatalf("absent => grace=%v len=%d, want 0/1", g, len(c))
	}

	// Invalid / non-positive values yield zero grace (caller uses the default).
	for _, bad := range []string{"FLEET_SESSION_GRACE=0", "FLEET_SESSION_GRACE=abc", "FLEET_SESSION_GRACE=-5"} {
		if g, _ := extractSessionGrace([]string{bad}); g != 0 {
			t.Fatalf("%q => grace %v, want 0", bad, g)
		}
	}
}

func TestSessionIdleGrace(t *testing.T) {
	t.Parallel()
	s := &persistentSession{}
	if got := s.idleGrace(); got != sessionIdleTimeout {
		t.Fatalf("default idleGrace = %v, want %v", got, sessionIdleTimeout)
	}
	s.grace = 5 * time.Minute
	if got := s.idleGrace(); got != 5*time.Minute {
		t.Fatalf("custom idleGrace = %v, want 5m", got)
	}
}
