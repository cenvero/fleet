// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/safetext"
	"github.com/cenvero/fleet/pkg/proto"
)

func (a *App) CollectMetrics(serverName string) (proto.MetricsSnapshot, error) {
	return a.collectMetrics(serverName, true)
}

// SampleMetrics collects, stores and alert-evaluates a live snapshot exactly
// like CollectMetrics but writes no audit entry. It is for high-frequency live
// views (`fleet top` refreshes every couple of seconds), which would otherwise
// append one audit record per server per frame.
func (a *App) SampleMetrics(serverName string) (proto.MetricsSnapshot, error) {
	return a.collectMetrics(serverName, false)
}

func (a *App) collectMetrics(serverName string, recordAudit bool) (proto.MetricsSnapshot, error) {
	return a.collectMetricsContext(context.Background(), serverName, recordAudit)
}

func (a *App) collectMetricsContext(ctx context.Context, serverName string, recordAudit bool) (proto.MetricsSnapshot, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return proto.MetricsSnapshot{}, err
	}

	// Raw call: this path saves the record itself below (with LastSeen), so
	// the generic last-seen refresh would only add a second write.
	response, err := a.callRPCContextRaw(ctx, server, proto.Envelope{
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
	snapshot = boundSnapshotText(snapshot)

	// Re-read the record: the call may have redialled and recorded a fresh
	// hello (agent version, capabilities), which saving the copy read before
	// the call would silently revert. A successful poll is also a sighting.
	if current, gerr := a.GetServer(serverName); gerr == nil {
		server = current
	}
	server.Metrics = snapshot
	server.Observed.Reachable = true
	server.Observed.LastSeen = time.Now().UTC()
	server.Observed.LastError = ""
	if err := a.SaveServer(server); err != nil {
		return proto.MetricsSnapshot{}, err
	}
	if err := a.persistMetricsSnapshot(serverName, snapshot); err != nil {
		return proto.MetricsSnapshot{}, err
	}
	if err := a.clearCollectionFailureAlert(serverName); err != nil {
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

// maxSnapshotText bounds each free-text field of an agent's metrics snapshot.
const maxSnapshotText = 1 << 10

// boundSnapshotText caps and neutralises the text an agent reports in a metrics
// snapshot. Every poll stores the snapshot in the server record and the metrics
// history, so an unbounded hostname or disk path from a hostile agent would
// grow the controller's database by that much every poll interval.
func boundSnapshotText(s proto.MetricsSnapshot) proto.MetricsSnapshot {
	s.Hostname = safetext.Terminal(safetext.Bound(s.Hostname, maxSnapshotText), false)
	s.DiskPath = safetext.Terminal(safetext.Bound(s.DiskPath, maxSnapshotText), false)
	return s
}

func (a *App) persistMetricsSnapshot(serverName string, snapshot proto.MetricsSnapshot) error {
	if a.MetricsDB == nil {
		return nil
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshal metrics snapshot: %w", err)
	}
	// One transaction for the latest-value row and the history row.
	return a.MetricsDB.RecordMetricSnapshot("latest."+serverName, serverName, snapshot.Timestamp, string(data))
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
		return a.observeAlert(alerts.Alert{
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
		return a.observeAlert(alerts.Alert{
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
	id := collectionFailureAlertID(serverName)
	// Fire the "offline" notification only on the transition into the failed
	// state (the alert did not already exist), so repeated polls while a server
	// stays down do not spam subscribers. Best-effort: never blocks the alert.
	if _, getErr := a.Alerts.Get(id); errors.Is(getErr, os.ErrNotExist) {
		a.fireNotify(NotifyEventOffline, fmt.Sprintf("%s is offline: metrics collection failed (%s)", serverName, err))
	}
	return a.observeAlert(alerts.Alert{
		ID:       id,
		Code:     "metrics.collect.failed",
		Server:   serverName,
		Severity: alerts.SeverityCritical,
		Message:  fmt.Sprintf("metrics collection failed for %s: %s", serverName, err),
	})
}

// clearCollectionFailureAlert removes a server's collection-failure ("offline")
// alert and, when one was actually present, fires the "online" notification —
// i.e. only on the recovery transition, not on every successful poll. The Delete
// itself is idempotent, so the worst case of a racing read is a missed (or, far
// less likely, duplicate) online notification, never a broken collection.
func (a *App) clearCollectionFailureAlert(serverName string) error {
	id := collectionFailureAlertID(serverName)
	_, getErr := a.Alerts.Get(id)
	wasDown := getErr == nil
	if err := a.Alerts.Delete(id); err != nil {
		return err
	}
	if wasDown {
		a.fireNotify(NotifyEventOnline, fmt.Sprintf("%s is back online: metrics collection recovered", serverName))
	}
	return nil
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
