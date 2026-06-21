// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

// TestResolveHostKeyPrompt covers the bootstrap host-key-changed handler
// selection. In `go test` stdin is not a terminal, so the interactive branch is
// exercised only for its non-tty fallback (nil = fail-closed).
func TestResolveHostKeyPrompt(t *testing.T) {
	cmd := &cobra.Command{}

	// --accept-new-host-key => a non-nil handler that always re-pins.
	fn := resolveHostKeyPrompt(cmd, true)
	if fn == nil {
		t.Fatal("autoAccept must return a non-nil handler")
	}
	if !fn("host", "OLD", "NEW") {
		t.Fatal("autoAccept handler must return true (re-pin)")
	}

	// No flag + non-interactive stdin => nil, preserving the fail-closed default
	// (a changed host key is refused, not silently re-pinned).
	if resolveHostKeyPrompt(cmd, false) != nil {
		t.Fatal("non-tty without --accept-new-host-key must return nil (fail-closed)")
	}
}
