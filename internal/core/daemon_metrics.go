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

const (
	// metricsPollParallelism bounds how many servers one poll cycle collects
	// from at once. Collections used to run one server at a time, so N slow
	// servers stretched every cycle to N× their latency.
	metricsPollParallelism = 16

	// metricSnapshotRetention is how long metric_snapshots history is kept; the
	// table otherwise grows forever (~720k rows/day at 500 servers polled every
	// minute). The latest value per server lives in metrics_state and is never
	// pruned.
	metricSnapshotRetention = 30 * 24 * time.Hour
	// metricRetentionInterval is how often the daemon prunes old history.
	metricRetentionInterval = time.Hour
	// metricRetentionBudget bounds one prune run; a large backlog is finished
	// by later runs.
	metricRetentionBudget = 10 * time.Minute
)

// metricsCollectTimeout bounds one server's collection within a poll cycle. The
// RPC used to run on context.Background() with no deadline, so a single hung
// agent stalled the poller forever. (A variable only so tests can shorten it.)
var metricsCollectTimeout = 20 * time.Second

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

// runMetricsPoller collects metrics from every server once per interval while
// the daemon runs, and keeps the stored history within its retention window.
// Cycles never overlap: the next one starts only after the previous returned
// (ticks that fire meanwhile are dropped by the ticker).
func (a *App) runMetricsPoller(ctx context.Context) {
	go a.runMetricsRetention(ctx)

	interval := a.metricsPollInterval()
	if interval <= 0 {
		return
	}

	a.collectMetricsCycleContext(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.collectMetricsCycleContext(ctx)
		}
	}
}

func (a *App) collectMetricsCycle() {
	a.collectMetricsCycleContext(context.Background())
}

// collectMetricsCycleContext collects from every server with bounded
// parallelism and a per-server deadline, then records one audit entry listing
// the failures in server order.
func (a *App) collectMetricsCycleContext(ctx context.Context) {
	servers, err := a.ListServers()
	if err != nil {
		a.appendPollAudit("metrics.poll.failed", "controller", err.Error())
		return
	}

	failures := make([]string, len(servers))
	ForEachLimit(len(servers), metricsPollParallelism, func(i int) {
		if ctx.Err() != nil {
			return // daemon stopping: skip servers not started yet
		}
		serverCtx, cancel := context.WithTimeout(ctx, metricsCollectTimeout)
		defer cancel()
		if _, err := a.collectMetricsContext(serverCtx, servers[i].Name, false); err != nil {
			failures[i] = fmt.Sprintf("%s: %v", servers[i].Name, err)
		}
	})
	var failed []string
	for _, f := range failures {
		if f != "" {
			failed = append(failed, f)
		}
	}
	if len(failed) > 0 {
		a.appendPollAudit("metrics.poll.failed", "controller", strings.Join(failed, "; "))
	}
}

// runMetricsRetention prunes metric_snapshots rows older than
// metricSnapshotRetention at daemon start and then hourly.
func (a *App) runMetricsRetention(ctx context.Context) {
	a.pruneMetricHistory(ctx)
	ticker := time.NewTicker(metricRetentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.pruneMetricHistory(ctx)
		}
	}
}

func (a *App) pruneMetricHistory(ctx context.Context) {
	if a == nil || a.MetricsDB == nil {
		return
	}
	pruneCtx, cancel := context.WithTimeout(ctx, metricRetentionBudget)
	defer cancel()
	removed, err := a.MetricsDB.PruneMetricSnapshots(pruneCtx, time.Now().Add(-metricSnapshotRetention))
	if removed > 0 {
		a.appendPollAudit("metrics.retention.pruned", "controller",
			fmt.Sprintf("removed %d metric snapshot(s) older than %s", removed, metricSnapshotRetention))
	}
	if err != nil && ctx.Err() == nil {
		a.appendPollAudit("metrics.retention.failed", "controller", err.Error())
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
