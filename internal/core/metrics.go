// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/pkg/proto"
)

func (a *App) CollectMetrics(serverName string) (proto.MetricsSnapshot, error) {
	return a.collectMetrics(serverName, true)
}

func (a *App) collectMetrics(serverName string, recordAudit bool) (proto.MetricsSnapshot, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return proto.MetricsSnapshot{}, err
	}

	response, err := a.callRPC(server, proto.Envelope{
		Action:  "metrics.collect",
		Payload: proto.MetricsPayload{Server: serverName},
	})
	if err != nil {
		_ = a.saveCollectionFailureAlert(serverName, err)
		return proto.MetricsSnapshot{}, err
	}
	if response.Error != nil {
		err := fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
		_ = a.saveCollectionFailureAlert(serverName, err)
		return proto.MetricsSnapshot{}, err
	}

	snapshot, err := proto.DecodePayload[proto.MetricsSnapshot](response.Payload)
	if err != nil {
		return proto.MetricsSnapshot{}, err
	}

	server.Metrics = snapshot
	if err := a.SaveServer(server); err != nil {
		return proto.MetricsSnapshot{}, err
	}
	if err := a.persistMetricsSnapshot(serverName, snapshot); err != nil {
		return proto.MetricsSnapshot{}, err
	}
	if err := a.Alerts.Delete(collectionFailureAlertID(serverName)); err != nil {
		return proto.MetricsSnapshot{}, err
	}
	if err := a.evaluateMetricAlerts(serverName, snapshot); err != nil {
		return proto.MetricsSnapshot{}, err
	}
	if recordAudit {
		if err := a.AuditLog.Append(logs.AuditEntry{
			Action:   "metrics.collect",
			Target:   serverName,
			Operator: a.operator(),
			Details:  fmt.Sprintf("cpu=%.1f memory=%.1f disk=%.1f", snapshot.CPUPercent, snapshot.MemoryPercent, snapshot.DiskPercent),
		}); err != nil {
			return proto.MetricsSnapshot{}, err
		}
	}
	return snapshot, nil
}

func (a *App) persistMetricsSnapshot(serverName string, snapshot proto.MetricsSnapshot) error {
	if a.MetricsDB == nil {
		return nil
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshal metrics snapshot: %w", err)
	}
	if err := a.MetricsDB.PutState("latest."+serverName, string(data)); err != nil {
		return err
	}
	return a.MetricsDB.AppendMetricSnapshot(serverName, snapshot.Timestamp, string(data))
}

func (a *App) evaluateMetricAlerts(serverName string, snapshot proto.MetricsSnapshot) error {
	if err := a.syncThresholdAlert(serverName, "cpu", snapshot.CPUPercent, 80, 90, "CPU"); err != nil {
		return err
	}
	if err := a.syncThresholdAlert(serverName, "memory", snapshot.MemoryPercent, 85, 95, "memory"); err != nil {
		return err
	}
	if snapshot.DiskPercent > 0 {
		if err := a.syncThresholdAlert(serverName, "disk", snapshot.DiskPercent, 85, 95, "disk"); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) syncThresholdAlert(serverName, metric string, value, warningThreshold, criticalThreshold float64, label string) error {
	warningID := metricAlertID(serverName, metric, alerts.SeverityWarning)
	criticalID := metricAlertID(serverName, metric, alerts.SeverityCritical)

	switch {
	case value >= criticalThreshold:
		if err := a.Alerts.Delete(warningID); err != nil {
			return err
		}
		return a.raiseAlert(alerts.Alert{
			ID:       criticalID,
			Code:     "metrics." + metric + ".critical",
			Server:   serverName,
			Severity: alerts.SeverityCritical,
			Message:  fmt.Sprintf("%s usage is %.1f%% on %s", label, value, serverName),
		})
	case value >= warningThreshold:
		if err := a.Alerts.Delete(criticalID); err != nil {
			return err
		}
		return a.raiseAlert(alerts.Alert{
			ID:       warningID,
			Code:     "metrics." + metric + ".warning",
			Server:   serverName,
			Severity: alerts.SeverityWarning,
			Message:  fmt.Sprintf("%s usage is %.1f%% on %s", label, value, serverName),
		})
	default:
		if err := a.Alerts.Delete(warningID); err != nil {
			return err
		}
		return a.Alerts.Delete(criticalID)
	}
}

func (a *App) saveCollectionFailureAlert(serverName string, err error) error {
	return a.raiseAlert(alerts.Alert{
		ID:       collectionFailureAlertID(serverName),
		Code:     "metrics.collect.failed",
		Server:   serverName,
		Severity: alerts.SeverityCritical,
		Message:  fmt.Sprintf("metrics collection failed for %s: %s", serverName, err),
	})
}

func metricAlertID(serverName, metric string, severity alerts.Severity) string {
	return "metrics-" + slugify(serverName) + "-" + slugify(metric) + "-" + string(severity)
}

func collectionFailureAlertID(serverName string) string {
	return "metrics-" + slugify(serverName) + "-collect-failed"
}

func slugify(input string) string {
	input = strings.ToLower(strings.TrimSpace(input))
	var out strings.Builder
	lastDash := false
	for _, r := range input {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				out.WriteByte('-')
				lastDash = true
			}
		}
	}
	value := strings.Trim(out.String(), "-")
	if value == "" {
		return "unknown"
	}
	return value
}
