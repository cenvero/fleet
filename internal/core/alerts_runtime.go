// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/logs"
)

func (a *App) raiseAlert(alert alerts.Alert) error {
	existing, err := a.Alerts.Get(alert.ID)
	created := false
	escalated := false
	notify := false
	switch {
	case err == nil:
		if alert.CreatedAt.IsZero() {
			alert.CreatedAt = existing.CreatedAt
		}
		alert.UpdatedAt = time.Now().UTC()
		alert.Occurrences = existing.Occurrences + 1
		alert.NotifyCount = existing.NotifyCount
		alert.LastNotifiedAt = existing.LastNotifiedAt
		alert.SuppressedUntil = existing.SuppressedUntil
		escalated = severityRank(alert.Severity) > severityRank(existing.Severity)
		if escalated || alert.Message != existing.Message {
			alert.AcknowledgedAt = nil
		} else if alert.AcknowledgedAt == nil {
			alert.AcknowledgedAt = existing.AcknowledgedAt
		}
		notify = a.shouldNotifyAlert(alert, existing, false, escalated)
	case errors.Is(err, os.ErrNotExist):
		created = true
		alert.CreatedAt = time.Now().UTC()
		alert.UpdatedAt = alert.CreatedAt
		alert.Occurrences = 1
		notify = a.shouldNotifyAlert(alert, alerts.Alert{}, true, false)
	case err != nil:
		return err
	}

	if err := a.Alerts.Save(alert); err != nil {
		return err
	}
	if created || escalated || notify {
		a.notifyAlert(alert)
	}
	return nil
}

func (a *App) notifyAlert(alert alerts.Alert) {
	if a == nil || a.Notifier == nil {
		return
	}
	if alert.Severity != alerts.SeverityCritical {
		return
	}
	if alert.SuppressedUntil != nil && alert.SuppressedUntil.After(time.Now().UTC()) {
		return
	}

	title := "Cenvero Fleet critical alert"
	if strings.TrimSpace(alert.Server) != "" {
		title = fmt.Sprintf("Cenvero Fleet: %s", alert.Server)
	}
	if err := a.Notifier.Notify(title, alert.Message); err != nil && a.AuditLog != nil {
		_ = a.AuditLog.Append(logs.AuditEntry{
			Action:   "alert.notify.failed",
			Target:   alert.ID,
			Operator: a.operator(),
			Details:  err.Error(),
		})
		return
	}

	now := time.Now().UTC()
	alert.LastNotifiedAt = &now
	alert.NotifyCount++
	alert.UpdatedAt = now
	_ = a.Alerts.Save(alert)
}

func (a *App) shouldNotifyAlert(alert, existing alerts.Alert, created, escalated bool) bool {
	if alert.Severity != alerts.SeverityCritical {
		return false
	}
	now := time.Now().UTC()
	if alert.SuppressedUntil != nil && alert.SuppressedUntil.After(now) {
		return false
	}
	if created || escalated {
		return true
	}
	if alert.AcknowledgedAt != nil {
		return false
	}
	if existing.LastNotifiedAt == nil {
		return true
	}
	cooldown, err := a.alertNotifyCooldown()
	if err != nil || cooldown <= 0 {
		return false
	}
	return existing.LastNotifiedAt.Add(cooldown).Before(now)
}

func (a *App) alertNotifyCooldown() (time.Duration, error) {
	if a == nil || strings.TrimSpace(a.Config.Runtime.AlertNotifyCooldown) == "" {
		return 0, nil
	}
	return time.ParseDuration(a.Config.Runtime.AlertNotifyCooldown)
}

func severityRank(severity alerts.Severity) int {
	switch severity {
	case alerts.SeverityCritical:
		return 3
	case alerts.SeverityWarning:
		return 2
	case alerts.SeverityInfo:
		return 1
	default:
		return 0
	}
}
