// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"strings"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// notifyDestructiveOperation fires the "destructive" notification event after a
// documented destructive command (core.IsDestructiveOperation: server remove,
// file rm, firewall enable, secret rotate, sync, ...) has completed
// successfully. Plain exec never fires it. The message names the command, its
// target server (if any) and the operator — never other arguments, which may
// carry values such as secrets. Best-effort: delivery errors are ignored.
func notifyDestructiveOperation(cmd *cobra.Command, configDir string) {
	top, sub := topLevelCommand(cmd)
	args := commandPositionalArgs(cmd)
	if !core.IsDestructiveOperation(top, sub, args) || !core.IsInitialized(configDir) {
		return
	}
	msg := "destructive operation: fleet " + strings.TrimSpace(top+" "+sub)
	if target := bestEffortTargetServer(top, args); target != "" {
		msg += " on " + target
	}
	msg += " by " + core.OperatorName(configDir, actingOperator)
	_ = core.NewNotifyStore(configDir).Fire(core.NotifyEventDestructive, msg)
}
