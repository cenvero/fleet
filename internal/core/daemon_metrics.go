// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/logs"
)

func (a *App) metricsPollInterval() time.Duration {
	value := strings.TrimSpace(a.Config.Runtime.MetricsPollInterval)
	if value == "" {
		return 0
	}
	interval, err := time.ParseDuration(value)
	if err != nil || interval <= 0 {
		return 0
	}
	return interval
}

func (a *App) runMetricsPoller(ctx context.Context) {
	interval := a.metricsPollInterval()
	if interval <= 0 {
		return
	}

	a.collectMetricsCycle()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.collectMetricsCycle()
		}
	}
}

func (a *App) collectMetricsCycle() {
	servers, err := a.ListServers()
	if err != nil {
		a.appendPollAudit("metrics.poll.failed", "controller", err.Error())
		return
	}

	var failures []string
	for _, server := range servers {
		if _, err := a.collectMetrics(server.Name, false); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", server.Name, err))
		}
	}
	if len(failures) > 0 {
		a.appendPollAudit("metrics.poll.failed", "controller", strings.Join(failures, "; "))
	}
}

func (a *App) appendPollAudit(action, target, details string) {
	if a == nil || a.AuditLog == nil {
		return
	}
	_ = a.AuditLog.Append(logs.AuditEntry{
		Action:   action,
		Target:   target,
		Operator: a.operator(),
		Details:  details,
	})
}
