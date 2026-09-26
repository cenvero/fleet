// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"path/filepath"

	"github.com/cenvero/fleet/internal/logs"
)

// Audit appends one entry to the controller audit log, attributed like every
// other App action (verified token, configured operator, or OS user). Callers
// must pass details that are already free of secret values.
func (a *App) Audit(action, target, details string) error {
	if a == nil || a.AuditLog == nil {
		return nil
	}
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   action,
		Target:   target,
		Operator: a.operator(),
		Details:  details,
	})
}

// AuditEvent appends one entry to the audit log of the controller at configDir,
// for CLI paths that change local stores (secrets, tokens, policies, approvals)
// without opening an App. operator is usually OperatorName(configDir, acting).
// Callers must pass details that are already free of secret values.
func AuditEvent(configDir, operator, action, target, details string) error {
	return logs.NewAuditLog(filepath.Join(configDir, "logs", "_audit.log")).Append(logs.AuditEntry{
		Action:   action,
		Target:   target,
		Operator: operator,
		Details:  details,
	})
}
