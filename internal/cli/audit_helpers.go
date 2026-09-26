// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"

	"github.com/cenvero/fleet/internal/core"
)

// auditLocal records a change to a local controller store (secrets, tokens,
// policies, approvals) in the audit log, attributed to the verified token or
// the configured/OS operator. Best-effort, like the other audit call sites;
// details must never carry secret values.
func auditLocal(configDir, action, target, details string) {
	_ = core.AuditEvent(configDir, core.OperatorName(configDir, actingOperator), action, target, details)
}

// redactForAudit applies the configured output-redaction policy to text bound
// for the audit log (e.g. a staged command). If the policy cannot be loaded the
// text is withheld rather than written unredacted.
func redactForAudit(configDir, text string) string {
	store, err := core.NewRedactStore(configDir)
	if err != nil || store == nil {
		return "[withheld: redaction policy unavailable]"
	}
	return store.Redact(text)
}

// execAuditDetails renders an exec outcome for the audit log: the command as
// displayed (secret references, never values; already redacted by the caller)
// and the exit code, timeout or transport error.
func execAuditDetails(displayCommand string, j execJSON) string {
	switch {
	case j.TimedOut:
		return fmt.Sprintf("command=%q timed_out=true", displayCommand)
	case j.AgentError != "":
		return fmt.Sprintf("command=%q error=%q", displayCommand, j.AgentError)
	default:
		return fmt.Sprintf("command=%q exit_code=%d", displayCommand, j.ExitCode)
	}
}
